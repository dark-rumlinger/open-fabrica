package validators

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// setupRawWSDB builds the of_ws_endpoints + of_files schema the raw-socket-vs-
// auth validator reads.
func setupRawWSDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_ws_endpoints (
		id INTEGER PRIMARY KEY, path TEXT, requires_auth INTEGER DEFAULT 0
	)`); err != nil {
		t.Fatalf("create of_ws_endpoints: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE of_files (
		id INTEGER PRIMARY KEY,
		kind TEXT, path TEXT, body TEXT DEFAULT '', scope TEXT,
		metadata TEXT, is_deleted INTEGER DEFAULT 0
	)`); err != nil {
		t.Fatalf("create of_files: %v", err)
	}
	return db
}

func saveWSEndpoint(t *testing.T, db *sql.DB, path string, requiresAuth bool) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, requires_auth) VALUES (?, ?)`, path, requiresAuth); err != nil {
		t.Fatalf("insert ws endpoint %s: %v", path, err)
	}
}

const rawSocketClient = `const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
const ws = new WebSocket(proto + '//' + location.host + '/api/game/ws?room=' + roomId);
ws.onmessage = (ev) => handle(JSON.parse(ev.data));`

const ofnetClient = `import { connect } from 'ofnet';
const net = connect("game", { room: roomId });
net.on("move", applyMove);
net.send({ type: "move", x: 1 });`

// The representative failure: a raw socket against a requires_auth endpoint 401s
// for every signed-in user (a browser socket cannot send an Authorization
// header) - the defect that leaves auth-gated multiplayer dead.
func TestRawWSAuth_FlagsRawSocketOnAuthEndpoint(t *testing.T) {
	db := setupRawWSDB(t)
	saveWSEndpoint(t, db, "game", true)
	savePageMeta(t, db, "file", "js/game.js", rawSocketClient, "")

	issues := rawWSAuthValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 {
		t.Fatalf("expected 1 raw_ws_auth issue, got %d: %+v", len(issues), issues)
	}
	if issues[0].Category != "raw_ws_auth" || issues[0].File != "js/game.js" {
		t.Errorf("unexpected issue: %+v", issues[0])
	}
	if issues[0].Severity != "error" || !issues[0].Deterministic {
		t.Errorf("raw_ws_auth must be a deterministic error: %+v", issues[0])
	}
}

// The fix-pass outcome: the same page connecting through the 'ofnet' helper is clean.
func TestRawWSAuth_CleanWhenOFNet(t *testing.T) {
	db := setupRawWSDB(t)
	saveWSEndpoint(t, db, "game", true)
	savePageMeta(t, db, "file", "js/game.js", ofnetClient, "")

	if issues := (rawWSAuthValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("ofnet-helper client must be clean, got: %+v", issues)
	}
}

// A raw socket against a PUBLIC endpoint is legal (documented in the create
// usage doc) and must not be flagged.
func TestRawWSAuth_CleanWhenEndpointPublic(t *testing.T) {
	db := setupRawWSDB(t)
	saveWSEndpoint(t, db, "game", false)
	savePageMeta(t, db, "file", "js/game.js", rawSocketClient, "")

	if issues := (rawWSAuthValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("raw socket on a public endpoint must be clean, got: %+v", issues)
	}
}

// js/ofnet.js itself constructs a raw socket; platform-managed files are the
// sanctioned exception and must never be flagged.
func TestRawWSAuth_SkipsPlatformManaged(t *testing.T) {
	db := setupRawWSDB(t)
	saveWSEndpoint(t, db, "game", true)
	savePageMeta(t, db, "file", "js/ofnet.js",
		`var ws = new WebSocket(proto + location.host + '/api/game/ws');`,
		`{"platform_managed": true}`)

	if issues := (rawWSAuthValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("platform-managed file must be skipped, got: %+v", issues)
	}
}

// A raw socket naming a DIFFERENT (public) endpoint's route must not be pinned
// on the auth-gated one.
func TestRawWSAuth_MatchesOnlyTheNamedEndpoint(t *testing.T) {
	db := setupRawWSDB(t)
	saveWSEndpoint(t, db, "game", true)
	saveWSEndpoint(t, db, "lobby", false)
	savePageMeta(t, db, "file", "js/lobby.js",
		`const ws = new WebSocket('wss://' + location.host + '/api/lobby/ws');`, "")

	if issues := (rawWSAuthValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("raw socket to the public endpoint must be clean, got: %+v", issues)
	}
}

// An inline <script> on a page body is caught too, not just .js modules.
func TestRawWSAuth_FlagsInlinePageScript(t *testing.T) {
	db := setupRawWSDB(t)
	saveWSEndpoint(t, db, "game", true)
	savePageMeta(t, db, "page", "/play",
		`<h1>Play</h1><script>const ws = new WebSocket('wss://x/api/game/ws');</script>`, "")

	issues := rawWSAuthValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 || issues[0].File != "/play" {
		t.Fatalf("expected 1 issue on /play, got: %+v", issues)
	}
}
