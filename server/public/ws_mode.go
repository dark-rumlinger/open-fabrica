package public

import (
	"encoding/json"

	"openfabrica/db"
)

// A realtime endpoint's per-connection behavior is a modeSession. The shared
// read/write pump (apiWebSocket) owns the whole connection lifecycle and calls
// these hooks for the parts that differ by delivery model, so the pump never
// branches on flags. relaySession broadcasts immediately; tickSession coalesces
// each client's latest frame into per-room snapshots; authoritativeSession
// routes _action frames through a server-owned room actor. Matchmaking is
// connect-time pairing layered on a relay-family session, not a strategy.
type modeSession interface {
	// intercept runs on each parsed text frame BEFORE _sender injection, _to
	// routing, delivery, or persistence. Returning true fully consumes the frame
	// (the authoritative _action path: applied by the actor, never relayed or
	// stored). Every other mode returns false.
	intercept(frame map[string]any) bool

	// roomSwitchField is the room_column a client may switch on in-band, or "" to
	// forbid it. Authoritative sessions return "" (the actor owns the room), as do
	// room_gated endpoints (the room is fixed at connect).
	roomSwitchField() string

	// deliver routes an accepted, _sender-injected, non-_to application frame.
	deliver(roomKey, roomName, clientID string, raw []byte)

	// coalesced reports whether deliver HELD the frame as latest-wins state rather
	// than sending it. Only the tick session does; it must be the session's own
	// answer, since inferring it from tick_hz misdescribes an authoritative
	// endpoint that also ticks yet delivers immediately.
	//
	// A coalesced frame is never persisted: the server drops it as soon as the next
	// one lands, so a row per client per tick records superseded positions and
	// buries the durable messages the table exists for.
	coalesced() bool

	// onDisconnect runs on read-pump exit. owner is false when a reconnect with the
	// same client id already superseded this connection, so a stale departure must
	// not tear down the successor's actor slot.
	onDisconnect(owner bool)
}

// connCtx carries everything buildModeSession needs from the accepted connection.
type connCtx struct {
	cfg          *wsEndpointConfig
	roomKey      string
	roomName     string
	endpointPath string
	roomTenantID int
	clientID     string
	userID       int
	clientMeta   json.RawMessage
	spectator    bool
	projectDB    *db.ProjectDB
}

// buildModeSession selects the per-connection strategy and performs any connect-
// time mode work. An authoritative reducer that loads owns the connection;
// otherwise matchmaking pairs the arrival and the connection falls to the relay
// family (tick when the endpoint ticks, plain relay otherwise). A reducer that
// will not load also falls back to the relay family.
func (h *Handler) buildModeSession(cx *connCtx) modeSession {
	base := relaySession{h: h, cfg: cx.cfg}

	if cx.cfg.Authoritative && cx.cfg.Reducer != "" {
		if a, ok := h.deps.WSHub.ensureActor(actorSpec{
			Key:           cx.roomKey,
			Room:          cx.roomName,
			Endpoint:      cx.endpointPath,
			TenantID:      cx.roomTenantID,
			ReducerSrc:    cx.cfg.Reducer,
			Ephemeral:     cx.cfg.Ephemeral,
			TickHz:        cx.cfg.TickHz,
			SeedTable:     cx.cfg.SeedTable,
			SeedKeyCol:    cx.cfg.SeedKeyColumn,
			SeedColumns:   cx.cfg.SeedColumns,
			SharedTable:   cx.cfg.SharedTable,
			SharedColumns: cx.cfg.SharedColumns,
			PDB:           cx.projectDB,
		}); ok {
			// The reducer's player.uid is defined only on a requires_auth endpoint
			// (frames contract). A token is validated whenever present so persisted
			// rows can carry user_id even on a public endpoint, but that authed id
			// must not reach the reducer here, or a logged-in visitor to a public
			// endpoint would get an unexpected player.uid. write_to_table's own
			// user_id stamping is a separate path and stays unaffected.
			authUID := 0
			if cx.cfg.RequiresAuth {
				authUID = cx.userID
			}
			a.join(cx.clientID, authUID, cx.clientMeta, cx.spectator)
			return &authoritativeSession{relaySession: base, actor: a, clientID: cx.clientID, roomKey: cx.roomKey}
		}
		// Reducer would not load: fall through to the relay family (relay/tick).
	}

	if cx.cfg.Matchmaking && !h.deps.WSHub.isMatchRoom(cx.roomKey) {
		crit := matchCriteria{
			GroupSize:       cx.cfg.GroupSize,
			MatchBy:         cx.cfg.MatchBy,
			RatingBand:      cx.cfg.RatingBand,
			BandWidenPerSec: cx.cfg.BandWidenPerSec,
			MaxWaitSecs:     cx.cfg.MaxWaitSecs,
		}
		// This arrival may complete a group; the hub mints one shared match room and
		// we push each paired peer a "matched" frame naming it. Peers then switch
		// into that room via room_column (a normal in-band room switch).
		if group, matchRoom, ok := h.deps.WSHub.formMatch(cx.roomKey, crit); ok {
			notifyMatched(h.deps.WSHub, cx.roomKey, matchRoom, cx.cfg.RoomColumn, group)
		}
		// A peer that couldn't be matched on arrival must still be re-evaluated as its
		// rating band widens with wait time - otherwise a quiet lobby of incompatible
		// peers never pairs despite MaxWaitSecs. ensureMatchTicker is idempotent and a
		// no-op unless criteria widening applies, so it's safe to call on every join.
		h.deps.WSHub.ensureMatchTicker(cx.roomKey, cx.cfg.RoomColumn, crit)
	}

	if cx.cfg.TickHz > 0 {
		return &tickSession{relaySession: base}
	}
	return &base
}

// relaySession is the default relay-family strategy: broadcast each frame to the
// rest of the room immediately. It also supplies the shared defaults every other
// session inherits (no intercept, in-band room switch on room_column unless
// room_gated, no teardown).
type relaySession struct {
	h   *Handler
	cfg *wsEndpointConfig
}

func (s *relaySession) intercept(map[string]any) bool { return false }

func (s *relaySession) roomSwitchField() string {
	if s.cfg.RoomGated {
		return "" // room_gated fixes the room at connect
	}
	return s.cfg.RoomColumn
}

func (s *relaySession) deliver(roomKey, _ string, clientID string, raw []byte) {
	s.h.deps.WSHub.broadcast(roomKey, clientID, raw, false)
}

// A relay sends every frame whole, so it has no coalesced lane and every frame
// still persists. authoritativeSession inherits this along with deliver.
func (s *relaySession) coalesced() bool { return false }

func (s *relaySession) onDisconnect(bool) {}

// tickSession coalesces continuous state: it holds each client's latest frame and
// a per-room ticker (started lazily on the first inbound frame) emits one combined
// snapshot per tick. Everything else is inherited from relaySession.
type tickSession struct {
	relaySession
}

func (s *tickSession) deliver(roomKey, roomName, clientID string, raw []byte) {
	s.h.deps.WSHub.setLatestState(roomKey, clientID, json.RawMessage(raw))
	s.h.deps.WSHub.ensureRoomTicker(roomKey, roomName, s.cfg.TickHz)
}

func (s *tickSession) coalesced() bool { return true }

// authoritativeSession is backed by a single-goroutine room actor that owns the
// state: _action frames are applied server-side (server-stamped _by, never
// relayed); plain frames still broadcast (inherited from relaySession) so chat
// alongside the game keeps working. The actor owns the room, so in-band room
// switching is disabled.
type authoritativeSession struct {
	relaySession
	actor    *roomActor
	clientID string
	roomKey  string // the connect-time room key, for releaseActor
}

func (s *authoritativeSession) intercept(frame map[string]any) bool {
	if _, isAction := frame["_action"]; isAction {
		s.actor.action(s.clientID, frame)
		return true
	}
	return false
}

func (s *authoritativeSession) roomSwitchField() string { return "" }

func (s *authoritativeSession) onDisconnect(owner bool) {
	// Only fire @leave when this connection still owns the id: a reconnect (same
	// cid) already took over the slot, so the old connection leaving must NOT mark
	// the player disconnected (the ghost bug).
	if owner {
		s.actor.leave(s.clientID)
	}
	s.h.deps.WSHub.releaseActor(s.roomKey)
}
