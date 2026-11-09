package public

import (
	"crypto/subtle"
	"database/sql"
	"log/slog"

	"github.com/google/uuid"

	"openfabrica/db"
)

// Per-room access gating for room_gated endpoints: the first peer into an
// unclaimed room mints (or reuses) a token that later peers must present, and the
// token is released when the room empties so the name becomes reclaimable.

// roomJoinGate enforces the per-room access token on a room_gated endpoint. It
// returns (token, isClaimer, ok): when ok is false the caller must refuse the
// upgrade with 401. The global room ("") is never gated. The FIRST peer into an
// unclaimed room claims it - the relay mints a token (or honors one presented
// from a pre-existing invite link, so a shared link keeps working across a brief
// empty) and returns isClaimer=true so the welcome frame can hand the token back
// for the invite link. Every later peer must present a matching ?join=<token>.
// The token is deliberately separate from any E2EE password, which never reaches
// the server. All error paths fail closed.
func (h *Handler) roomJoinGate(projectDB *db.ProjectDB, endpointPath string, tenantID int, room, presented string) (token string, isClaimer bool, ok bool) {
	if room == "" {
		return "", false, true // global channel is not gated
	}

	var stored string
	err := projectDB.QueryRow(
		"SELECT token FROM of_ws_room_tokens WHERE endpoint_path = ? AND tenant_id = ? AND room = ?",
		endpointPath, tenantID, room,
	).Scan(&stored)
	switch {
	case err == nil:
		// Room already claimed: the presented token must match exactly.
		return stored, false, tokenEqual(presented, stored)
	case err != sql.ErrNoRows:
		slog.Warn("ws room gate: token lookup failed", "path", endpointPath, "room", room, "error", err)
		return "", false, false // DB error - fail closed
	}

	// Unclaimed room. Honor a token carried from an existing invite link;
	// otherwise mint a fresh one. INSERT OR IGNORE + re-read resolves the race
	// where two peers reach an empty room simultaneously - exactly one wins the
	// row and becomes the claimer; the loser is treated as a normal joiner.
	minted := presented
	if minted == "" {
		minted = uuid.New().String()
	}
	if _, ierr := projectDB.ExecWrite(
		"INSERT OR IGNORE INTO of_ws_room_tokens (endpoint_path, tenant_id, room, token) VALUES (?, ?, ?, ?)",
		endpointPath, tenantID, room, minted,
	); ierr != nil {
		slog.Warn("ws room gate: token claim failed", "path", endpointPath, "room", room, "error", ierr)
		return "", false, false
	}

	var final string
	if rerr := projectDB.QueryRow(
		"SELECT token FROM of_ws_room_tokens WHERE endpoint_path = ? AND tenant_id = ? AND room = ?",
		endpointPath, tenantID, room,
	).Scan(&final); rerr != nil {
		slog.Warn("ws room gate: token re-read failed", "path", endpointPath, "room", room, "error", rerr)
		return "", false, false
	}
	if final == minted {
		return final, true, true // we won the claim
	}
	// Lost the race: another peer claimed first. Require the presented token.
	return final, false, tokenEqual(presented, final)
}

// tokenEqual is a length-checked constant-time comparison of two access tokens.
func tokenEqual(a, b string) bool {
	if a == "" || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// pruneRoomToken deletes a room_gated endpoint's access token when the room
// empties, mirroring pruneEphemeralRoom: the claim is single-session, so the
// room name becomes reclaimable and a finished call leaves no token behind. A
// reconnecting peer that still holds the invite link re-establishes the same
// token via roomJoinGate's claim path, so this does not break a creator's own
// reload (which momentarily empties the room).
func pruneRoomToken(projectDB *db.ProjectDB, ws *wsEndpointConfig, roomName string, tenantID int) {
	if ws == nil || !ws.RoomGated || roomName == "" {
		return
	}
	if _, err := projectDB.ExecWrite(
		"DELETE FROM of_ws_room_tokens WHERE endpoint_path = ? AND tenant_id = ? AND room = ?",
		ws.Path, tenantID, roomName,
	); err != nil {
		slog.Warn("ws room token prune failed", "endpoint", ws.Path, "room", roomName, "error", err)
	}
}
