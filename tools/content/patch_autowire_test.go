package content

import (
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// The auto-only change: a PATCH that introduces a call to a sibling-exported symbol
// must AUTO-WIRE the import - exactly as save does - and land, instead of bouncing to
// a full re-save and forcing the model to hand-edit a fragile import line (the desync
// that drives patch thrash). This pins save/patch parity on imports:
// the model USES a symbol and the platform owns the import line.
func TestPatch_ForgottenReference_AutoWiresLikeSave(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "util.js", `export function fmt(n){ return n + 1; }`)
	seedJS(t, db, "page.js", `export function show(n){ return n; }`)

	// Patch in a CALL to fmt() with no import - the wiring the model would otherwise
	// try to hand-patch onto the import line.
	res, _ := artifactActionPatch(ctx, "file", "page.js", []map[string]any{
		{"search": "return n;", "replace": "return fmt(n);"},
	})
	if res == nil || !res.Success {
		t.Fatalf("a patch introducing a single-exporter forgotten import must auto-wire and land, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	imported, _ := data["auto_imported"].([]string)
	if len(imported) == 0 {
		t.Fatalf("patch must surface the auto-wired import under auto_imported, got %+v", data)
	}
	if j := strings.Join(imported, " "); !strings.Contains(j, "fmt") || !strings.Contains(j, "util") {
		t.Errorf("auto_imported should name fmt from util, got %q", j)
	}
	// The stored file now carries the import AND the call - the model never typed the import.
	a, _ := store.ArtifactGet(db, "file", "page.js")
	if a == nil {
		t.Fatal("page.js vanished after patch")
	}
	if !strings.Contains(a.Body, "import { fmt }") || !strings.Contains(a.Body, "fmt(n)") {
		t.Fatalf("stored page.js must carry the auto-wired import and the call, got:\n%s", a.Body)
	}

	// Control: a patch that leaves the file self-consistent still needs no wiring and
	// reports no auto_imported (the heal fires only when there is a real forgotten import).
	res2, _ := artifactActionPatch(ctx, "file", "util.js", []map[string]any{
		{"search": "return n + 1;", "replace": "return n + 2;"},
	})
	if res2 == nil || !res2.Success {
		t.Fatalf("a self-consistent patch must land, got %+v", res2)
	}
	if d, _ := res2.Data.(map[string]any); d["auto_imported"] != nil {
		t.Errorf("a patch needing no wiring must not report auto_imported, got %+v", d["auto_imported"])
	}
}

// The partial-import case: the file ALREADY imports a name from the sole owner, then
// calls a second symbol that owner also exports. The platform must MERGE the new name
// into the existing import line and land - not reject with "add it to that line, don't
// re-edit the call site" (the directive that contradicted the build frame's never-hand-
// edit-an-import-line rule). Pinned on the patch path (save shares resolveWrittenImports).
func TestPatch_PartialImport_MergesIntoExistingLine(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "util.js", "export function a(n){ return n; }\nexport function b(n){ return n + 1; }")
	seedJS(t, db, "page.js", "import { a } from './util.js';\nexport function show(n){ return a(n); }")

	res, _ := artifactActionPatch(ctx, "file", "page.js", []map[string]any{
		{"search": "return a(n);", "replace": "return a(n) + b(n);"},
	})
	if res == nil || !res.Success {
		t.Fatalf("a partial-import forgotten reference must auto-merge and land, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	if imported, _ := data["auto_imported"].([]string); len(imported) == 0 || !strings.Contains(strings.Join(imported, " "), "b") {
		t.Fatalf("patch must surface the merged import under auto_imported, got %+v", data)
	}
	a2, _ := store.ArtifactGet(db, "file", "page.js")
	if a2 == nil {
		t.Fatal("page.js vanished after patch")
	}
	// Both names in ONE brace (merged, sorted), and no second import statement.
	if !strings.Contains(a2.Body, "import { a, b }") {
		t.Fatalf("b must be merged into the existing import line, got:\n%s", a2.Body)
	}
	if strings.Contains(a2.Body, "import { b }") {
		t.Fatalf("merge must not add a second import statement for the same module, got:\n%s", a2.Body)
	}
}

// The member-position twin of the forgotten call: a patch that starts using a sibling's
// exported object through `name.prop` (e.g. `hooks.reset = fn` and
// `hooks.openReport(m)` right after the model added `hooks` to the store) auto-wires the
// import like a bare call does, instead of bouncing an undefined_reference whose own
// message already names the one import line to add.
func TestPatch_MemberUseOfSiblingExport_AutoWires(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	if _, err := db.Exec(`INSERT INTO of_runs (id, plan_json) VALUES ('run_1', '{"external_libraries":[]}')`); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	seedJS(t, db, "santa.state.js", "export const state = {};\nexport const hooks = { openReport: null, reset: null };")
	seedJS(t, db, "santa.chat.js", "import { state } from './santa.state.js';\nexport function resetConversation(){ state.m = []; }\nexport function initChat(){\n  state.ready = true;\n}\nexport function flag(m){ return m; }")

	res, _ := artifactActionPatch(ctx, "file", "santa.chat.js", []map[string]any{
		{"search": "  state.ready = true;", "replace": "  hooks.reset = resetConversation;\n  state.ready = true;"},
		{"search": "return m;", "replace": "hooks.openReport(m); return m;"},
	})
	if res == nil || !res.Success {
		t.Fatalf("a member use of a single-exporter sibling symbol must auto-wire and land, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	imported, _ := data["auto_imported"].([]string)
	if j := strings.Join(imported, " "); !strings.Contains(j, "hooks") || !strings.Contains(j, "santa.state") {
		t.Errorf("auto_imported should name hooks from santa.state, got %q", j)
	}
	a, _ := store.ArtifactGet(db, "file", "santa.chat.js")
	if a == nil || !strings.Contains(a.Body, "hooks }") && !strings.Contains(a.Body, "hooks,") && !strings.Contains(a.Body, ", hooks") {
		t.Fatalf("stored santa.chat.js must import hooks, got:\n%v", a)
	}
}
