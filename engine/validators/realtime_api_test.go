package validators

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"openfabrica/db"
)

func setupRealtimeAPIDB(t *testing.T) *sql.DB {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	return pdb.Writer()
}

func addEndpoint(t *testing.T, q *sql.DB, path, model string, tickHz int, matchmaking, roomGated bool) {
	t.Helper()
	if _, err := q.Exec(`INSERT INTO of_ws_endpoints (path, model, tick_hz, matchmaking, room_gated) VALUES (?, ?, ?, ?, ?)`,
		path, model, tickHz, matchmaking, roomGated); err != nil {
		t.Fatalf("insert endpoint %s: %v", path, err)
	}
}

func addJS(t *testing.T, q *sql.DB, path, body string) {
	t.Helper()
	if _, err := q.Exec(`INSERT INTO of_files (kind, path, body, scope) VALUES ('file', ?, ?, 'global')`, path, body); err != nil {
		t.Fatalf("insert file %s: %v", path, err)
	}
}

func realtimeAPIIssues(t *testing.T, q *sql.DB) []Issue {
	t.Helper()
	return realtimeAPIValidator{}.Validate(context.Background(), q, "", "")
}

func TestRealtimeAPI_ActionOnTickEndpointFlags(t *testing.T) {
	q := setupRealtimeAPIDB(t)
	addEndpoint(t, q, "arena", "tick", 30, false, false)
	addJS(t, q, "/files/game.js", `import { connect } from 'ofnet';
const net = connect("arena", { room });
net.onState((s) => render(s));
net.action("move", { x: 1 });`)
	issues := realtimeAPIIssues(t, q)
	if len(issues) != 1 || !strings.Contains(issues[0].Message, "manage_authoritative") {
		t.Fatalf("net.action/onState on a tick endpoint must flag once, got %+v", issues)
	}
}

func TestRealtimeAPI_OnMatchedOnRelayFlags(t *testing.T) {
	q := setupRealtimeAPIDB(t)
	addEndpoint(t, q, "lobby", "relay", 0, false, false)
	addJS(t, q, "/files/lobby.js", `const net = connect("lobby", { room: "lobby" }); net.onMatched((m) => go(m));`)
	if issues := realtimeAPIIssues(t, q); len(issues) != 1 || !strings.Contains(issues[0].Message, "manage_matchmaking") {
		t.Fatalf("onMatched on a relay must flag once, got %+v", issues)
	}
}

func TestRealtimeAPI_CorrectModelsPass(t *testing.T) {
	q := setupRealtimeAPIDB(t)
	addEndpoint(t, q, "game", "authoritative", 0, false, false)
	addEndpoint(t, q, "lobby", "matchmaking", 0, true, false)
	addJS(t, q, "/files/game.js", `const net = connect("game", { room }); net.onState(render); net.action("play", { cell: 4 });`)
	addJS(t, q, "/files/lobby.js", `connect("lobby", { room: "lobby" }).onMatched((m) => start(m));`)
	// Two literal endpoints in one file (the matchmaking hand-off): not attributable, skipped.
	addJS(t, q, "/files/handoff.js", `connect("lobby", { room: "lobby" }).onMatched((m) => { const g = connect("game", { room: m._room }); g.onState(render); });`)
	if issues := realtimeAPIIssues(t, q); len(issues) != 0 {
		t.Fatalf("correct model usage must not flag, got %+v", issues)
	}
}

func TestRealtimeAPI_RawSocketToGatedRoomFlags(t *testing.T) {
	q := setupRealtimeAPIDB(t)
	addEndpoint(t, q, "call", "signaling", 0, false, true)
	addJS(t, q, "/files/call.js", `const ws = new WebSocket(proto + '//' + location.host + '/api/call/ws?room=' + id);`)
	if issues := realtimeAPIIssues(t, q); len(issues) != 1 || !strings.Contains(issues[0].Message, "room_gated") {
		t.Fatalf("a raw socket to a room_gated endpoint must flag once, got %+v", issues)
	}
}
