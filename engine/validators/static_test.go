package validators

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"openfabrica/store"

	_ "modernc.org/sqlite"
)

// setupStaticDB builds an of_files schema with the columns store.ArtifactList
// reads (it scans storage/body/metadata/etc., not just the link checker's subset).
func setupStaticDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_files (
		id INTEGER PRIMARY KEY,
		kind TEXT, path TEXT, storage TEXT DEFAULT 'inline', body TEXT DEFAULT '',
		disk_path TEXT DEFAULT '', content_type TEXT DEFAULT '', metadata TEXT DEFAULT '{}',
		scope TEXT DEFAULT 'page', size INTEGER DEFAULT 0, status TEXT DEFAULT 'published',
		is_deleted INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func saveCodeFile(t *testing.T, db *sql.DB, path, body string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO of_files (kind, path, storage, body, scope) VALUES ('file', ?, 'inline', ?, 'page')`,
		path, body); err != nil {
		t.Fatalf("insert %s: %v", path, err)
	}
}

func savePage(t *testing.T, db *sql.DB, path, body string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO of_files (kind, path, storage, body, scope) VALUES ('page', ?, 'inline', ?, 'page')`,
		path, body); err != nil {
		t.Fatalf("insert %s: %v", path, err)
	}
}

func staticIssues(t *testing.T, db *sql.DB) []Issue {
	t.Helper()
	return staticCodeValidator{}.Validate(context.Background(), db, "", "")
}

func hasIssue(issues []Issue, category, file string) bool {
	for _, is := range issues {
		if is.Category == category && is.File == file {
			return true
		}
	}
	return false
}

func anyIssueForFile(issues []Issue, file string) bool {
	for _, is := range issues {
		if is.File == file {
			return true
		}
	}
	return false
}

func TestStaticCode_FlagsJSSyntaxError(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "files/broken.js", `export function f() {`) // unclosed brace
	saveCodeFile(t, db, "files/ok.js", `export const x = 1;`)

	issues := staticIssues(t, db)
	if !hasIssue(issues, "js_syntax", "files/broken.js") {
		t.Fatalf("expected js_syntax error for files/broken.js, got %+v", issues)
	}
	if anyIssueForFile(issues, "files/ok.js") {
		t.Fatalf("a clean file must not be flagged, got %+v", issues)
	}
}

func TestStaticCode_FlagsCSSBraceImbalance(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "css/site.css", `.a{ color:red;`) // missing closing brace
	if !hasIssue(staticIssues(t, db), "css_syntax", "css/site.css") {
		t.Fatalf("expected css_syntax error for css/site.css")
	}
}

func TestStaticCode_FlagsUnresolvedNamedImport(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "files/util.js", `export const present = 1;`)           // exports 'present', not 'missing'
	saveCodeFile(t, db, "files/app.js", `import { missing } from './util.js';`) // names an export that does not exist

	if !hasIssue(staticIssues(t, db), "module_imports", "files/app.js") {
		t.Fatalf("expected module_imports error for files/app.js (imports a name util.js never exports)")
	}
}

func TestStaticCode_CleanProjectPasses(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "files/util.js", `export const present = 1;`)
	saveCodeFile(t, db, "files/app.js", `import { present } from './util.js'; console.log(present);`)
	saveCodeFile(t, db, "css/site.css", `.a{ color:red; }`)

	if issues := staticIssues(t, db); len(issues) != 0 {
		t.Fatalf("a clean project must produce no static issues, got %+v", issues)
	}
}

// The whole-graph backstop: a cyclic import graph (entry imports the leaf, leaf
// imports a service back from the entry) is a deterministic error that drives a
// fix-pass, even though every name resolves and the page would still boot.
func TestStaticCode_FlagsModuleCycle(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "files/game.js", `import './game.tool.js'; export function register(s){} export function getScene(){ return {}; }`)
	saveCodeFile(t, db, "files/game.tool.js", `import { register, getScene } from './game.js'; export function beginNewGame(){ register('tool'); return getScene(); }`)

	issues := staticIssues(t, db)
	if !hasIssue(issues, "module_cycle", "files/game.js") {
		t.Fatalf("expected module_cycle error naming the smallest cycle member, got %+v", issues)
	}
	for _, is := range issues {
		if is.Category == "module_cycle" {
			if is.Severity != "warning" || !is.Deterministic {
				t.Fatalf("module_cycle is a deterministic warning (the page boots; the click-through reports wiring it kills): %+v", is)
			}
			if !strings.Contains(is.Message, "import cycle") {
				t.Errorf("module_cycle message should name the cycle, got: %s", is.Message)
			}
		}
	}
}

// The correct one-directional shape (entry -> leaf -> zero-import core leaf)
// produces no graph issue.
func TestStaticCode_AcyclicTreeNoCycleIssue(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "files/game.core.js", `export function register(s){} export function getScene(){ return {}; }`)
	saveCodeFile(t, db, "files/game.js", `import './game.tool.js';`)
	saveCodeFile(t, db, "files/game.tool.js", `import { register, getScene } from './game.core.js'; export function beginNewGame(){ register('tool'); return getScene(); }`)

	for _, is := range staticIssues(t, db) {
		if is.Category == "module_cycle" {
			t.Fatalf("an acyclic tree must produce no module_cycle issue, got %+v", is)
		}
	}
}

func hasIssuePage(issues []Issue, category, page string) bool {
	for _, is := range issues {
		if is.Category == category && is.Page == page {
			return true
		}
	}
	return false
}

// A page carries its JS in inline <script> blocks, which the path-keyed file
// checks never parse. The build-end gate parses them too, so a syntax error in an
// inline script is a deterministic js_syntax error keyed to the page - matching
// the save-time inlineScriptSyntax gate via the shared analysis.InlineScripts.
func TestStaticCode_FlagsInlineScriptSyntaxError(t *testing.T) {
	db := setupStaticDB(t)
	savePage(t, db, "/", "<main>\n<script>\n  const x = ;\n</script>\n</main>")
	savePage(t, db, "/ok", "<main><script>const y = 1;</script></main>")

	issues := staticIssues(t, db)
	if !hasIssuePage(issues, "js_syntax", "/") {
		t.Fatalf("expected a js_syntax error for the inline <script> in '/', got %+v", issues)
	}
	for _, is := range issues {
		if is.Page == "/ok" {
			t.Fatalf("a page with a clean inline script must not be flagged, got %+v", is)
		}
		if is.Category == "js_syntax" && is.Page == "/" {
			if is.Severity != "error" || !is.Deterministic {
				t.Fatalf("an inline-script js_syntax issue must be a deterministic error so it drives a fix-pass: %+v", is)
			}
		}
	}
}

// module_specifier: a bare import resolving to neither a project module nor a
// declared importmap library is the runtime "Failed to resolve module specifier"
// that esbuild and the page probe miss (the simplex-noise live-build failure).
// The validator must flag it against the plan's declared libraries, and stay
// silent once the library is declared.
func TestStaticCode_UnresolvedBareSpecifierFlagged(t *testing.T) {
	db := setupStaticDB(t)
	// of_runs carries the plan whose external_libraries define the importmap.
	if _, err := db.Exec(`CREATE TABLE of_runs (id INTEGER PRIMARY KEY, plan_json TEXT, started_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("of_runs schema: %v", err)
	}
	setPlan := func(planJSON string) {
		if _, err := db.Exec(`DELETE FROM of_runs`); err != nil {
			t.Fatalf("clear of_runs: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO of_runs (plan_json) VALUES (?)`, planJSON); err != nil {
			t.Fatalf("insert run: %v", err)
		}
	}

	saveCodeFile(t, db, "files/game.state.js", `export const state = {};`)
	saveCodeFile(t, db, "files/game.planet.js", `import { createNoise2D } from 'simplex-noise';
import { state } from './game.state.js';
export function gen() { return createNoise2D(); }`)

	// three declared, simplex-noise NOT -> the planet leaf's bare import is unresolved.
	setPlan(`{"external_libraries":[{"name":"three.js","kind":"module","module_name":"three","url":"https://cdn/three.module.js"}]}`)
	if !hasIssue(staticIssues(t, db), "module_specifier", "files/game.planet.js") {
		t.Fatalf("an undeclared bare specifier must be flagged as module_specifier; got %+v", staticIssues(t, db))
	}

	// Declare simplex-noise as a module -> no module_specifier finding.
	setPlan(`{"external_libraries":[
		{"name":"three.js","kind":"module","module_name":"three","url":"https://cdn/three.module.js"},
		{"name":"simplex-noise","kind":"module","module_name":"simplex-noise","url":"https://cdn/npm/simplex-noise/dist/esm/simplex-noise.js"}]}`)
	for _, is := range staticIssues(t, db) {
		if is.Category == "module_specifier" {
			t.Fatalf("a declared module library must not be flagged, got %+v", is)
		}
	}
}

// The two shapes a call-position scan cannot see, each reported as a non-looping
// warning: world.js reads a name it never imported, having taken only the setter
// its state leaf exports beside the live binding; entry.js writes a name it never
// declared. Both parse, link, and load clean, since neither reference runs on
// load.
func TestStaticCode_FlagsUnboundMemberAndAssignment(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "files/state.js", `export let net = null;
export function setNet(n){ net = n; }
export let scene = null;
export function setEngine(h){ scene = h.scene; }`)
	saveCodeFile(t, db, "files/world.js", `import { setNet } from './state.js';
export function sendTick(dt){ net.send({ dt: dt }); }`)
	saveCodeFile(t, db, "files/entry.js", `import { setEngine } from './state.js';
function build(scene){ return scene; }
export function boot(){ const local = build({}); setEngine({ scene: local }); scene = local; }`)

	issues := staticIssues(t, db)
	for _, f := range []string{"files/world.js", "files/entry.js"} {
		if !hasIssue(issues, "undefined_reference", f) {
			t.Fatalf("expected undefined_reference for %s, got %+v", f, issues)
		}
	}
	// The leaf declares every name it assigns, so it must stay clean.
	if anyIssueForFile(issues, "files/state.js") {
		t.Fatalf("the state leaf declares what it assigns and must not be flagged, got %+v", issues)
	}
	for _, is := range issues {
		if is.Category != "undefined_reference" {
			continue
		}
		if is.Severity != store.SeverityWarning || is.Deterministic {
			t.Fatalf("a member/assignment finding must be a non-looping warning: %+v", is)
		}
		switch is.File {
		case "files/world.js":
			if !strings.Contains(is.Message, "`net`") || !strings.Contains(is.Message, "import { net } from 'state';") {
				t.Errorf("world.js message must steer to the live binding it skipped: %s", is.Message)
			}
		case "files/entry.js":
			if !strings.Contains(is.Message, "Declare `let scene;`") || !strings.Contains(is.Message, "setter") {
				t.Errorf("entry.js message must steer to a declaration, not an import: %s", is.Message)
			}
		}
	}
}

// undefined_reference: objects.js calls getEventPos, a PRIVATE helper defined (not
// exported) in paint.js and never imported. Every import resolves and the page
// loads clean, so module_imports and the runtime probe both miss it; the
// source-level backstop must flag it and steer to the private definer.
func TestStaticCode_FlagsUndefinedReference(t *testing.T) {
	db := setupStaticDB(t)
	// With a readable plan that declares no library, no ambient global could supply the
	// name, so the answer is certain; without one it is only a warning.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS of_runs (id TEXT PRIMARY KEY, plan_json TEXT, started_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("runs schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_runs (id, plan_json) VALUES ('run_1', '{"external_libraries":[]}')`); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	saveCodeFile(t, db, "files/canvas.js", `export function screenToDoc(x, y){ return { x: x, y: y }; }`)
	saveCodeFile(t, db, "files/paint.js", `import { screenToDoc } from './canvas.js';
function getEventPos(e){ return screenToDoc(e.clientX, e.clientY); }
export function brushDown(e){ return getEventPos(e); }`)
	saveCodeFile(t, db, "files/objects.js", `import { screenToDoc } from './canvas.js';
export function moveMove(e){ var p = getEventPos(e); return p; }`)

	issues := staticIssues(t, db)
	if !hasIssue(issues, "undefined_reference", "files/objects.js") {
		t.Fatalf("expected undefined_reference for files/objects.js (calls a sibling's private getEventPos), got %+v", issues)
	}
	if anyIssueForFile(issues, "files/paint.js") || anyIssueForFile(issues, "files/canvas.js") {
		t.Fatalf("the defining and clean modules must not be flagged, got %+v", issues)
	}
	for _, is := range issues {
		if is.Category == "undefined_reference" && is.File == "files/objects.js" {
			if is.Severity != "error" || !is.Deterministic {
				t.Fatalf("undefined_reference must be a deterministic error so it drives a fix-pass: %+v", is)
			}
			if !strings.Contains(is.Message, "getEventPos") || !strings.Contains(is.Message, "paint.js") {
				t.Errorf("message should name getEventPos and its private definer paint.js, got: %s", is.Message)
			}
		}
	}
}

// TestStaticCode_FlagsArityMismatch is the arity-mismatch regression: a same-module
// 1-parameter function called with two arguments. This slips through because
// the bundle links clean and the throwing path only ran after first paint (past
// the in-build probe's window); the static check must now catch it up front.
func TestStaticCode_FlagsArityMismatch(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "files/game.terrain.js", `function buildFlora(planet){
  return genFloraName(planet, planet.rng());
}
function genFloraName(rng){ return String(rng()); }`)

	issues := staticIssues(t, db)
	if !hasIssue(issues, "arity_mismatch", "files/game.terrain.js") {
		t.Fatalf("expected arity_mismatch for files/game.terrain.js, got %+v", issues)
	}
	for _, is := range issues {
		if is.Category == "arity_mismatch" && is.File == "files/game.terrain.js" {
			if is.Severity != "warning" || !is.Deterministic {
				t.Fatalf("arity_mismatch is a deterministic warning (usually an ignored extra argument): %+v", is)
			}
			if !strings.Contains(is.Message, "genFloraName") {
				t.Errorf("message should name the over-applied function, got: %s", is.Message)
			}
		}
	}
}

// TestStaticCode_ArityCorrectIsSilent proves the check does not fire once the
// call passes the function reference in the single slot the definition declares.
func TestStaticCode_ArityCorrectIsSilent(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "files/game.terrain.js", `function buildFlora(planet){
  return genFloraName(planet.rng);
}
function genFloraName(rng){ return String(rng()); }`)

	if issues := staticIssues(t, db); hasIssue(issues, "arity_mismatch", "files/game.terrain.js") {
		t.Fatalf("clean call must not be flagged, got %+v", issues)
	}
}

func TestStaticCode_AllIssuesAreDeterministicErrors(t *testing.T) {
	db := setupStaticDB(t)
	saveCodeFile(t, db, "files/broken.js", `export const x = 1 +;`)
	saveCodeFile(t, db, "css/bad.css", `.a{`)

	issues := staticIssues(t, db)
	if len(issues) == 0 {
		t.Fatal("expected issues for broken JS and CSS")
	}
	for _, is := range issues {
		if is.Severity != "error" || !is.Deterministic {
			t.Fatalf("every static issue must be a deterministic error so it drives a fix-pass: %+v", is)
		}
	}
}

// `new QRCode(...)` and `new JSZip()` call globals that declared classic
// libraries name. They are bound at runtime, so the call-position check must not flag
// them (it did, and the fix-pass rewrote working calls to window.QRCode); an undeclared
// call is still flagged.
func TestStaticCode_DeclaredLibraryGlobalCallsNotFlagged(t *testing.T) {
	db := setupStaticDB(t)
	if _, err := db.Exec(`CREATE TABLE of_runs (id INTEGER PRIMARY KEY, plan_json TEXT, started_at TEXT)`); err != nil {
		t.Fatalf("create of_runs: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_runs (plan_json, started_at) VALUES (?, '2026-09-24')`,
		`{"external_libraries":[
			{"name":"QRCode.js","url":"https://cdn.jsdelivr.net/npm/qrcodejs@1.0.0/qrcode.min.js","kind":"classic","global_name":"QRCode"},
			{"name":"JSZip","url":"https://cdn.jsdelivr.net/npm/jszip@3.10.1/dist/jszip.min.js","kind":"classic","global_name":"JSZip"}]}`); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	saveCodeFile(t, db, "files/share.js", `export function qr(el, url){ return new QRCode(el, { text: url }); }
export function zip(){ return new JSZip(); }`)
	saveCodeFile(t, db, "files/bad.js", `export function go(){ return missingHelper(); }`)

	issues := staticIssues(t, db)
	if anyIssueForFile(issues, "files/share.js") {
		t.Fatalf("declared library globals must not be flagged, got %+v", issues)
	}
	if !hasIssue(issues, "undefined_reference", "files/bad.js") {
		t.Fatalf("an undeclared call must still be flagged, got %+v", issues)
	}
}
