package content

import (
	"testing"

	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// The defect this gate closes: a leaf importing engine services back from the
// entry. The names RESOLVE (the entry really exports register/getScene), so the
// unresolved-import gate passes - yet the import couples the leaf back to the
// entry and closes a cycle game.js <-> game.tool.js. That cyclic graph still
// boots but ships its wiring silently dead (the registry the entry sets up in its
// body runs after the hoisted leaf imports that fill it). The save must be
// rejected with cyclic_import and steered to the zero-import store module fix.
func TestSave_CyclicImport_RejectedWhenNamesResolve(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	// Entry already imports the leaf and exports the engine services the leaf wants.
	seedJS(t, db, "game.js", `import './game.tool.js';
export function register(s){}
export function getScene(){ return {}; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "game.tool.js",
		Body: `import { register, getScene } from './game.js';
export function beginNewGame(){ register('tool'); return getScene(); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("a leaf importing back from the entry must be rejected, got %+v", res)
	}
	if res.Code != "cyclic_import" {
		t.Errorf("Code = %q, want cyclic_import", res.Code)
	}
	// The loop is named from the edited file (CycleThrough(path)), so it reads
	// game.tool.js -> game.js -> game.tool.js.
	if !contains(res.Error, "cyclic module graph") || !contains(res.Error, "game.tool.js -> game.js -> game.tool.js") {
		t.Errorf("reject should name the loop, got: %s", res.Error)
	}
	if !contains(res.Error, "zero-import store module") {
		t.Errorf("reject should steer to the zero-import store fix, got: %s", res.Error)
	}
}

// The same write that closes a loop from the OTHER direction: the entry adding an
// import of a leaf that already imports it back. The gate must fire on whichever
// edit closes the cycle, not only on the leaf side.
func TestSave_CyclicImport_RejectedOnEntryClosingEdge(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	// Leaf already imports the entry (resolves); entry does not yet import the leaf.
	seedJS(t, db, "game.tool.js", `import { register } from './game.js';
export function beginNewGame(){ register('tool'); }`)
	seedJS(t, db, "game.js", `export function register(s){}`)

	// Now the entry adds the leaf import - this is the edge that closes the cycle.
	res, err := (&ArtPatch{
		Kind:     "file",
		FilePath: "game.js",
		Patches: []map[string]any{
			{"search": "export function register(s){}", "replace": "import './game.tool.js';\nexport function register(s){}"},
		},
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("the entry import that closes a cycle must be rejected, got %+v", res)
	}
	if res.Code != "cyclic_import" {
		t.Errorf("Code = %q, want cyclic_import", res.Code)
	}
}

// The batch-fix: when a leaf closes a cycle back to the entry, the reject must name
// the entry's WHOLE exported surface and the zero-import leaf to move it into - not
// just the one or two symbols this edit reached for. When the entry exports many
// UI-glue symbols, steering the model to relocate only the ones the first leaf
// imported re-trips the gate for every later leaf and migrates the surface one batch
// at a time over many turns.
func TestSave_CyclicImport_NamesWholeEntrySurfaceAndDest(t *testing.T) {
	ctx := setupClobberDB(t)
	db := ctx.DB
	// Zero-import state leaf: the correct home for shared symbols.
	seedJS(t, db, "editor.state.js", `export const store = {};
export const dom = {};`)
	// Entry imports the state leaf AND the canvas leaf, and (wrongly) exports the
	// shared UI glue the leaves need.
	seedJS(t, db, "editor.js", `import { store, dom } from './editor.state.js';
import './editor.canvas.js';
export function setTool(t){}
export function updateStatusZoom(){}
export function updateStatusCursor(){}
export function updateBrushCursor(){}`)
	// Page attachment is what makes editor.js the entry.
	if _, err := db.Exec(`INSERT INTO of_page_assets (page_path, file_path, position) VALUES ('/', 'editor.js', 0)`); err != nil {
		t.Fatalf("attach entry: %v", err)
	}

	// The leaf reaches for two of the entry's glue symbols - closing the cycle.
	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "editor.canvas.js",
		Body: `import { store, dom } from './editor.state.js';
import { updateStatusZoom, updateStatusCursor } from './editor.js';
export function CanvasSystem(){ updateStatusZoom(); updateStatusCursor(); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "cyclic_import" {
		t.Fatalf("leaf importing back from the entry must be rejected cyclic_import, got %+v", res)
	}
	// (a) names the hub entry.
	if !contains(res.Error, "editor.js") {
		t.Errorf("reject should name the entry hub, got: %s", res.Error)
	}
	// (b) lists the ENTIRE entry surface, including symbols this edit did NOT import
	// (setTool, updateBrushCursor), so one pass can move all of them.
	for _, sym := range []string{"setTool", "updateStatusZoom", "updateStatusCursor", "updateBrushCursor"} {
		if !contains(res.Error, sym) {
			t.Errorf("reject should list the whole entry surface incl. %q, got: %s", sym, res.Error)
		}
	}
	// (c) names the zero-import state leaf as the destination.
	if !contains(res.Error, "editor.state.js") {
		t.Errorf("reject should name the zero-import store module destination, got: %s", res.Error)
	}
	// (d) instructs a single-turn batch move.
	if !contains(res.Error, "ONE turn") {
		t.Errorf("reject should instruct a single-turn batch relocation, got: %s", res.Error)
	}
}

// Asymmetry preserved: a cycle already present in oldBody is an in-progress state this
// edit did not create, so an unrelated later edit to a file already in the cycle is not
// re-rejected as cyclic_import (the message change must not disturb cycleRejection's
// false-positive-safe oldBody subtraction).
func TestPatch_PreexistingCycle_NotReRejected(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "editor.state.js", `export const store = {};`)
	seedJS(t, db, "editor.js", `import './editor.canvas.js';
export function setTool(t){}`)
	// Seed the leaf ALREADY closing the cycle (seedJS bypasses the gate).
	seedJS(t, db, "editor.canvas.js", `import { setTool } from './editor.js';
export function CanvasSystem(){ setTool('brush'); }`)

	res, err := (&ArtPatch{
		Kind:     "file",
		FilePath: "editor.canvas.js",
		Patches: []map[string]any{
			{"search": "setTool('brush');", "replace": "setTool('brush'); /* pan */"},
		},
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res != nil && !res.Success && res.Code == "cyclic_import" {
		t.Fatalf("a pre-existing cycle must not be re-rejected on an unrelated edit, got %+v", res)
	}
}

// The architecturally-correct shape must pass cleanly: the entry imports the leaf
// (one-directional), and the leaf imports the shared engine services from a
// zero-import core leaf - never from the entry. No cycle, names resolve.
func TestSave_AcyclicTreeViaZeroImportLeaf_Accepted(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	// Zero-import core leaf holds the shared engine services.
	seedJS(t, db, "game.core.js", `export function register(s){}
export function getScene(){ return {}; }`)
	// Entry imports the leaf (the unimported root).
	seedJS(t, db, "game.js", `import './game.tool.js';`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "game.tool.js",
		Body: `import { register, getScene } from './game.core.js';
export function beginNewGame(){ register('tool'); return getScene(); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a clean entry -> leaf -> zero-import-leaf tree must be accepted, got %+v", res)
	}
	if d, ok := res.Data.(map[string]any); ok {
		if d["imports_resolved"] != true {
			t.Errorf("imports_resolved should be true on the clean tree, got %+v", d["imports_resolved"])
		}
	}
}

// A common shape: two feature leaves that each call into the other
// (chat re-renders the verdict after a reply; the verdict calls back into chat), with
// the page entry importing both and in no cycle. Neither leaf is the entry, so the
// reject must not call one "the entry" or tell the model to move a leaf's whole API
// into the store: it names the loop as leaf-to-leaf and steers the call that points
// back into a notification through the store module both already import.
func TestSave_CyclicImport_PeerLeavesSteerToNotification(t *testing.T) {
	ctx := setupClobberDB(t)
	seedJS(t, ctx.DB, "app.state.js", `export const state = { messages: [] };`)
	seedJS(t, ctx.DB, "app.chat.js", `import { state } from './app.state.js';
import { maybeIssueVerdict } from './app.verdict.js';
export function sendMessage(t){ state.messages.push(t); maybeIssueVerdict(); }
export function regenerateLast(){}`)
	seedJS(t, ctx.DB, "app.js", `import './app.chat.js';
import './app.verdict.js';`)
	if _, err := ctx.DB.Exec(`INSERT INTO of_page_assets (page_path, file_path, position) VALUES ('/', 'app.js', 0)`); err != nil {
		t.Fatalf("attach entry: %v", err)
	}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "app.verdict.js",
		Body: `import { state } from './app.state.js';
import { regenerateLast } from './app.chat.js';
export function maybeIssueVerdict(){ if (!state.messages.length) regenerateLast(); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "cyclic_import" {
		t.Fatalf("two leaves importing each other must be rejected cyclic_import, got %+v", res)
	}
	for _, want := range []string{"app.verdict.js -> app.chat.js -> app.verdict.js", "None of these is the page entry", "hooks", "subscribe", "`app.state.js`"} {
		if !contains(res.Error, want) {
			t.Errorf("reject should mention %q, got: %s", want, res.Error)
		}
	}
	for _, bad := range []string{"is the page entry (the unimported root)", "Move them ALL", "sendMessage"} {
		if contains(res.Error, bad) {
			t.Errorf("a leaf-to-leaf loop must not be framed as an entry relocation (%q), got: %s", bad, res.Error)
		}
	}
}

// The store module the reject names must be one the looping modules actually import.
// An unrelated zero-import file (a login page's helper) is never offered as the store.
func TestSave_CyclicImport_NeverNamesAnUnrelatedStore(t *testing.T) {
	ctx := setupClobberDB(t)
	seedJS(t, ctx.DB, "js/auth.js", `export function login(){}`)
	seedJS(t, ctx.DB, "app.editor.js", `import { openDetail } from './app.detail.js';
export function openEditor(){ openDetail(); }`)
	seedJS(t, ctx.DB, "app.detail.js", `export function openDetail(){}`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "app.detail.js",
		Body: `import { openEditor } from './app.editor.js';
export function openDetail(){ openEditor(); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "cyclic_import" {
		t.Fatalf("want cyclic_import, got %+v", res)
	}
	if contains(res.Error, "auth.js") {
		t.Errorf("an unrelated zero-import file must not be offered as the store, got: %s", res.Error)
	}
}
