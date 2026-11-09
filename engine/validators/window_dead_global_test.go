package validators

// The dead-window-global validator closes the gap where a `window.__peerDist`-style
// global is read by one leaf and assigned by no file - a bridge
// that resolves to `undefined`, invisible to every other static gate (the root
// `window` is bound, so undefined_reference is blind; there is no import edge, so the
// module gates are blind; it lives in one file, so the cross-module bridge gate is
// blind).

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"openfabrica/store"
)

// deadGlobalDB builds an in-memory DB with the of_files and of_runs tables the
// validator (via jsAndPageFiles) and unboundRefSeverity read.
func deadGlobalDB(t *testing.T, planJSON string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE of_files (
			id INTEGER PRIMARY KEY,
			kind TEXT, path TEXT, body TEXT DEFAULT '', scope TEXT,
			metadata TEXT, is_deleted INTEGER DEFAULT 0
		)`,
		`CREATE TABLE of_runs (id INTEGER PRIMARY KEY, plan_json TEXT, started_at TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO of_runs (plan_json, started_at) VALUES (?, '2026-07-20')`, planJSON); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return db
}

func seedDeadGlobalFile(t *testing.T, db *sql.DB, path, body string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, body, metadata) VALUES ('file', ?, ?, '')`, path, body); err != nil {
		t.Fatalf("insert file %s: %v", path, err)
	}
}

func deadGlobalIssues(t *testing.T, db *sql.DB) []Issue {
	t.Helper()
	return windowDeadGlobalValidator{}.Validate(context.Background(), db, "", "")
}

// esmPlan declares three.js as an importmap ESM - it defines no ambient global, so a
// dead window read is deterministic (unboundRefSeverity returns error/true).
const esmPlan = `{"external_libraries":[{"name":"three.js","kind":"module","module_name":"three","url":"https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js"}]}`

// TestDeadWindowGlobal_ReadWithNoProducerIsFlagged: a global read in one file and
// written in none - the dead-global defect this validator exists to catch.
func TestDeadWindowGlobal_ReadWithNoProducerIsFlagged(t *testing.T) {
	db := deadGlobalDB(t, esmPlan)
	// chat reads window.__peerDist; remote (which owns peer positions) never assigns it.
	seedDeadGlobalFile(t, db, "world.chat.js", `export function isFar(id){ return window.__peerDist && window.__peerDist[id] > 40; }`)
	seedDeadGlobalFile(t, db, "world.remote.js", `export function initRemote(){ return 1; }`)

	issues := deadGlobalIssues(t, db)
	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1: %+v", len(issues), issues)
	}
	is := issues[0]
	if is.Category != "window_dead_global" || is.File != "world.chat.js" {
		t.Fatalf("wrong finding: category=%q file=%q", is.Category, is.File)
	}
	if is.Severity != store.SeverityError || !is.Deterministic {
		t.Errorf("a `__` dead read under an ESM-only plan must be error+deterministic (drives a fix-pass), got %q/%v", is.Severity, is.Deterministic)
	}
	if !strings.Contains(is.Message, "window.__peerDist") {
		t.Errorf("message must name the dead symbol; got %q", is.Message)
	}
}

// A produced global is wired, not dead - even when the producer is a different file.
func TestDeadWindowGlobal_ProducedElsewhereIsNotFlagged(t *testing.T) {
	db := deadGlobalDB(t, esmPlan)
	seedDeadGlobalFile(t, db, "world.chat.js", `export function isFar(id){ return window.__peerDist[id] > 40; }`)
	seedDeadGlobalFile(t, db, "world.remote.js", `window.__peerDist = {}; export function set(id,d){ window.__peerDist[id] = d; }`)
	if issues := deadGlobalIssues(t, db); len(issues) != 0 {
		t.Fatalf("a window global assigned by any file is not dead; got %+v", issues)
	}
}

// Platform-injected and browser/BOM reads are allowlisted and must never flag, even
// though no project file assigns them.
func TestDeadWindowGlobal_AllowlistedReadsNotFlagged(t *testing.T) {
	db := deadGlobalDB(t, esmPlan)
	seedDeadGlobalFile(t, db, "hud.js", `export function layout(){ return window.innerWidth + window.__routeParams.id + window.devicePixelRatio; }`)
	if issues := deadGlobalIssues(t, db); len(issues) != 0 {
		t.Fatalf("allowlisted window reads must not flag; got %+v", issues)
	}
}

// A BARE dead read might be an unnamed classic-library global, so with a UMD library
// declared it degrades to a warning (does not drive a fix-pass); a `__` dead read is
// never a library global and stays a hard error regardless.
func TestDeadWindowGlobal_SeverityFollowsLibraryAmbiguity(t *testing.T) {
	const umdPlan = `{"external_libraries":[{"name":"chart.js","url":"https://cdn.jsdelivr.net/npm/chart.js@4/dist/chart.umd.js"}]}`

	dbBare := deadGlobalDB(t, umdPlan)
	seedDeadGlobalFile(t, dbBare, "chart.js", `export function draw(){ return window.MysteryLib.render(); }`)
	bare := deadGlobalIssues(t, dbBare)
	if len(bare) != 1 || bare[0].Severity != store.SeverityWarning || bare[0].Deterministic {
		t.Fatalf("a bare dead read with a UMD library declared must be a non-deterministic warning, got %+v", bare)
	}

	dbUnderscore := deadGlobalDB(t, umdPlan)
	seedDeadGlobalFile(t, dbUnderscore, "chat.js", `export function isFar(id){ return window.__peerDist[id]; }`)
	us := deadGlobalIssues(t, dbUnderscore)
	if len(us) != 1 || us[0].Severity != store.SeverityError || !us[0].Deterministic {
		t.Fatalf("a `__` dead read is never a library global and must stay error+deterministic even with a UMD library, got %+v", us)
	}
}

// A bare dead read may be a declared library's global with a missing or wrong
// global_name, so the message leads with the declaration fix and tells the model
// not to change the code; a `__` read cannot be a library global and gets the
// plain export/import remedy only.
func TestDeadWindowGlobal_BareReadLeadsWithLibraryCheck(t *testing.T) {
	plan := `{"external_libraries":[{"name":"qr","url":"https://cdn.example/qr.js","global_name":"OldQR"}]}`
	db := deadGlobalDB(t, plan)
	seedDeadGlobalFile(t, db, "app.result.js", `export function draw(el){ return window.qrcode(4, 'M'); }`)
	issues := deadGlobalIssues(t, db)
	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1: %+v", len(issues), issues)
	}
	msg := issues[0].Message
	lib := strings.Index(msg, "FIRST rule out a library global")
	refactor := strings.Index(msg, "Move each symbol")
	if lib < 0 || refactor < 0 || lib > refactor {
		t.Fatalf("the library check must come before the refactor advice: %q", msg)
	}
	if !strings.Contains(msg, "global_name") || !strings.Contains(msg, "must NOT be changed") {
		t.Errorf("message must steer to the declaration fix and forbid changing the code: %q", msg)
	}

	db = deadGlobalDB(t, esmPlan)
	seedDeadGlobalFile(t, db, "world.chat.js", `export function isFar(id){ return window.__peerDist[id] > 40; }`)
	issues = deadGlobalIssues(t, db)
	if len(issues) != 1 || strings.Contains(issues[0].Message, "FIRST rule out a library global") {
		t.Fatalf("a `__` read is never a library global: %+v", issues)
	}
}
