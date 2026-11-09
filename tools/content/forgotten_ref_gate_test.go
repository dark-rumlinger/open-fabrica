package content

import (
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// The failure this gate catches. canvas.js exports screenToDoc; paint.js defines a PRIVATE
// getEventPos (not exported) and uses it; objects.js imports screenToDoc but calls
// getEventPos without importing or defining it. Every import resolves, so the
// import gate passes - the forgotten-reference gate must refuse it and steer to the
// module that defines it privately, in one shot.
func TestSave_ForgottenReference_DefinedButNotExportedSibling(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "canvas.js", `export function screenToDoc(x, y){ return { x: x, y: y }; }`)
	seedJS(t, db, "paint.js", `import { screenToDoc } from './canvas.js';
function getEventPos(e){ return screenToDoc(e.clientX, e.clientY); }
export function brushDown(e){ return getEventPos(e); }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "objects.js",
		Body: `import { screenToDoc } from './canvas.js';
export function moveMove(e){ var p = getEventPos(e); return p; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "undefined_reference" {
		t.Fatalf("calling a sibling's private helper must be rejected as undefined_reference, got %+v", res)
	}
	if !contains(res.Error, "`getEventPos`") {
		t.Errorf("reject should name getEventPos, got: %s", res.Error)
	}
	if !contains(res.Error, "not exported in `paint.js`") {
		t.Errorf("reject should point at the defining module, got: %s", res.Error)
	}
	if !contains(res.Error, "import { getEventPos } from 'paint';") {
		t.Errorf("reject should carry the corrected import line, got: %s", res.Error)
	}
}

// The forgotten-import variant: the helper IS exported by exactly one sibling and the
// writer just never imported it, with no existing import line from that sibling to
// merge into - the clean fresh-leaf case. The gate now AUTO-WIRES the missing import
// and lets the write land (instead of a reject-retry bounce), surfacing the added line
// on the success echo so the model sees the wiring it did not type.
func TestSave_ForgottenReference_AutoWiresExportedSibling(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "util.js", `export function fmt(n){ return n + 1; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "page.js",
		Body:     `export function show(n){ return fmt(n); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a single-exporter forgotten import should auto-wire and pass, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	imported, _ := data["auto_imported"].([]string)
	if len(imported) != 1 || !contains(imported[0], "import { fmt } from 'util';") {
		t.Fatalf("success echo should report the auto-added import of fmt from util.js, got %+v", data["auto_imported"])
	}
	// The stored body must actually carry the import now (the wiring is real, not
	// just echoed), and the module must resolve clean.
	got, err := store.ArtifactGet(db, "file", "page.js")
	if err != nil || got == nil {
		t.Fatalf("get page.js: %v", err)
	}
	if !contains(got.Body, "import { fmt } from 'util';") || !contains(got.Body, "return fmt(n)") {
		t.Fatalf("stored body should carry the auto-added import above the original code, got:\n%s", got.Body)
	}
}

// Ambiguity guard: when TWO siblings export the same name, the fix is a real choice
// the model must make, so auto-wire stands down and the reject fires (naming both).
func TestSave_ForgottenReference_AmbiguousExporterNotAutoWired(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "a.js", `export function draw(){ return 1; }`)
	seedJS(t, db, "b.js", `export function draw(){ return 2; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "page.js",
		Body:     `export function run(){ return draw(); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "undefined_reference" {
		t.Fatalf("a name two siblings export must NOT be auto-wired (ambiguous); expected reject, got %+v", res)
	}
}

// A genuinely-undefined name (no module exports it) is a real defect, never auto-wired.
func TestSave_ForgottenReference_UndefinedNameNotAutoWired(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "page.js",
		Body:     `export function run(){ return nowhere(); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "undefined_reference" {
		t.Fatalf("a name no module exports must not be auto-wired; expected reject, got %+v", res)
	}
}

// The partial-import case this covers: a leaf imports SOME names from the state leaf
// (state, C, ISLANDS, register) but calls getHooks - a name the SAME leaf also exports -
// without importing it. The platform MERGES getHooks into the existing import line so the
// write lands, sparing the model from hand-editing the import line (matching the build
// frame's never-hand-edit rule). Without the merge a model tends to loop here, convinced
// getHooks is "already imported at the top" and re-editing the call site with three
// different tools instead of the import line.
func TestSave_PartialImport_MergesIntoExistingImportLine(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "sailing.state.js", `export const state = {};
export const C = {};
export const ISLANDS = [];
export function register(n, fn){ return [n, fn]; }
export function getHooks(){ return {}; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "sailing.ocean.js",
		Body: `import { state, C, ISLANDS, register } from './sailing.state.js';
export function initOcean(){ register('update', tick); }
function getSun(){ return getHooks().getSun(); }
function tick(){ return getSun(); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a partial-import forgotten reference must auto-merge and land, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	imported, _ := data["auto_imported"].([]string)
	if len(imported) == 0 || !contains(strings.Join(imported, " "), "getHooks") {
		t.Fatalf("success echo should report getHooks merged into the import, got %+v", data["auto_imported"])
	}
	got, err := store.ArtifactGet(db, "file", "sailing.ocean.js")
	if err != nil || got == nil {
		t.Fatalf("get sailing.ocean.js: %v", err)
	}
	// getHooks merged into the ONE existing import statement - never a second one.
	if !contains(got.Body, "getHooks") || strings.Count(got.Body, "import {") != 1 {
		t.Fatalf("getHooks must merge into the single existing import line, got:\n%s", got.Body)
	}
}

// Adding the missing import clears the reject in one turn.
func TestSave_ForgottenReference_ClearedByAddingImport(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "util.js", `export function fmt(n){ return n + 1; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "page.js",
		Body: `import { fmt } from 'util';
export function show(n){ return fmt(n); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("importing the name should pass the gate, got %+v", res)
	}
}

// Capitalized call names are constructors / ambient globals by JS convention - a modern
// browser API (`new CSSStyleSheet()`) the curated jsGlobals list does not enumerate. This
// call-position gate has no library-exactness escape hatch, so hard-rejecting an unlisted
// capitalized name would block valid code and force wasted repair turns. It must pass
// (demoted to the non-blocking post-build undefined_reference nudge), while a lowercase
// forgotten helper in the same body still hard-rejects.
func TestSave_ForgottenReference_CapitalizedBrowserCtorNotRejected(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "theme.js",
		Body: `const sheet = new CSSStyleSheet();
sheet.replaceSync(':root{color:red}');
export function apply(){ document.adoptedStyleSheets = [sheet]; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a capitalized browser constructor absent from jsGlobals must not be hard-rejected, got %+v", res)
	}

	// The lowercase counterpart is still the genuine forgotten-helper catch: a bare
	// lowercase call no module exports must reject even alongside a demoted capitalized name.
	res2, err := (&ArtSave{
		Kind:     "file",
		FilePath: "theme2.js",
		Body: `const sheet = new CSSStyleSheet();
export function apply(){ return decorate(sheet); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res2 == nil || res2.Success || res2.Code != "undefined_reference" {
		t.Fatalf("a lowercase forgotten helper must still reject even beside a demoted capitalized name, got %+v", res2)
	}
}

// False-positive-safe asymmetry. A body already carrying the undefined reference is
// an in-progress repair state; an edit that leaves that pre-existing reference in
// place (here, adding an unrelated export) must NOT be trapped - only a reference
// THIS edit introduces is refused. The prior body is seeded directly so it reaches
// the DB without passing the gate, exactly as a pre-gate or patch-path write would.
func TestSave_ForgottenReference_PreexistingNotTrapped(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "objects.js", `export function moveMove(e){ return getEventPos(e); }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "objects.js",
		Body: `export function moveMove(e){ return getEventPos(e); }
export function extra(){ return 1; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res != nil && !res.Success && res.Code == "undefined_reference" {
		t.Fatalf("a pre-existing undefined reference must not trap an unrelated edit, got %+v", res)
	}
}

// A lowercase global a declared classic library names (canvas-confetti's `confetti`)
// is bound by its <script src>: calling it bare must save, not be rejected as a
// forgotten helper.
func TestSave_ForgottenReference_DeclaredLibraryGlobalCallSaves(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	if _, err := db.Exec(
		`INSERT INTO of_runs (id, plan_json) VALUES ('run_1', ?)`,
		`{"external_libraries":[{"name":"canvas-confetti","url":"https://cdn.jsdelivr.net/npm/canvas-confetti@1.9.3/dist/confetti.browser.min.js","kind":"classic","global_name":"confetti"}]}`,
	); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "party.js",
		Body:     `export function celebrate(){ confetti({ particleCount: 80 }); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a declared library global must not be rejected, got %+v", res)
	}
}
