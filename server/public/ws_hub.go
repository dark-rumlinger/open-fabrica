package public

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"openfabrica/internal/safego"
)

// wsClientBufferSize is the outbound channel depth per client. Sized for realtime games:
// 4096 absorbs short stalls (GC, slow consumer) at 30Hz with several peers without dropping.
const wsClientBufferSize = 4096

// wsHub manages WebSocket rooms. Each room key is built via events.RoomKey
// ("projectID:tenantID:endpointPath:room"), ensuring full project AND tenant isolation.
type wsHub struct {
	mu    sync.RWMutex
	rooms map[string]*wsRoom
	// matchCaps caps each minted matchmaking room at its group size, keyed by the match
	// room's full key. A matchmaking endpoint runs uncapped (group_size, not max_clients,
	// governs it), so without this anyone who learns a match id joins the match as a
	// player. Entries expire after matchCapTTL. Guarded by mu.
	matchCaps map[string]matchCap
	// actors holds the authoritative room actors, guarded by actorsMu (separate from mu:
	// actor lifecycle is reference-counted independently of relay-room membership).
	actorsMu sync.Mutex
	actors   map[string]*roomActor
}

// matchCap is one minted match room's participant cap and when it was minted.
type matchCap struct {
	size    int
	created time.Time
}

// matchCapTTL bounds how long a minted match room keeps its cap entry. Matches are
// short-lived; the bound only keeps the map from growing when matched peers never arrive.
const matchCapTTL = 6 * time.Hour

// roomCap is the participant cap for roomKey: a minted match room's group size, else the
// endpoint's max_clients. The caller holds h.mu.
func (h *wsHub) roomCap(roomKey string, maxClients int) int {
	if mc, ok := h.matchCaps[roomKey]; ok {
		return mc.size
	}
	return maxClients
}

// wsRoom holds the clients in a room plus optional per-room tick state. latestStates is
// populated only when tick_hz > 0: the read pump calls setLatestState() per inbound message
// and a per-room ticker emits one snapshot per tick collapsing every client's latest state.
type wsRoom struct {
	clients      map[string]*wsClient
	latestStates map[string]json.RawMessage
	tickCancel   context.CancelFunc
	// matchCancel stops the per-lobby matchmaking re-evaluation goroutine (see
	// ensureMatchTicker). Set only on a criteria-matchmaking lobby (always tick_hz=0, so it
	// never coexists with tickCancel); cancelled when the lobby empties.
	matchCancel context.CancelFunc
}

// wsMessage carries a message with its frame type (text or binary).
type wsMessage struct {
	data   []byte
	binary bool // true = binary frame, false = text frame
}

// wsClient represents a single WebSocket connection in a room.
type wsClient struct {
	id string
	ch chan wsMessage // buffered outbound messages
	// meta is the client's presence profile (name/avatar), supplied at connect via
	// ?meta=<json> and carried in the welcome roster (_peers) and join broadcast (_meta) so
	// peers learn who's present from the server. Raw JSON; nil when none was supplied.
	meta json.RawMessage
	// matched is set by formMatch once this client is placed into a matchmaking group, so a
	// concurrent formMatch in the same lobby never pairs it twice before it switches into its
	// match room. Guarded by the hub mutex.
	matched bool
	// superseded is closed when another connection takes this client's id over. One identity
	// owns one live connection: the loser's write pump returns on this, closing its socket.
	// Closed exactly once (the takeover also removes this client from the room, so no later
	// join finds it); never closed for a client still in the room.
	superseded chan struct{}
	// spectator is set at join when the room was already at its max_clients cap: the
	// connection still receives broadcasts/snapshots but its inbound frames are dropped.
	spectator bool
	// joinedAt is when this connection joined its current room. Used by criteria matchmaking
	// to widen a waiting peer's rating band with time, so nobody waits forever.
	joinedAt time.Time
}

// newWSClient is the only way to build a client, so every client carries its channels: a
// hand-built one losing a takeover would close a nil channel and panic the hub.
func newWSClient(id string, meta json.RawMessage) *wsClient {
	return &wsClient{
		id:         id,
		ch:         make(chan wsMessage, wsClientBufferSize),
		meta:       meta,
		joinedAt:   time.Now(),
		superseded: make(chan struct{}),
	}
}

// NewWSHub creates a new WebSocket hub for managing rooms.
func NewWSHub() *wsHub {
	return &wsHub{
		rooms:     make(map[string]*wsRoom),
		actors:    make(map[string]*roomActor),
		matchCaps: make(map[string]matchCap),
	}
}

// join adds a client to a room, broadcasts a JOIN to other members, and returns the client
// handle. roomName is the user-facing room id (no projectID:endpointPath prefix) shown in the
// _room field. extraWelcome, when non-nil, is merged into this joiner's welcome frame (e.g. a
// minted _joinToken); reserved _-prefixed system keys always win a collision.
func (h *wsHub) join(roomKey, roomName, clientID string, maxClients int, meta json.RawMessage, extraWelcome map[string]any) *wsClient {
	c := newWSClient(clientID, meta)
	h.mu.Lock()
	maxClients = h.roomCap(roomKey, maxClients)
	room, ok := h.rooms[roomKey]
	if !ok {
		room = &wsRoom{clients: make(map[string]*wsClient)}
		h.rooms[roomKey] = room
	}
	// A connection arriving with an id already in the room is a reconnect/takeover (same
	// stable cid): it REPLACES the old entry, keeps its role, and is NOT re-announced (the old
	// connection's leave is a supersession-safe no-op), so the player keeps its slot, no ghost.
	old, isReconnect := room.clients[clientID]
	// Snapshot the roster BEFORE adding the joiner so welcome reflects who was already present.
	// _peers carries each peer's presence meta so a joiner sees names/avatars with zero round
	// trips. Exclude self (a reconnect would list its old entry); playerCount drives the gate.
	existingPeers := make([]map[string]any, 0, len(room.clients))
	playerCount := 0
	for id, ec := range room.clients {
		if id == clientID {
			continue
		}
		existingPeers = append(existingPeers, map[string]any{"id": id, "meta": ec.meta})
		if !ec.spectator {
			playerCount++
		}
	}
	// Capacity gate: past max_clients participants, admit joiners as spectators (decided under
	// the lock so a join race can't overfill). A reconnect keeps its existing role.
	if isReconnect {
		c.spectator = old.spectator
		c.matched = old.matched // a matched peer re-dialing its match room must not be paired again
	} else {
		c.spectator = maxClients > 0 && playerCount >= maxClients
	}
	room.clients[clientID] = c
	clientCount := len(room.clients)
	if isReconnect {
		// The old connection lost the slot: close it so the identity keeps exactly one live
		// socket. Its leave is already a no-op (leave compares by pointer), unwinding the loser.
		close(old.superseded)
	}
	h.mu.Unlock()

	role := "player"
	if c.spectator {
		role = "spectator"
	}
	welcome := map[string]any{
		"_type":     "welcome",
		"_clientId": clientID,
		"_room":     roomName,
		"_clients":  clientCount,
		"_peers":    existingPeers,
		"_role":     role,
	}
	if maxClients > 0 {
		welcome["_capacity"] = maxClients
	}
	for k, v := range extraWelcome {
		if _, reserved := welcome[k]; !reserved {
			welcome[k] = v
		}
	}
	welcomeMsg, _ := json.Marshal(welcome)
	select {
	case c.ch <- wsMessage{data: welcomeMsg}:
	default:
		slog.Warn("ws: welcome message dropped (channel full)", "room", roomKey, "client", clientID)
	}

	// Announce only a genuinely new arrival. A reconnect's id was never removed
	// from peers' rosters, so re-broadcasting a join would be a phantom double-add.
	if !isReconnect {
		joinFields := map[string]any{
			"_type":    "join",
			"_sender":  clientID,
			"_room":    roomName,
			"_clients": clientCount,
			"_role":    role,
		}
		if meta != nil {
			joinFields["_meta"] = meta
		}
		joinMsg, _ := json.Marshal(joinFields)
		h.broadcast(roomKey, clientID, joinMsg, false)
	}

	return c
}

// leave removes a client from a room and reports (empty, owner): whether the room became
// empty, and whether THIS connection still owned the id. An empty room is torn down; the
// caller uses `empty` for end-of-session cleanup and `owner` to gate the actor's @leave.
// Supersession-safe: it acts only when room.clients[id] is still THIS *wsClient, so a
// reconnect that replaced the entry makes the old connection's later leave a no-op (owner=false).
func (h *wsHub) leave(roomKey, roomName string, client *wsClient) (empty bool, owner bool) {
	clientID := client.id
	h.mu.Lock()
	room, ok := h.rooms[roomKey]
	if !ok {
		h.mu.Unlock()
		return false, false
	}
	if room.clients[clientID] != client {
		h.mu.Unlock()
		return false, false // superseded by a reconnect; the new owner keeps the slot
	}
	delete(room.clients, clientID)
	if room.latestStates != nil {
		delete(room.latestStates, clientID)
	}
	clientCount := len(room.clients)
	empty = clientCount == 0
	var cancelTick, cancelMatch context.CancelFunc
	if empty {
		cancelTick = room.tickCancel
		cancelMatch = room.matchCancel
		delete(h.rooms, roomKey)
	}
	h.mu.Unlock()
	if cancelTick != nil {
		cancelTick()
	}
	if cancelMatch != nil {
		cancelMatch()
	}

	if !empty {
		leaveMsg, _ := json.Marshal(map[string]any{
			"_type":    "leave",
			"_sender":  clientID,
			"_room":    roomName,
			"_clients": clientCount,
		})
		h.broadcast(roomKey, clientID, leaveMsg, false)
	}
	return empty, true
}

// isMatchRoom reports whether roomKey is a room formMatch minted. Pairing never runs
// there: peers reconnecting into their match would otherwise be re-matched into yet
// another room.
func (h *wsHub) isMatchRoom(roomKey string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.matchCaps[roomKey]
	return ok
}

// isSpectator reads a client's CURRENT role under the hub lock; moveClient can demote a
// player mid-connection.
func (h *wsHub) isSpectator(c *wsClient) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return c.spectator
}

// moveClient relocates an existing client (the SAME *wsClient and its outbound channel)
// between rooms for an in-band room switch. Reusing the object is the point: the write pump
// drains client.ch, so a new client would silently stop delivering the new room's traffic.
// Mirrors leave+join and reports whether the old room emptied. Supersession-safe: a no-op if
// this connection no longer owns its id in the old room.
func (h *wsHub) moveClient(c *wsClient, fromKey, fromName, toKey, toName string, maxClients int) (oldEmpty bool, ok bool) {
	h.mu.Lock()
	fromRoom := h.rooms[fromKey]
	if fromRoom == nil || fromRoom.clients[c.id] != c {
		h.mu.Unlock()
		return false, false
	}
	delete(fromRoom.clients, c.id)
	if fromRoom.latestStates != nil {
		delete(fromRoom.latestStates, c.id)
	}
	fromCount := len(fromRoom.clients)
	oldEmpty = fromCount == 0
	var cancelTick, cancelMatch context.CancelFunc
	if oldEmpty {
		cancelTick = fromRoom.tickCancel
		cancelMatch = fromRoom.matchCancel
		delete(h.rooms, fromKey)
	}

	maxClients = h.roomCap(toKey, maxClients)
	toRoom, ok2 := h.rooms[toKey]
	if !ok2 {
		toRoom = &wsRoom{clients: make(map[string]*wsClient)}
		h.rooms[toKey] = toRoom
	}
	existingPeers := make([]map[string]any, 0, len(toRoom.clients))
	destPlayers := 0
	for id, ec := range toRoom.clients {
		if id == c.id {
			continue
		}
		existingPeers = append(existingPeers, map[string]any{"id": id, "meta": ec.meta})
		if !ec.spectator {
			destPlayers++
		}
	}
	// Capacity gate on the destination too: a participant switching into a full room is
	// demoted to spectator, so an in-band switch can't overfill. (Spectators stay spectators.)
	if maxClients > 0 && !c.spectator && destPlayers >= maxClients {
		c.spectator = true
	}
	toRoom.clients[c.id] = c
	toCount := len(toRoom.clients)
	h.mu.Unlock()
	if cancelTick != nil {
		cancelTick()
	}
	if cancelMatch != nil {
		cancelMatch()
	}

	if !oldEmpty {
		leaveMsg, _ := json.Marshal(map[string]any{
			"_type": "leave", "_sender": c.id, "_room": fromName, "_clients": fromCount,
		})
		h.broadcast(fromKey, c.id, leaveMsg, false)
	}

	role := "player"
	if c.spectator {
		role = "spectator"
	}
	welcome := map[string]any{
		"_type": "welcome", "_clientId": c.id, "_room": toName,
		"_clients": toCount, "_peers": existingPeers, "_role": role,
	}
	// Same _capacity the join-path welcome carries. Every matchmaking handoff
	// arrives through this path, so omitting it left an "R/N players" UI with no N
	// from the moment the match started.
	if maxClients > 0 {
		welcome["_capacity"] = maxClients
	}
	welcomeMsg, _ := json.Marshal(welcome)
	select {
	case c.ch <- wsMessage{data: welcomeMsg}:
	default:
		slog.Warn("ws: welcome message dropped on room move (channel full)", "room", toKey, "client", c.id)
	}

	joinFields := map[string]any{
		"_type": "join", "_sender": c.id, "_room": toName, "_clients": toCount, "_role": role,
	}
	if c.meta != nil {
		joinFields["_meta"] = c.meta
	}
	joinMsg, _ := json.Marshal(joinFields)
	h.broadcast(toKey, c.id, joinMsg, false)
	return oldEmpty, true
}

// broadcast sends msg to all clients in the room except the sender.
func (h *wsHub) broadcast(roomKey, senderID string, msg []byte, binary bool) {
	h.mu.RLock()
	room := h.rooms[roomKey]
	if room == nil {
		h.mu.RUnlock()
		return
	}
	targets := make([]*wsClient, 0, len(room.clients))
	for _, c := range room.clients {
		if c.id != senderID {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	m := wsMessage{data: msg, binary: binary}
	for _, c := range targets {
		select {
		case c.ch <- m:
		default:
			slog.Warn("ws: message dropped (channel full)", "room", roomKey, "client", c.id)
			h.sendError(c, "buffer full, message dropped")
		}
	}
}

// broadcastAll sends msg to ALL clients in the room (including sender).
func (h *wsHub) broadcastAll(roomKey string, msg []byte, binary bool) {
	h.mu.RLock()
	room := h.rooms[roomKey]
	if room == nil {
		h.mu.RUnlock()
		return
	}
	targets := make([]*wsClient, 0, len(room.clients))
	for _, c := range room.clients {
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	m := wsMessage{data: msg, binary: binary}
	for _, c := range targets {
		select {
		case c.ch <- m:
		default:
			// Don't warn for system messages on full buffers.
		}
	}
}

// sendTo sends msg to a specific client in a room. Returns false if client not found.
func (h *wsHub) sendTo(roomKey, targetID string, msg []byte, binary bool) bool {
	h.mu.RLock()
	room := h.rooms[roomKey]
	if room == nil {
		h.mu.RUnlock()
		return false
	}
	target := room.clients[targetID]
	h.mu.RUnlock()

	if target == nil {
		return false
	}
	select {
	case target.ch <- wsMessage{data: msg, binary: binary}:
		return true
	default:
		slog.Warn("ws: direct message dropped (channel full)", "room", roomKey, "target", targetID)
		return false
	}
}

// sendError sends a system error message to a single client.
func (h *wsHub) sendError(c *wsClient, msg string) {
	errMsg, _ := json.Marshal(map[string]any{
		"_type":    "error",
		"_message": msg,
	})
	select {
	case c.ch <- wsMessage{data: errMsg}:
	default:
		// Can't send error if buffer is full, nothing more to do.
	}
}

// roomClients returns the list of client IDs in a room.
func (h *wsHub) roomClients(roomKey string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	room := h.rooms[roomKey]
	if room == nil {
		return nil
	}
	ids := make([]string, 0, len(room.clients))
	for id := range room.clients {
		ids = append(ids, id)
	}
	return ids
}

// matchCriteria tunes formMatch. The zero value (MatchBy == "") reproduces the plain
// "claim any groupSize unmatched peers" behavior.
type matchCriteria struct {
	GroupSize       int
	MatchBy         string  // "" | "rating" | "region" | "rating+region"
	RatingBand      int     // initial rating half-width for rating matching
	BandWidenPerSec float64 // how fast the band grows with the anchor peer's wait
	MaxWaitSecs     int     // past this the anchor matches any rating (region still honored); 0 = never widen to infinite
}

func matchByRating(m string) bool { return m == "rating" || m == "rating+region" }
func matchByRegion(m string) bool { return m == "region" || m == "rating+region" }

// clientMatchInfo is a waiting peer's matchmaking attributes, parsed from its
// presence meta plus how long it has waited.
type clientMatchInfo struct {
	id     string
	rating float64
	region string
	waited float64 // seconds
}

// parseMatchMeta pulls {rating, region} out of a client's ?meta= presence
// profile. Missing/invalid fields read as 0 / "" (i.e. unrated / no region).
func parseMatchMeta(raw json.RawMessage) (rating float64, region string) {
	if len(raw) == 0 {
		return 0, ""
	}
	var m struct {
		Rating float64 `json:"rating"`
		Region string  `json:"region"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Rating, m.Region
}

// formMatch is the server-side matchmaker, called when a peer joins a matchmaking lobby.
// When enough compatible unmatched peers wait it claims groupSize of them (marking each
// matched so a concurrent call can't re-pair them), mints a shared match-room name, and
// returns the claimed ids; ok=false when too few compatible peers wait.
//
// With criteria (MatchBy set) it anchors on the LONGEST-WAITING peer, whose rating band
// widens with its wait so it is guaranteed a match once the band (or MaxWaitSecs) opens far
// enough. That guarantee needs re-evaluation over time even with no new connect, so
// ensureMatchTicker runs this on a per-lobby timer. Region, when required, is always honored.
func (h *wsHub) formMatch(roomKey string, c matchCriteria) (group []string, matchRoom string, ok bool) {
	groupSize := c.GroupSize
	if groupSize < 2 {
		groupSize = 2
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.rooms[roomKey]
	if room == nil {
		return nil, "", false
	}

	// No criteria: original "first groupSize unmatched" behavior.
	if !matchByRating(c.MatchBy) && !matchByRegion(c.MatchBy) {
		for id, cl := range room.clients {
			if cl.matched {
				continue
			}
			group = append(group, id)
			if len(group) == groupSize {
				break
			}
		}
		if len(group) < groupSize {
			return nil, "", false
		}
		return h.claimGroup(roomKey, room, group)
	}

	// Criteria-based. Collect unmatched peers with their attributes.
	now := time.Now()
	waiting := make([]clientMatchInfo, 0, len(room.clients))
	for id, cl := range room.clients {
		if cl.matched {
			continue
		}
		rating, region := parseMatchMeta(cl.meta)
		waiting = append(waiting, clientMatchInfo{id: id, rating: rating, region: region, waited: now.Sub(cl.joinedAt).Seconds()})
	}
	if len(waiting) < groupSize {
		return nil, "", false
	}

	// Anchor on the longest-waiting peer (its band is the widest), then gather the
	// closest-rated, region-compatible peers within that band.
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].waited > waiting[j].waited })
	anchor := waiting[0]

	band := math.Inf(1)
	if matchByRating(c.MatchBy) {
		if !(c.MaxWaitSecs > 0 && anchor.waited >= float64(c.MaxWaitSecs)) {
			band = float64(c.RatingBand) + c.BandWidenPerSec*anchor.waited
		}
	}

	rest := waiting[1:]
	sort.Slice(rest, func(i, j int) bool {
		return math.Abs(rest[i].rating-anchor.rating) < math.Abs(rest[j].rating-anchor.rating)
	})
	group = []string{anchor.id}
	for _, w := range rest {
		if matchByRegion(c.MatchBy) && w.region != anchor.region {
			continue
		}
		if matchByRating(c.MatchBy) && math.Abs(w.rating-anchor.rating) > band {
			continue
		}
		group = append(group, w.id)
		if len(group) == groupSize {
			break
		}
	}
	if len(group) < groupSize {
		return nil, "", false
	}
	return h.claimGroup(roomKey, room, group)
}

// claimGroup marks each id matched and mints the shared match-room name. The
// caller holds h.mu.
func (h *wsHub) claimGroup(lobbyKey string, room *wsRoom, group []string) ([]string, string, bool) {
	for _, id := range group {
		if cl := room.clients[id]; cl != nil {
			cl.matched = true
		}
	}
	// Random, not sequential: a counter is guessable and restarts at 1 with the process,
	// so a third party could walk into m-3, and a match routed to an authoritative
	// endpoint would resume a previous game's persisted state under a reused id.
	name := "m-" + randomMatchID()
	if h.matchCaps == nil {
		h.matchCaps = make(map[string]matchCap)
	}
	now := time.Now()
	for k, mc := range h.matchCaps {
		if now.Sub(mc.created) > matchCapTTL {
			delete(h.matchCaps, k)
		}
	}
	h.matchCaps[matchRoomKey(lobbyKey, name)] = matchCap{size: len(group), created: now}
	return group, name, true
}

// randomMatchID is 13 lowercase base32 characters (64 bits) from crypto/rand.
func randomMatchID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// matchRoomKey swaps the lobby's room name for the match room's in a full room key
// ("projectID:tenantID:endpointPath:room"; the endpoint path holds no ':').
func matchRoomKey(lobbyKey, matchRoom string) string {
	parts := strings.SplitN(lobbyKey, ":", 4)
	if len(parts) < 4 {
		return lobbyKey + "|" + matchRoom
	}
	return parts[0] + ":" + parts[1] + ":" + parts[2] + ":" + matchRoom
}

// matchTickInterval is how often a criteria-matchmaking lobby re-evaluates its waiting peers
// so time-based rating-band widening (and MaxWaitSecs) is honored even when no new peer
// connects, which would otherwise strand two waiting, still-incompatible peers forever.
const matchTickInterval = time.Second

// ensureMatchTicker starts a per-lobby re-evaluation goroutine if one isn't already running,
// so a peer unmatched on arrival is re-checked as its rating band widens. Idempotent; the
// goroutine is cancelled when the lobby empties. No-op unless time loosens matching: only a
// widening RATING band relaxes over time, so nothing else needs a timer.
func (h *wsHub) ensureMatchTicker(roomKey, roomField string, crit matchCriteria) {
	if !matchByRating(crit.MatchBy) || (crit.BandWidenPerSec <= 0 && crit.MaxWaitSecs <= 0) {
		return
	}
	h.mu.Lock()
	room := h.rooms[roomKey]
	if room == nil || room.matchCancel != nil {
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	room.matchCancel = cancel
	h.mu.Unlock()

	safego.Go("ws-match-ticker", func() { h.runMatchTicker(ctx, roomKey, roomField, crit) })
}

func (h *wsHub) runMatchTicker(ctx context.Context, roomKey, roomField string, crit matchCriteria) {
	t := time.NewTicker(matchTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Same match+notify pair the connect path uses: formMatch claims the group under
			// h.mu (so a concurrent connect can't double-pair it), and notifyMatched must run
			// WITHOUT the lock held. Pairing the last peers empties the lobby, cancelling us.
			if group, matchRoom, ok := h.formMatch(roomKey, crit); ok {
				notifyMatched(h, roomKey, matchRoom, roomField, group)
			}
		}
	}
}

// setLatestState records a client's most recent message in the room's tick buffer. Used
// only in tick_hz>0 mode: the read pump calls this instead of broadcasting per frame, and
// the per-room ticker emits one combined snapshot per tick. No-op if the room is gone.
func (h *wsHub) setLatestState(roomKey, clientID string, msg json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.rooms[roomKey]
	if room == nil {
		return
	}
	if room.latestStates == nil {
		room.latestStates = make(map[string]json.RawMessage)
	}
	room.latestStates[clientID] = msg
}

// ensureRoomTicker starts a per-room snapshot goroutine if one isn't running. Idempotent,
// safe to call from the read pump per frame; the ticker stops when the room empties.
func (h *wsHub) ensureRoomTicker(roomKey, roomName string, hz int) {
	if hz <= 0 {
		return
	}
	h.mu.Lock()
	room := h.rooms[roomKey]
	if room == nil || room.tickCancel != nil {
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	room.tickCancel = cancel
	h.mu.Unlock()

	safego.Go("ws-room-ticker", func() { h.runRoomTicker(ctx, roomKey, roomName, hz) })
}

func (h *wsHub) runRoomTicker(ctx context.Context, roomKey, roomName string, hz int) {
	interval := time.Second / time.Duration(hz)
	if interval <= 0 {
		interval = time.Millisecond * 33 // 30Hz fallback
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.emitRoomSnapshot(roomKey, roomName)
		}
	}
}

// emitRoomSnapshot broadcasts one combined frame of every client's latest state. Shape:
//
//	{"_type":"snapshot","_room":"<name>","states":{"<clientId>": <last-msg>, ...}}
//
// Skips when no state was recorded since the last tick (cleared on send; idle rooms silent).
func (h *wsHub) emitRoomSnapshot(roomKey, roomName string) {
	h.mu.Lock()
	room := h.rooms[roomKey]
	if room == nil || len(room.latestStates) == 0 {
		if room != nil {
			room.latestStates = nil
		}
		h.mu.Unlock()
		return
	}
	states := room.latestStates
	room.latestStates = nil
	h.mu.Unlock()

	payload, err := json.Marshal(map[string]any{
		"_type":  "snapshot",
		"_room":  roomName,
		"states": states,
	})
	if err != nil {
		return
	}
	h.broadcastAll(roomKey, payload, false)
}

// BroadcastToRoom implements events.WSBroadcaster, allowing server-side
// components (Actions Runner) to push messages to connected WebSocket clients.
func (h *wsHub) BroadcastToRoom(roomKey string, msg json.RawMessage) {
	h.broadcastAll(roomKey, []byte(msg), false)
}

// activeRooms returns all active room keys matching a prefix (e.g. "1:chat:").
func (h *wsHub) activeRooms(prefix string) map[string]int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make(map[string]int)
	for key, room := range h.rooms {
		if len(prefix) == 0 || len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			result[key] = len(room.clients)
		}
	}
	return result
}
