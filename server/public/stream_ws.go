package public

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/internal/safego"
	"openfabrica/internal/wsendpoint"
	"openfabrica/security"
	"openfabrica/server/middleware"
	"openfabrica/store"
)

// WebSocket (/ws) handler

// validTableName is the canonical SQL identifier regex shared across the public
// handlers. Aliases security.IdentifierRegex so every interpolation site validates
// against the same definition security.ValidateTableName/ValidateColumnName use.
var validTableName = security.IdentifierRegex

type wsEndpointConfig struct {
	Path         string
	WriteToTable string
	RoomColumn   string // JSON field to extract room from messages (e.g. "room")
	RequiresAuth bool
	// TickHz: 0 = relay each message immediately (chat-style). > 0 switches
	// the endpoint into snapshot mode: each client's latest state is held and
	// the hub emits one combined snapshot per tick.
	TickHz int
	// RateLimit: inbound messages per second per client. 0 = unlimited.
	RateLimit int
	// MaxMsgSize: hard cap on a single inbound frame in bytes. 0 = unlimited.
	MaxMsgSize int
	// TenantColumn: column on WriteToTable auto-stamped from claims.TenantID
	// on inbound persisted messages. Also folded into the room key so two
	// tenants on the same endpoint+room name cannot see each other's traffic.
	TenantColumn string
	// E2EE: when true the relay enforces end-to-end encryption - application frames
	// must carry the {iv,ct} ciphertext envelope (see e2eeFrameAllowed), plaintext
	// bodies are dropped, and only ciphertext is persisted. The crypto runs in the
	// browser (js/e2ee.js); the server never holds a key. Control/handshake and
	// WebRTC signaling frames still pass in the clear.
	E2EE bool
	// Ephemeral: when true the relay deletes this endpoint's persisted rows for a
	// room the moment it empties, so a throwaway session leaves nothing behind. Only
	// acts when WriteToTable and RoomColumn are both set (the delete is scoped WHERE
	// RoomColumn = <room>). Defaults on for e2ee endpoints.
	Ephemeral bool
	// MessageTTLSeconds: when > 0, persisted rows on WriteToTable older than this
	// many seconds are swept on relay activity (connect / room-empty). Age-based
	// "disappearing messages", composing with the occupancy-based Ephemeral prune.
	// 0 = keep forever. See pruneExpiredMessages.
	MessageTTLSeconds int
	// RoomGated: when true the relay requires a matching per-room access token
	// (?join=<token>) before a peer may join any non-global room. The first peer into
	// an unclaimed room mints it (returned in welcome._joinToken); later peers must
	// present it or the upgrade is refused with 401. Distinct from any E2EE password,
	// it is the only thing stopping a peer who merely knows the room name from pulling
	// the peer-to-peer media. See roomJoinGate / pruneRoomToken.
	RoomGated bool
	// Matchmaking: when true the relay pairs arriving peers server-side. A peer waits
	// in the lobby room; once GroupSize unmatched peers are present the server mints
	// one shared match room, assigns roles, and pushes each a "matched" frame. The
	// peer then switches into that room via RoomColumn. See formMatch / notifyMatched.
	Matchmaking bool
	// GroupSize: peers per match for a matchmaking endpoint (>=2).
	GroupSize int
	// Matchmaking tuning (criteria-based pairing). MatchBy: "" (any peer) |
	// "rating" | "region" | "rating+region"; peers pass {rating, region} in their
	// ?meta= presence profile. RatingBand is the initial rating half-width;
	// BandWidenPerSec grows it with the longest-waiting peer's wait; MaxWaitSecs is
	// when that peer matches any rating (region still honored). See formMatch.
	MatchBy         string
	RatingBand      int
	BandWidenPerSec float64
	MaxWaitSecs     int
	// MaxClients: cap on participants per room (0 = unlimited). Beyond it, joiners
	// become read-only spectators. Not applied on matchmaking lobbies. See join.
	MaxClients int
	// Authoritative: when true the endpoint is backed by a room actor that runs
	// Reducer; _action frames are applied server-side and each player is pushed
	// their view(). See room_actor.go.
	Authoritative bool
	// Reducer: the model-authored reduce()/view() JS the room actor runs sandboxed.
	Reducer string
	// Join-seed: on @join the room actor reads SeedColumns from SeedTable WHERE
	// SeedKeyColumn = <authenticated uid> and injects them as event.player.seed, so an
	// authoritative reducer seeds durable state from the DB rather than trusting client
	// meta. Empty SeedTable = disabled. Authored via manage_authoritative set_reducer.
	SeedTable     string
	SeedKeyColumn string
	SeedColumns   []string
	// Shared/global read: when set, the room actor reads SharedTable's rows
	// (SharedColumns, or all) and injects them as event.shared on each non-@tick
	// event, so an authoritative reducer can read cross-room/global state. Empty
	// SharedTable = disabled. Authored via manage_authoritative set_reducer.
	SharedTable   string
	SharedColumns []string
	// Cached column schema for write_to_table. tableColumns is the SET of column
	// names write_to_table may fill: membership only, since the declared type is
	// never consulted here.
	tableColumns map[string]bool
	// Cached set of columns a write_to_table insert MUST supply: NOT NULL columns
	// with no DEFAULT (the integer PK excluded). A relay carries heterogeneous frames,
	// so wsWriteToTable skips a frame that omits any of these instead of throwing a
	// NOT NULL constraint per frame.
	requiredColumns map[string]bool
}

func (h *Handler) loadWSEndpoint(projectDB *sql.DB, path string) (*wsEndpointConfig, error) {
	row, err := wsendpoint.Load(projectDB, path)
	if err != nil {
		return nil, err
	}
	ws := &wsEndpointConfig{
		Path:              row.Path,
		WriteToTable:      row.WriteToTable,
		RoomColumn:        row.RoomColumn,
		RequiresAuth:      row.RequiresAuth,
		TickHz:            row.TickHz,
		RateLimit:         row.RateLimit,
		MaxMsgSize:        row.MaxMsgSize,
		TenantColumn:      row.TenantColumn,
		E2EE:              row.E2EE,
		Ephemeral:         row.Ephemeral,
		RoomGated:         row.RoomGated,
		Matchmaking:       row.Matchmaking,
		GroupSize:         row.GroupSize,
		MaxClients:        row.MaxClients,
		Authoritative:     row.Authoritative,
		Reducer:           row.Reducer,
		MessageTTLSeconds: row.MessageTTLSeconds,
		SeedTable:         row.SeedTable,
		SeedKeyColumn:     row.SeedKeyColumn,
		SeedColumns:       row.SeedColumnList(),
		MatchBy:           row.MatchBy,
		RatingBand:        row.RatingBand,
		BandWidenPerSec:   row.BandWidenPerSec,
		MaxWaitSecs:       row.MaxWaitSecs,
		SharedTable:       row.SharedTable,
		SharedColumns:     row.SharedColumnList(),
	}

	// Pre-load table column schema for structured write_to_table inserts.
	if ws.WriteToTable != "" && validTableName.MatchString(ws.WriteToTable) {
		var schemaDef sql.NullString
		// A miss leaves tableColumns nil; wsWriteToTable then falls back to the
		// raw-JSON `data` column insert. The shared schema reader handles both forms
		// a column may be stored in, so constraints map like bare types.
		_ = projectDB.QueryRow("SELECT schema_def FROM of_dynamic_tables WHERE table_name = ?", ws.WriteToTable).Scan(&schemaDef)
		if schemaDef.Valid && schemaDef.String != "" {
			ws.tableColumns = store.SchemaDefColumnSet(schemaDef.String)
		}
		// Cache the table's required (NOT NULL, no-default) columns from the live
		// schema so wsWriteToTable can skip a frame that can't fill them, rather than
		// attempting the insert and logging a NOT NULL constraint failure per frame.
		if phys, perr := store.SanitizedTableName(ws.WriteToTable); perr == nil {
			if req, rerr := store.RequiredColumns(projectDB, phys); rerr == nil {
				ws.requiredColumns = req
			}
		}
	}
	return ws, nil
}

// resolveClientID picks a STABLE per-player id so a reconnect re-attaches to the
// same identity instead of arriving as a new player and ghosting its old slot.
// Priority: the authenticated user (unspoofable) > a client-supplied ?cid=
// (client-asserted, fine for casual games) > a fresh per-connection UUID.
//
// The ?cid= is a private reconnect SECRET, never the public id: the id handed out in
// welcome._clientId / _peers / _sender is a one-way hash of it (publicClientID). Were
// the cid itself public, any peer could read a victim's id off the roster, reconnect
// with ?cid=<victim>, and the hub's same-id takeover would close the victim's socket
// and hand the attacker their slot (and, on an authoritative game, their _by).
func resolveClientID(r *http.Request, projectID int, requiresAuth bool, userID int) string {
	if requiresAuth && userID > 0 {
		return fmt.Sprintf("u%d", userID)
	}
	if cid := r.URL.Query().Get("cid"); validClientID(cid) {
		return publicClientID(projectID, cid)
	}
	return uuid.New().String()
}

// publicClientID derives the roster-visible id from a private cid. Scoped per
// project, not per endpoint, so a matchmaking lobby and the authoritative game its
// matched peers move to see the same id for the same browser.
func publicClientID(projectID int, cid string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("of-cid|%d|%s", projectID, cid)))
	return "c-" + hex.EncodeToString(sum[:])[:20]
}

// effectiveRoomTenant returns the tenant a WebSocket room key is scoped to: the
// caller's tenant ONLY when the endpoint opted into tenancy (has a tenant_column),
// else 0, so a signed-in player's JWT tenant never silently shards a public
// game/relay room. Mirrors apiWSRooms and the ws_broadcast trigger. The caller
// keeps the real tenant for the active-tenant guard and tenant_column row-stamping.
func effectiveRoomTenant(ws *wsEndpointConfig, tenantID int) int {
	if ws != nil && ws.TenantColumn != "" {
		return tenantID
	}
	return 0
}

// wsCloseSuperseded is the close code a connection gets when a newer one with the same
// client id takes its slot. The ofnet helper does not auto-reconnect on it.
const wsCloseSuperseded websocket.StatusCode = 4001

// reservedInboundKeys are the system-frame fields the server owns. They are
// stripped from every client frame before it is relayed or persisted, so a peer
// cannot forge presence, matchmaking, or authoritative state by hand-writing a
// frame the helper would treat as the server's. _to and _discrete are NOT here:
// those are legitimate client->server markers, read and stripped where they are
// acted on.
// The list stays inside the underscore namespace on purpose: "state" and "rev"
// ride the authoritative frame but are perfectly ordinary app field names, and
// stripping _type alone already stops a forged system frame from being dispatched
// as one (the helper keys every system frame off _type).
var reservedInboundKeys = []string{
	"_type", "_clientId", "_clients", "_peers", "_role", "_room", "_capacity",
	"_joinToken", "_sender", "_by", "_fault", "_message",
}

// validClientID accepts a bounded, url-safe client id. Anything else falls back to
// a server UUID rather than being rejected, so a bad value never breaks connect.
func validClientID(s string) bool {
	if len(s) < 6 || len(s) > 64 {
		return false
	}
	for _, ch := range s {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
		default:
			return false
		}
	}
	return true
}

// wsErr sends a `{_type:"error",_message:msg}` text frame to the client. The
// write error is ignored: a failed write means the conn is already gone, which
// the read loop observes on its next iteration.
func wsErr(ctx context.Context, conn *websocket.Conn, msg string) {
	body, _ := json.Marshal(map[string]any{"_type": "error", "_message": msg})
	_ = conn.Write(ctx, websocket.MessageText, body)
}

// wsConnectSpec is everything apiWebSocket resolves before the upgrade: the
// endpoint config plus the per-connection identity and room facts the pumps need.
type wsConnectSpec struct {
	wsCfg        *wsEndpointConfig
	endpointPath string
	userID       int
	roomTenantID int
	initialRoom  string
	clientMeta   json.RawMessage
	joinToken    string
	isClaimer    bool
}

// prepareWSConnect runs the whole pre-upgrade gate: endpoint load, token/auth,
// tenant gate, room-name and presence-meta parsing, the per-room join gate, and
// the per-visitor connection cap. It writes the HTTP error itself and reports
// ok=false when the connection must not proceed; on ok=true the caller must
// defer the returned release. All of this must run before websocket.Accept
// hijacks the response, since a refusal after that is no longer a readable HTTP
// status.
func (h *Handler) prepareWSConnect(w http.ResponseWriter, r *http.Request, projectID int, projectDB *db.ProjectDB) (spec wsConnectSpec, release func(), ok bool) {
	// Extract the endpoint path from the URL (strip trailing "/ws" sub-route).
	fullPath := strings.TrimPrefix(r.URL.Path, "/api/")
	endpointPath := strings.TrimSuffix(fullPath, "/ws")

	wsCfg, err := h.loadWSEndpoint(projectDB.Reader(), endpointPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "websocket endpoint not found")
		return spec, nil, false
	}

	// Validate the token (always, when present) so we can stamp the user_id
	// on persisted messages even on endpoints that don't *require* auth. The
	// RequiresAuth gate only changes whether a missing/invalid token is fatal.
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	var userID, tenantID int
	if token != "" {
		okTok, claims := h.validateProjectTokenOrJWTWithClaims(projectID, token)
		if !okTok && wsCfg.RequiresAuth {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return spec, nil, false
		}
		if claims != nil {
			userID = claims.UserID
			tenantID = claims.TenantID
		}
	} else if wsCfg.RequiresAuth {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return spec, nil, false
	}

	// Tenanted endpoint without an active tenant: refuse the upgrade. The
	// session has no way to write or read tenant-scoped state.
	if wsCfg.TenantColumn != "" && wsCfg.RequiresAuth && tenantID == 0 {
		writeError(w, http.StatusForbidden, "no active tenant; call /tenant/switch first")
		return spec, nil, false
	}

	// The room key is tenant-scoped only when the endpoint opted into tenancy; a
	// non-tenanted game/relay endpoint shares one room across tenants (the real
	// tenantID stays available for the guard above and tenant_column row-stamping).
	roomTenantID := effectiveRoomTenant(wsCfg, tenantID)

	initialRoom := r.URL.Query().Get("room") // default "" = global room
	if len(initialRoom) > wsMaxRoomName {
		writeError(w, http.StatusBadRequest, "room name too long")
		return spec, nil, false
	}

	// Optional presence profile supplied at connect (?meta=<json>): carried in the
	// welcome roster and join broadcast so peers see names/avatars without an
	// app-level announce. Capped and JSON-validated; anything else is ignored.
	var clientMeta json.RawMessage
	if raw := r.URL.Query().Get("meta"); raw != "" && len(raw) <= 2048 && json.Valid([]byte(raw)) {
		clientMeta = json.RawMessage(raw)
	}

	// Per-room entry gate. Must run BEFORE the upgrade so a refusal is a plain HTTP
	// 401 the client can read (once websocket.Accept hijacks the connection we can no
	// longer write a status). The first peer into an unclaimed room claims it
	// (isClaimer) and gets the minted token back in welcome to build the invite link;
	// later peers must present a matching ?join=.
	var joinToken string
	var isClaimer bool
	if wsCfg.RoomGated {
		tok, claimer, okGate := h.roomJoinGate(projectDB, endpointPath, roomTenantID, initialRoom, r.URL.Query().Get("join"))
		if !okGate {
			writeError(w, http.StatusUnauthorized, "invalid or missing room access token")
			return spec, nil, false
		}
		joinToken, isClaimer = tok, claimer
	}

	// One visitor may hold only so many sockets: each carries buffers and goroutines.
	releaseConn, okConn := acquireWSConn(projectID, middleware.ClientIP(r))
	if !okConn {
		if isClaimer {
			pruneRoomToken(projectDB, wsCfg, initialRoom, roomTenantID)
		}
		writeError(w, http.StatusTooManyRequests, "too many open connections")
		return spec, nil, false
	}

	return wsConnectSpec{
		wsCfg:        wsCfg,
		endpointPath: endpointPath,
		userID:       userID,
		roomTenantID: roomTenantID,
		initialRoom:  initialRoom,
		clientMeta:   clientMeta,
		joinToken:    joinToken,
		isClaimer:    isClaimer,
	}, releaseConn, true
}

// apiWebSocket handles GET /api/{path}/ws: pure transparent relay with room support.
// Messages are relayed as-sent to all other clients in the same room, with _sender injected.
func (h *Handler) apiWebSocket(w http.ResponseWriter, r *http.Request, projectID int, projectDB *db.ProjectDB) {
	spec, releaseConn, ok := h.prepareWSConnect(w, r, projectID, projectDB)
	if !ok {
		return
	}
	defer releaseConn()
	wsCfg := spec.wsCfg
	endpointPath := spec.endpointPath
	userID := spec.userID
	roomTenantID := spec.roomTenantID
	initialRoom := spec.initialRoom
	clientMeta := spec.clientMeta
	joinToken := spec.joinToken
	isClaimer := spec.isClaimer

	// Accept the WebSocket connection. CORS does not apply to WebSocket upgrades, so
	// the browser's origin policy does not gate this; the real protection is the
	// bearer/join-token check above, which runs before the upgrade. Auth is
	// token-based (not cookie-based), so a cross-origin page cannot ride a victim's
	// ambient credentials onto the socket.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Warn("websocket accept error", "project_id", projectID, "path", endpointPath, "error", err)
		if isClaimer {
			// This claimer minted a room token before the upgrade; with Accept failed,
			// no client ever joins/leaves to prune it, leaving the room permanently
			// gated with a token nobody holds. Release it so the next peer can reclaim.
			pruneRoomToken(projectDB, wsCfg, initialRoom, roomTenantID)
		}
		return
	}
	defer func() { _ = conn.CloseNow() }()

	// Make the configured max_msg_size govern reads. The coder/websocket default read
	// limit is 32KB; without raising it, a frame between 32KB and the configured cap
	// is closed by the library (status 1009) before the app-level size check runs, so
	// the peer silently disconnects instead of getting the graceful "too large" error.
	// Add a small slack so a marginally oversize frame still hits the in-band reject
	// (connection stays open) rather than a hard library close; 0 = unlimited.
	conn.SetReadLimit(wsReadLimit(wsCfg.MaxMsgSize))

	clientID := resolveClientID(r, projectID, wsCfg.RequiresAuth, userID)
	// Room key folds the EFFECTIVE tenant in: on a tenant_column endpoint two
	// tenants with the same room name land in separate hubs, but a non-tenanted
	// endpoint uses roomTenantID=0 so every player shares the room regardless of
	// any tenant their JWT carries.
	roomKey := events.RoomKey(projectID, roomTenantID, endpointPath, initialRoom)

	// Hand the minted token to the claiming peer via the welcome frame so it can
	// assemble the invite link; everyone else's welcome is unchanged.
	var extraWelcome map[string]any
	if isClaimer && joinToken != "" {
		extraWelcome = map[string]any{"_joinToken": joinToken}
	}
	// Capacity applies to the room you occupy, not a matchmaking holding lobby
	// (group_size governs match size there).
	roomCap := wsCfg.MaxClients
	if wsCfg.Matchmaking {
		roomCap = 0
	}
	client := h.deps.WSHub.join(roomKey, initialRoom, clientID, roomCap, clientMeta, extraWelcome)
	spectator := client.spectator

	// Build the per-connection mode strategy (relay / tick / authoritative /
	// matchmaking / signaling). Does any connect-time mode work (matchmaking pairing,
	// wiring the authoritative room actor) and returns the strategy the read pump
	// dispatches through, so the pump never branches on model flags. See ws_mode.go.
	sess := h.buildModeSession(&connCtx{
		cfg:          wsCfg,
		roomKey:      roomKey,
		roomName:     initialRoom,
		endpointPath: endpointPath,
		roomTenantID: roomTenantID,
		clientID:     clientID,
		userID:       userID,
		clientMeta:   clientMeta,
		spectator:    spectator,
		projectDB:    projectDB,
	})

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Track current room (key + user-facing name) for dynamic room switching via room_column.
	currentRoomKey := roomKey
	currentRoomName := initialRoom

	defer func() {
		empty, owner := h.deps.WSHub.leave(currentRoomKey, currentRoomName, client)
		sess.onDisconnect(owner)
		if owner && empty {
			pruneEphemeralRoom(projectDB, wsCfg, currentRoomName, roomTenantID)
			pruneRoomToken(projectDB, wsCfg, currentRoomName, roomTenantID)
		}
		pruneExpiredMessages(projectDB, wsCfg, roomTenantID)
	}()

	// Disappearing messages: sweep rows past their TTL when a peer connects, so a
	// joiner never loads history older than message_ttl_seconds (no-op when unset).
	pruneExpiredMessages(projectDB, wsCfg, roomTenantID)

	// Per-client inbound rate limit: simple sliding-window token bucket,
	// refilled at RateLimit messages per second. 0 = unlimited.
	rateTokens := float64(wsCfg.RateLimit)
	lastTokenRefill := time.Now()
	checkRate := func() bool {
		if wsCfg.RateLimit <= 0 {
			return true
		}
		now := time.Now()
		elapsed := now.Sub(lastTokenRefill).Seconds()
		rateTokens += elapsed * float64(wsCfg.RateLimit)
		if rateTokens > float64(wsCfg.RateLimit) {
			rateTokens = float64(wsCfg.RateLimit)
		}
		lastTokenRefill = now
		if rateTokens < 1 {
			return false
		}
		rateTokens--
		return true
	}

	// Read pump: reads incoming messages and broadcasts to room.
	go func() {
		defer cancel()
		defer safego.Recover("ws-read-pump")
		spectatorNotified := false
		actionRefusedNotified := false
		throttleNotified := false
		ratchetPersistNotified := false
		for {
			msgType, data, err := conn.Read(ctx)
			if err != nil {
				return
			}

			// Spectators (room was full at join) are read-only: drop their inbound
			// frames, telling them once why. They still receive all broadcasts.
			// Read the live flag, not the connect-time copy: an in-band switch into a full
			// room demotes a player to spectator (moveClient), and the copy would keep
			// accepting its input.
			if h.deps.WSHub.isSpectator(client) {
				if !spectatorNotified {
					spectatorNotified = true
					wsErr(ctx, conn, "room is full; you joined as a spectator (read-only)")
				}
				continue
			}

			if wsCfg.MaxMsgSize > 0 && len(data) > wsCfg.MaxMsgSize {
				wsErr(ctx, conn, fmt.Sprintf("message dropped: size %d exceeds limit %d bytes", len(data), wsCfg.MaxMsgSize))
				continue
			}
			if !checkRate() {
				// Tell the client once. A silent drop is indistinguishable from a
				// bug in their own send path, and a per-frame send loop can sit above
				// the limit for the whole session without ever finding out.
				if !throttleNotified {
					throttleNotified = true
					wsErr(ctx, conn, fmt.Sprintf("frames dropped: inbound rate limit of %d msg/sec exceeded; send continuous state with net.send at your tick rate rather than every animation frame", wsCfg.RateLimit))
				}
				continue
			}

			isBinary := msgType == websocket.MessageBinary

			// Binary messages: relay as-is with no JSON processing, for compact game
			// state updates. Binary frames bypass tick coalescing (already the compact
			// path; the snapshot frame is JSON-only).
			if isBinary {
				// On an e2ee endpoint a raw binary frame can't be verified as
				// ciphertext (it never reaches the JSON plaintext guard), so it would be
				// an unencrypted leak. The e2ee helper never sends binary, so refuse it
				// here rather than relay cleartext bytes.
				if wsCfg.E2EE {
					wsErr(ctx, conn, "e2ee endpoint: binary frames are not allowed; send encrypted blobs through the 'e2ee' session's encryptBlob(), which returns {meta, chunks:[base64]} to carry as JSON")
					continue
				}
				h.deps.WSHub.broadcast(currentRoomKey, clientID, data, true)
				continue
			}

			// Text messages: validate JSON, inject _sender, relay.
			var msgMap map[string]any
			if err := json.Unmarshal(data, &msgMap); err != nil {
				continue // skip non-JSON or non-object messages
			}

			// The system-frame namespace is server-only: these are keys clients
			// RECEIVE and never send. Relayed on, a forged {"_type":"welcome"} or
			// {"_type":"leave","_sender":"<victim>"} drives every peer's helper down
			// its presence path and wipes or rewrites their roster, and a forged
			// "matched" walks the room into an attacker-chosen id. App frames use
			// "type", so dropping these costs a well-behaved client nothing.
			for _, k := range reservedInboundKeys {
				delete(msgMap, k)
			}

			// Mode intercept: the authoritative session consumes _action frames (the
			// room actor applies them and server-stamps _by; they are neither relayed
			// nor persisted). Every other mode returns false and the frame continues.
			if sess.intercept(msgMap) {
				continue
			}
			// An _action frame that no actor consumed means net.action() was called on a
			// relay/tick/matchmaking endpoint (or one whose reducer was never attached).
			// Relayed on, it would reach peers as a plain frame and never change any state,
			// with nothing in the console. Refuse it visibly, once per connection.
			if _, isAction := msgMap["_action"]; isAction {
				if !actionRefusedNotified {
					actionRefusedNotified = true
					wsErr(ctx, conn, "net.action() needs an authoritative endpoint (manage_authoritative create + set_reducer); this endpoint has no reducer, so the action was dropped. Use net.event/net.send here.")
				}
				continue
			}

			// Dynamic room switching: if the frame carries the mode's room-switch
			// field, move the client to that room. The mode decides whether switching
			// is allowed (authoritative sessions and room_gated endpoints return "").
			if field := sess.roomSwitchField(); field != "" {
				if msgRoom, ok := msgMap[field].(string); ok && msgRoom != "" {
					newRoomKey := events.RoomKey(projectID, roomTenantID, endpointPath, msgRoom)
					if newRoomKey != currentRoomKey {
						leftRoomName := currentRoomName
						// Move the SAME client object (and its channel) so the write
						// pump keeps delivering in the new room; a leave+new-join would
						// orphan the channel and the switched peer would go silent.
						if oldEmpty, ok := h.deps.WSHub.moveClient(client, currentRoomKey, leftRoomName, newRoomKey, msgRoom, wsCfg.MaxClients); ok {
							if oldEmpty {
								pruneEphemeralRoom(projectDB, wsCfg, leftRoomName, roomTenantID)
								pruneRoomToken(projectDB, wsCfg, leftRoomName, roomTenantID)
							}
							currentRoomKey = newRoomKey
							currentRoomName = msgRoom
							// A room-switch frame is control plumbing, like _discrete: the
							// move already replays a fresh welcome + join/leave to everyone
							// who needs to know. Relaying it on would broadcast
							// {<room_column>:"m-1"} into the destination room on every
							// matchmaking handoff and, on a write_to_table endpoint, insert
							// a junk row for it.
							continue
						}
					}
				}
			}

			// E2EE enforcement: on an encrypted endpoint, refuse to relay or
			// persist anything that isn't the ciphertext envelope or a permitted
			// cleartext control/signaling frame. This is what turns "the client
			// should encrypt" into "the relay never carries plaintext bodies."
			if wsCfg.E2EE && !e2eeFrameAllowed(msgMap) {
				wsErr(ctx, conn, "e2ee endpoint: plaintext message body rejected; send the {iv,ct} envelope via the 'e2ee' session's encrypt() (import { session } from 'e2ee')")
				continue
			}

			// Discrete frames (net.event: chat, emotes, one-off signals) must reach the
			// room as distinct events. Read and STRIP the marker: it is client<->server
			// plumbing, not app data, and a tick session would otherwise fold a discrete
			// frame into the sender's latest-state slot, where all but the last per tick
			// are dropped.
			discrete, _ := msgMap["_discrete"].(bool)
			delete(msgMap, "_discrete")

			// _to is the same kind of marker - it addresses the frame, it is not part
			// of it - so read and strip it here too, before the frame is marshalled.
			// Left in, the recipient sees the routing key as app data and a
			// write_to_table endpoint persists it into the message row.
			to, _ := msgMap["_to"].(string)
			delete(msgMap, "_to")

			// Inject _sender so receivers know who sent the message.
			msgMap["_sender"] = clientID

			outBytes, err := json.Marshal(msgMap)
			if err != nil {
				continue
			}

			// Route the frame. _to and _discrete both bypass mode delivery: a _to frame
			// is addressed to one client, a discrete frame must broadcast now. Otherwise
			// the mode delivers: relay broadcasts immediately, tick coalesces.
			coalesced := false
			if to != "" {
				h.deps.WSHub.sendTo(currentRoomKey, to, outBytes, false)
			} else if discrete {
				h.deps.WSHub.broadcast(currentRoomKey, clientID, outBytes, false)
			} else {
				sess.deliver(currentRoomKey, currentRoomName, clientID, outBytes)
				coalesced = sess.coalesced()
			}

			// Optional: write to table with structured column mapping. On an e2ee
			// endpoint only the ciphertext envelope is persisted. A coalesced frame is
			// never persisted: on a tick endpoint net.send is continuous latest-wins
			// state, so a row per client per tick would bury the durable messages the
			// table exists for. The durable lane is net.event (and _to), persisted here.
			if wsCfg.WriteToTable != "" && !coalesced && validTableName.MatchString(wsCfg.WriteToTable) &&
				(!wsCfg.E2EE || isE2EEEnvelope(msgMap)) {
				if wsCfg.E2EE && isE2EERatchetEnvelope(msgMap) {
					// Ratchet destroys each message key after use, so this durable write
					// would store a row no member can ever reopen. The frame still reached
					// the room live (above); only the futile persist is skipped. Tell the
					// app once so the misconfiguration surfaces in testing rather than as
					// silently unreadable history.
					if !ratchetPersistNotified {
						ratchetPersistNotified = true
						wsErr(ctx, conn, "e2ee endpoint: ratchet messages are not persisted (their keys are destroyed after use), so this write_to_table would store unreadable rows. Use archive:true for durable encrypted history, or drop write_to_table for a ratcheting room.")
					}
				} else {
					wsWriteToTable(projectDB, wsCfg, msgMap, userID, roomTenantID, currentRoomName)
				}
			}
		}
	}()

	// Write pump: drains the client's outbound channel and sends keepalives.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-client.superseded:
			// Another connection took this client's id over. Close with a dedicated
			// code (not the bare drop CloseNow gives) so the ofnet helper can tell
			// "replaced by a newer connection" from a network drop and NOT auto-
			// reconnect - two sockets sharing one id would otherwise take the slot
			// back from each other forever. Returning unwinds the read pump too, so
			// the identity keeps exactly one live socket.
			_ = conn.Close(wsCloseSuperseded, "superseded by a newer connection with the same client id")
			return
		case msg := <-client.ch:
			wsMsgType := websocket.MessageText
			if msg.binary {
				wsMsgType = websocket.MessageBinary
			}
			// Per-write deadline: a stalled/slow consumer must not wedge the write
			// pump forever (which would also starve the keepalive ping and pin this
			// goroutine). If a single write can't land in time, drop the connection.
			writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(writeCtx, wsMsgType, msg.data)
			cancel()
			if err != nil {
				return
			}
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// notifyMatched pushes each peer in a freshly formed matchmaking group a "matched"
// frame naming the shared match room, the peer's assigned role (index 0 "host", the
// rest "player"), its index, and the ordered roster. Delivered into the lobby room
// the peers are still in; each then switches into matchRoom via room_column. Sent
// (not broadcast) so the assignment stays private to the matched peers.
func notifyMatched(hub *wsHub, lobbyRoomKey, matchRoom, roomField string, group []string) {
	if roomField == "" {
		roomField = "room"
	}
	for i, id := range group {
		role := "player"
		if i == 0 {
			role = "host"
		}
		frame, err := json.Marshal(map[string]any{
			"_type":      "matched",
			"_room":      matchRoom,
			"room_field": roomField, // the room_column field to switch with (the 'ofnet' helper uses it)
			"role":       role,
			"index":      i,
			"players":    group,
		})
		if err != nil {
			continue
		}
		hub.sendTo(lobbyRoomKey, id, frame, false)
	}
}

// apiWSRooms returns the list of active WebSocket rooms and their client counts.
// Room keys are tenant-scoped, so an endpoint with a tenant_column requires a
// tenant-scoped JWT (we read that tenant's bucket); non-tenanted endpoints read the
// anonymous bucket (tenantID=0).
func (h *Handler) apiWSRooms(w http.ResponseWriter, r *http.Request, projectID int, projectDB *db.ProjectDB, endpointPath string) {
	tenantID := 0
	wsCfg, _ := h.loadWSEndpoint(projectDB.Reader(), endpointPath)
	if wsCfg != nil && wsCfg.TenantColumn != "" {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		ok, claims := h.validateProjectTokenOrJWTWithClaims(projectID, token)
		if !ok || claims == nil || claims.TenantID == 0 {
			writeError(w, http.StatusUnauthorized, "tenant-scoped token required")
			return
		}
		tenantID = claims.TenantID
	} else if wsCfg != nil && wsCfg.RequiresAuth {
		// Auth-gated but not tenanted: the room roster and client ids are only
		// for authenticated callers. Without this an anonymous request enumerates
		// every room and connected client id on a requires_auth endpoint.
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if ok, _ := h.validateProjectTokenOrJWTWithClaims(projectID, token); !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	prefix := events.RoomKeyPrefix(projectID, tenantID, endpointPath)
	specificRoom := r.URL.Query().Get("room")

	if specificRoom != "" {
		// Return clients in a specific room.
		roomKey := events.RoomKey(projectID, tenantID, endpointPath, specificRoom)
		clients := h.deps.WSHub.roomClients(roomKey)
		if clients == nil {
			clients = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"room":    specificRoom,
			"clients": clients,
			"count":   len(clients),
		})
		return
	}

	// Return all rooms for this endpoint.
	activeRooms := h.deps.WSHub.activeRooms(prefix)
	type roomInfo struct {
		Room  string `json:"room"`
		Count int    `json:"count"`
	}
	var rooms []roomInfo
	for key, count := range activeRooms {
		roomName := key[len(prefix):]
		rooms = append(rooms, roomInfo{Room: roomName, Count: count})
	}
	if rooms == nil {
		rooms = []roomInfo{}
	}
	writeJSON(w, http.StatusOK, rooms)
}
