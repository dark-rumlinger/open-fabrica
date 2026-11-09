package snapshotbuild

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// mustExec runs a write against the in-memory test DB, failing the test on error.
func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec failed: %v\nquery: %s", err, query)
	}
}

// setupSnapshotDB builds the minimal schema BuildProjectSnapshot and
// store.UnlinkedPageArtifacts read: of_files (with metadata + content_type)
// and of_page_assets. Endpoint/table queries tolerate their tables being absent.
func setupSnapshotDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_files (
		id INTEGER PRIMARY KEY,
		kind TEXT, path TEXT, scope TEXT, body TEXT, content_type TEXT,
		metadata TEXT DEFAULT '{}', is_deleted INTEGER DEFAULT 0
	)`); err != nil {
		t.Fatalf("create of_files: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE of_page_assets (page_path TEXT, file_path TEXT, position INTEGER DEFAULT 0)`); err != nil {
		t.Fatalf("create of_page_assets: %v", err)
	}
	return db
}

// Declared external libraries must surface in the snapshot (the model's ground
// truth) so it neither re-declares one nor hand-writes its tags, and a change to
// the library set must move the fingerprint so the snapshot re-renders.
func TestSnapshot_LibrariesRenderAndFingerprint(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `CREATE TABLE of_runs (id TEXT PRIMARY KEY, plan_json TEXT, started_at DATETIME DEFAULT CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO of_runs (id, plan_json) VALUES ('run_1', ?)`,
		`{"external_libraries":[{"name":"Press Start 2P","url":"https://fonts.example/press.css"}]}`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	if len(s.Libraries) != 1 || s.Libraries[0].Name != "Press Start 2P" {
		t.Fatalf("expected the declared library in the snapshot, got %+v", s.Libraries)
	}
	out := s.Render()
	if !strings.Contains(out, "### Libraries (1)") || !strings.Contains(out, "Press Start 2P") {
		t.Fatalf("Render must list declared libraries:\n%s", out)
	}
	fpWith := s.Fingerprint()

	// Removing the library must change the fingerprint so the snapshot re-renders.
	mustExec(t, db, `UPDATE of_runs SET plan_json = ? WHERE id = 'run_1'`, `{"external_libraries":[]}`)
	s2, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot (2): %v", err)
	}
	if len(s2.Libraries) != 0 {
		t.Fatalf("expected no libraries after removal, got %+v", s2.Libraries)
	}
	if s2.Fingerprint() == fpWith {
		t.Fatal("fingerprint must change when the library set changes")
	}
}

// Views (create_view) live in sqlite_master, not of_dynamic_tables. Without a
// section of their own the model sees only the read endpoint layered on top and
// cannot reconcile a task declaring the view name with project state. They must
// surface in the snapshot: rendered in their own section, columns resolved,
// tracked as a fingerprinted entity, and internal of_ views excluded.
func TestSnapshot_ViewsRenderAndFingerprint(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `CREATE VIEW metrics_overview AS SELECT 1 AS total_users, 2.5 AS revenue_mtd`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	if len(s.Views) != 1 || s.Views[0].Name != "metrics_overview" {
		t.Fatalf("expected the view in the snapshot, got %+v", s.Views)
	}
	if !strings.Contains(strings.Join(s.Views[0].Columns, ","), "total_users") {
		t.Fatalf("view columns must be resolved, got %+v", s.Views[0].Columns)
	}
	out := s.Render()
	if !strings.Contains(out, "### Views (1)") || !strings.Contains(out, "metrics_overview") {
		t.Fatalf("Render must list views:\n%s", out)
	}
	fpWith := s.Fingerprint()

	// Internal of_ views must never leak into the model-facing snapshot.
	mustExec(t, db, `CREATE VIEW of_internal AS SELECT 1 AS x`)
	s2, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot (2): %v", err)
	}
	if len(s2.Views) != 1 {
		t.Fatalf("of_ views must be excluded, got %+v", s2.Views)
	}

	// Dropping the user view must move the fingerprint so the snapshot re-renders.
	mustExec(t, db, `DROP VIEW metrics_overview`)
	s3, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot (3): %v", err)
	}
	if len(s3.Views) != 0 {
		t.Fatalf("expected no user views after drop, got %+v", s3.Views)
	}
	if s3.Fingerprint() == fpWith {
		t.Fatal("fingerprint must change when the view set changes")
	}
}

// A module reached only through a linked entry's import graph must NOT be flagged
// UNLINKED: ServeFile serves it by URL and the browser fetches it via the entry's
// import, so the page boots with only the entry linked. This guards against a
// naive set-intersection that would flag every not-directly-linked module and
// force the model to link each file one by one.
func TestSnapshot_ImportedModuleNotFlaggedUnlinked(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type, metadata) VALUES
		('page', '/', 'page', '<html></html>', 'text/html', '{"title":"Crime City"}'),
		('file', 'js/main.js',  'page', 'import { setup } from "./input.js"; import "./city.js";', 'text/javascript', '{"is_module":true}'),
		('file', 'js/input.js', 'page', 'export function setup(){}', 'text/javascript', '{"is_module":true}'),
		('file', 'js/city.js',  'page', 'export const city = {};', 'text/javascript', '{"is_module":true}')`)
	// Only the entry module is linked. input.js and city.js are reached via import.
	mustExec(t, db, `INSERT INTO of_page_assets (page_path, file_path) VALUES ('/', 'js/main.js')`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	if len(s.UnlinkedPageFiles) != 0 {
		t.Fatalf("imported modules should not be unlinked, got %v", s.UnlinkedPageFiles)
	}
	out := s.Render()
	if strings.Contains(out, "Unlinked page-scope artifacts") {
		t.Fatalf("snapshot must not warn about reachable modules:\n%s", out)
	}
	if strings.Contains(out, "UNLINKED") {
		t.Fatalf("reachable modules must not be tagged UNLINKED:\n%s", out)
	}
}

// TestSnapshot_ModuleContracts pins the per-module export/import surface the
// snapshot carries: each JS module renders under its bare import specifier (the
// one style the platform teaches), listing named exports and project-internal
// imports - BOTH bare ('state') and relative ('./state.js') specifiers resolve -
// while a bare EXTERNAL library import is not a cross-module concern and is
// omitted.
func TestSnapshot_ModuleContracts(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type, metadata) VALUES
		('page', '/', 'page', '<html></html>', 'text/html', '{"title":"Sheet"}'),
		('file', 'js/state.js', 'page', 'export function createCellKey(col, row){}`+"\n"+`export const TOTAL = 1;', 'text/javascript', '{"is_module":true}'),
		('file', 'js/parser.js', 'page', 'import { createCellKey } from "./state.js";`+"\n"+`import * as math from "mathlib";`+"\n"+`export function parseFormula(){ return createCellKey(); }', 'text/javascript', '{"is_module":true}'),
		('file', 'js/grid.js', 'page', 'import { TOTAL } from "state";`+"\n"+`export function rows(){ return TOTAL; }', 'text/javascript', '{"is_module":true}')`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}

	byPath := map[string]ModuleContract{}
	for _, m := range s.Modules {
		byPath[m.Path] = m
	}
	if got := strings.Join(byPath["js/state.js"].Exports, ","); got != "createCellKey,TOTAL" {
		t.Errorf("state.js exports = %q, want createCellKey,TOTAL", got)
	}
	if byPath["js/state.js"].Bare != "state" {
		t.Errorf("state.js Bare = %q, want state", byPath["js/state.js"].Bare)
	}
	parser := byPath["js/parser.js"]
	if len(parser.Imports) != 1 || parser.Imports[0].Spec != "./state.js" ||
		strings.Join(parser.Imports[0].Names, ",") != "createCellKey" {
		t.Errorf("parser.js imports = %+v, want one {./state.js: createCellKey} (external lib import filtered)", parser.Imports)
	}
	// A bare PROJECT-module import is part of the cross-module contract and must
	// survive the external-library filter.
	grid := byPath["js/grid.js"]
	if len(grid.Imports) != 1 || grid.Imports[0].Spec != "state" ||
		strings.Join(grid.Imports[0].Names, ",") != "TOTAL" {
		t.Errorf("grid.js imports = %+v, want one {state: TOTAL}", grid.Imports)
	}

	out := s.Render()
	if !strings.Contains(out, "### Module contracts") {
		t.Fatalf("Render missing Module contracts section:\n%s", out)
	}
	if !strings.Contains(out, "`state` (js/state.js) exports: createCellKey(col, row), TOTAL") {
		t.Errorf("Render missing bare-named signature-aware state.js exports line:\n%s", out)
	}
	if params, ok := byPath["js/state.js"].Sigs["createCellKey"]; !ok || strings.Join(params, ",") != "col,row" {
		t.Errorf("state.js Sigs[createCellKey] = %v (ok=%v), want [col row]", params, ok)
	}
	if !strings.Contains(out, "imports { createCellKey } from ./state.js") {
		t.Errorf("Render missing parser.js import line:\n%s", out)
	}
	if !strings.Contains(out, "imports { TOTAL } from state") {
		t.Errorf("Render missing grid.js bare import line:\n%s", out)
	}
	if strings.Contains(out, "mathlib") {
		t.Errorf("an external library import must not appear in module contracts:\n%s", out)
	}
}

// TestSnapshot_ModuleDataShapes pins the DATA contract the snapshot now carries: the
// field keys of an exported object literal (a shared-state leaf, a factory's returned
// frame) are surfaced under a `shapes:` line so a producer and consumer agree on the exact
// keys - the drift a symbol-only contract cannot catch (`{yaw}` written / `.y` read). A
// module that exports no object literal has no shapes line.
func TestSnapshot_ModuleDataShapes(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type, metadata) VALUES
		('page', '/', 'page', '<html></html>', 'text/html', '{"title":"Arena"}'),
		('file', 'js/arena.state.js', 'page', 'export const state = { players: {}, myId: null };`+"\n"+`export function packState(){ return { x: 0, y: 0, yaw: 0 }; }', 'text/javascript', '{"is_module":true}'),
		('file', 'js/arena.net.js', 'page', 'import { packState } from "./arena.state.js";`+"\n"+`export function send(){ return packState(); }', 'text/javascript', '{"is_module":true}')`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}

	byPath := map[string]ModuleContract{}
	for _, m := range s.Modules {
		byPath[m.Path] = m
	}
	state := byPath["js/arena.state.js"]
	if got := strings.Join(state.Shapes["state"], ","); got != "players,myId" {
		t.Errorf("state.js Shapes[state] = %q, want players,myId", got)
	}
	if got := strings.Join(state.Shapes["packState"], ","); got != "x,y,yaw" {
		t.Errorf("state.js Shapes[packState] = %q, want x,y,yaw", got)
	}
	if len(byPath["js/arena.net.js"].Shapes) != 0 {
		t.Errorf("net.js exports no object literal, want no shapes, got %v", byPath["js/arena.net.js"].Shapes)
	}

	out := s.Render()
	if !strings.Contains(out, "shapes: packState{ x, y, yaw }, state{ players, myId }") {
		t.Errorf("Render missing data-shapes line for arena.state.js:\n%s", out)
	}
}

// TestSnapshot_StyleContract pins the design-system contract: the global
// stylesheet's tokens and class names are surfaced (so they survive a context
// rollover and pages reuse them), while a page-scope stylesheet is excluded (it
// would bloat the contract with page count and surface one-offs).
func TestSnapshot_StyleContract(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type, metadata) VALUES
		('page', '/', 'page', '<html></html>', 'text/html', '{"title":"Blog"}'),
		('file', 'css/app.css', 'global', ':root{--color-bg:#fff;--space-3:12px}.btn{color:var(--color-bg)}.card{padding:var(--space-3)}@keyframes fade-in{from{opacity:0}}', 'text/css', '{}'),
		('file', 'css/post.css', 'page', '.post-only-widget{display:flex}', 'text/css', '{}')`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	if len(s.Styles) != 1 || s.Styles[0].Path != "css/app.css" {
		t.Fatalf("expected only the global stylesheet in Styles, got %+v", s.Styles)
	}
	sc := s.Styles[0]
	if strings.Join(sc.Vars, ",") != "--color-bg,--space-3" {
		t.Errorf("tokens = %v", sc.Vars)
	}
	if strings.Join(sc.Classes, ",") != "btn,card" {
		t.Errorf("classes = %v", sc.Classes)
	}
	if strings.Join(sc.Keyframes, ",") != "fade-in" {
		t.Errorf("keyframes = %v", sc.Keyframes)
	}

	out := s.Render()
	if !strings.Contains(out, "### Design system (shared CSS) (1)") {
		t.Fatalf("Render missing Design system section:\n%s", out)
	}
	if !strings.Contains(out, "tokens: --color-bg: #fff; --space-3: 12px") || !strings.Contains(out, "classes: btn, card") {
		t.Fatalf("Render missing token/class lines:\n%s", out)
	}
	// A page-scope stylesheet's classes must NOT appear in the design-system contract.
	if strings.Contains(out, "post-only-widget") {
		t.Fatalf("page-scope CSS must not leak into the design system contract:\n%s", out)
	}
}

// The contract is names-only: adding a class changes the fingerprint and surfaces
// in the delta, but a value-only edit (same names) does not churn the cache, and
// deleting the global stylesheet drops the entity.
func TestSnapshot_StyleContractFingerprint(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type, metadata) VALUES
		('file', 'css/app.css', 'global', ':root{--c:#fff}.btn{color:var(--c)}', 'text/css', '{}')`)

	s1, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	fp1 := s1.Fingerprint()

	// A non-token declaration edit must NOT move the fingerprint: class bodies are
	// still not carried.
	mustExec(t, db, `UPDATE of_files SET body = ':root{--c:#fff}.btn{color:var(--c);padding:8px}' WHERE path = 'css/app.css'`)
	sPad, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot (pad): %v", err)
	}
	if sPad.Fingerprint() != fp1 {
		t.Fatalf("a class-body edit must not change the fingerprint")
	}

	// A TOKEN VALUE edit must move it: the snapshot carries token values, so a
	// stale one would hand the next page the wrong colour.
	mustExec(t, db, `UPDATE of_files SET body = ':root{--c:#000}.btn{color:var(--c);padding:8px}' WHERE path = 'css/app.css'`)
	s2, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot (2): %v", err)
	}
	if s2.Fingerprint() == fp1 {
		t.Fatalf("a token value edit must change the fingerprint (values are carried)")
	}
	if delta := s2.RenderDelta(s1.Entities, s1); !strings.Contains(delta, "--c: #000") {
		t.Fatalf("token delta must show the new value, got:\n%s", delta)
	}

	// Adding a class must move the fingerprint and surface in the delta.
	mustExec(t, db, `UPDATE of_files SET body = ':root{--c:#000}.btn{color:var(--c)}.alert{color:red}' WHERE path = 'css/app.css'`)
	s3, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot (3): %v", err)
	}
	if s3.Fingerprint() == fp1 {
		t.Fatalf("adding a class must change the fingerprint")
	}
	if delta := s3.RenderDelta(s2.Entities, s2); !strings.Contains(delta, "alert") {
		t.Fatalf("delta must surface the new class:\n%s", delta)
	}

	// Deleting the global stylesheet drops the style entity.
	mustExec(t, db, `UPDATE of_files SET is_deleted = 1 WHERE path = 'css/app.css'`)
	s4, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot (4): %v", err)
	}
	if len(s4.Styles) != 0 {
		t.Fatalf("deleting the global CSS must drop the style contract, got %+v", s4.Styles)
	}
	if _, ok := s4.Entities["style:css/app.css"]; ok {
		t.Fatalf("removed stylesheet must not remain an entity")
	}
}

// A page-scope file no linked module imports (orphan JS) and CSS (which an ES
// module cannot pull in) must still be flagged so the page isn't shipped without
// its script/styles.
func TestSnapshot_OrphanAndCSSStillFlagged(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type, metadata) VALUES
		('page', '/', 'page', '<html></html>', 'text/html', '{"title":"Home"}'),
		('file', 'js/main.js',   'page', 'import "./input.js";', 'text/javascript', '{"is_module":true}'),
		('file', 'js/input.js',  'page', 'export function setup(){}', 'text/javascript', '{"is_module":true}'),
		('file', 'js/orphan.js', 'page', 'export const dead = 1;', 'text/javascript', '{"is_module":true}'),
		('file', 'css/app.css',  'page', 'body{}', 'text/css', '{}')`)
	mustExec(t, db, `INSERT INTO of_page_assets (page_path, file_path) VALUES ('/', 'js/main.js')`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	got := strings.Join(s.UnlinkedPageFiles, ",")
	if got != "css/app.css,js/orphan.js" {
		t.Fatalf("expected css/app.css and js/orphan.js flagged, got %q", got)
	}
	out := s.Render()
	if !strings.Contains(out, "css/app.css") || !strings.Contains(out, "js/orphan.js") {
		t.Fatalf("orphan + CSS must appear in warning:\n%s", out)
	}
	if strings.Contains(out, "js/input.js`, call") {
		t.Fatalf("imported input.js must not be in the unlinked warning list:\n%s", out)
	}
}

// The Page DOM anchors surface: a JS-carrying page's declared element ids (its own
// markup + the shared layout + included components) are surfaced so a script binds
// to what the page provides instead of inventing a container; a page with no script
// is excluded; and adding an id moves the fingerprint so the snapshot re-renders.
func TestSnapshot_PageDOMAnchors(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES
		('page','/','', '<div id="gameView"><div id="chatPanel"><div id="chatLog"></div></div>{{include:hud}}</div>', 'text/html'),
		('layout','default','', '<html><body id="appRoot">{{content}}</body></html>', 'text/html'),
		('component','hud','', '<div id="notifStack"></div>', 'text/html'),
		('file','town.js','page','export const x = 1;', 'application/javascript'),
		('page','/about','', '<div id="aboutBox">no script here</div>', 'text/html')`)
	mustExec(t, db, `INSERT INTO of_page_assets (page_path, file_path) VALUES ('/','town.js')`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	if len(s.PageDOM) != 1 || s.PageDOM[0].Path != "/" {
		t.Fatalf("PageDOM = %+v, want exactly one contract for '/'", s.PageDOM)
	}
	ids := strings.Join(s.PageDOM[0].IDs, ",")
	for _, want := range []string{"appRoot", "chatLog", "chatPanel", "gameView", "notifStack"} {
		if !strings.Contains(ids, want) {
			t.Errorf("DOM contract missing %q (page + layout + component ids), got: %s", want, ids)
		}
	}
	r := s.Render()
	if !strings.Contains(r, "### Page DOM anchors") || !strings.Contains(r, "chatPanel") {
		t.Errorf("Render missing the Page DOM anchors section:\n%s", r)
	}
	if strings.Contains(r, "aboutBox") {
		t.Errorf("a page with no page-scope script must not appear in DOM anchors:\n%s", r)
	}

	fp1 := s.Fingerprint()
	mustExec(t, db, `UPDATE of_files SET body = '<div id="gameView"><div id="chatPanel"></div><div id="hudLayer"></div>{{include:hud}}</div>' WHERE kind='page' AND path='/'`)
	s2, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot (2): %v", err)
	}
	if s2.Fingerprint() == fp1 {
		t.Errorf("adding an element id must move the snapshot fingerprint (so the snapshot re-renders)")
	}
	if !strings.Contains(strings.Join(s2.PageDOM[0].IDs, ","), "hudLayer") {
		t.Errorf("the newly added id hudLayer should appear in the DOM contract")
	}
}
