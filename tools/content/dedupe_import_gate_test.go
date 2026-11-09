package content

import (
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// The weak-model loop this closes: a leaf imports a sibling symbol at the top AND
// re-imports it lower down (a `gridToWorld` pulled in twice). esbuild rejects the
// exact duplicate as an "already declared" binding (static_error); the model then
// reaches for a manual delete-the-line edit and corrupts the file's syntax
// instead (a patch loop). The gate now auto-collapses the redundant copy -
// which changes no semantics - and the write lands, surfacing the removal on the echo.
func TestSave_DuplicateImport_AutoCollapses(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "terrain.js", `export function gridToWorld(n){ return n; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "builder.history.js",
		Body: `import { gridToWorld } from './terrain.js';
export function record(x){ return gridToWorld(x); }
import { gridToWorld } from './terrain.js';`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a redundant duplicate import should auto-collapse and pass, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	removed, _ := data["auto_imported"].([]string)
	if len(removed) != 1 || !contains(removed[0], "removed duplicate") {
		t.Fatalf("echo should report the removed duplicate import, got %+v", data["auto_imported"])
	}
	// The stored body must actually carry a single import now (the fix is real) -
	// and the specifier normalizer collapses the relative form to the bare name.
	got, err := store.ArtifactGet(db, "file", "builder.history.js")
	if err != nil || got == nil {
		t.Fatalf("get builder.history.js: %v", err)
	}
	if n := strings.Count(got.Body, "import { gridToWorld } from 'terrain'"); n != 1 {
		t.Fatalf("stored body should carry exactly one bare-specifier import of gridToWorld, got %d:\n%s", n, got.Body)
	}
	if !contains(got.Body, "return gridToWorld(x)") {
		t.Fatalf("the module's real code must be preserved, got:\n%s", got.Body)
	}
}

// Conservatism guard: the SAME name imported from two DIFFERENT sources is a real
// binding conflict the model must resolve, not a redundant duplicate - dedupe stands
// down and the static_error reject stands.
func TestSave_DuplicateImport_DifferentSource_StaysRejected(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "terrain.js", `export function gridToWorld(n){ return n; }`)
	seedJS(t, db, "world.js", `export function gridToWorld(n){ return n * 2; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "builder.history.js",
		Body: `import { gridToWorld } from './terrain.js';
import { gridToWorld } from './world.js';
export function record(x){ return gridToWorld(x); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// A duplicate declaration parses as a syntax error ("already been declared"); dedupe
	// declines it (different specifiers = a real conflict), so the reject must stand.
	if res == nil || res.Success || res.Code != "syntax_error" {
		t.Fatalf("same-name-different-source must NOT be auto-collapsed; expected syntax_error, got %+v", res)
	}
}

// steerEditRejectToResave is a no-op on a nil rejection and, on a real one, appends the
// re-apply decision rule: patch again if you hold the exact bytes (the cheap path), else
// re-get or re-save the whole corrected body - no longer an unconditional "re-save the
// WHOLE file" lead (which fought "patch is the cheapest shape").
func TestSteerEditRejectToResave(t *testing.T) {
	if got := steerEditRejectToResave(nil); got != nil {
		t.Errorf("nil rejection must pass through, got %+v", got)
	}
	rej := &tools.Result{Success: false, Code: "undefined_reference", Error: "boom."}
	got := steerEditRejectToResave(rej)
	if !contains(got.Error, "patch again") {
		t.Errorf("edit reject steer must offer the patch-again path when the bytes are held, got: %s", got.Error)
	}
	if !contains(got.Error, "re-save the whole corrected body") || !contains(got.Error, "action=\"save\"") {
		t.Errorf("edit reject steer must still name the re-save fallback, got: %s", got.Error)
	}
}

// Integration: a module-wiring reject reached through a PARTIAL EDIT (here a patch that
// introduces an undefined reference) must carry the re-apply steer, since a follow-up
// patch anchored against bytes the model may not hold is what drifts into the loop.
func TestPatch_ModuleReject_CarriesResaveSteer(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "util.js", `export function fmt(n){ return n + 1; }`)
	seedJS(t, db, "page.js", `import { fmt } from './util.js';
export function show(n){ return fmt(n); }`)

	// Patch the call site to reach a name nothing exports - `gone` has no exporter, so
	// auto-wire cannot fire and the patch rejects with the re-apply steer attached.
	res, err := (&ArtPatch{
		Kind:     "file",
		FilePath: "page.js",
		Patches: []map[string]any{
			{"search": "return fmt(n);", "replace": "return fmt(n) + gone(n);"},
		},
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "undefined_reference" {
		t.Fatalf("a patch introducing an undefined reference must be rejected, got %+v", res)
	}
	if !contains(res.Error, "re-save the whole corrected body") {
		t.Errorf("a partial-edit module reject must carry the re-apply steer with the re-save fallback, got: %s", res.Error)
	}
}

// The patch_anchor_not_found message must give the targeted recovery first - re-get the
// exact region and copy a correct anchor - and still name the re-save fallback for when
// the model does not hold the file's current bytes.
func TestPatch_AnchorNotFound_SteersToResave(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "x.js", "const a = 1;\nconst b = 2;\n")

	res, err := (&ArtPatch{
		Kind:     "file",
		FilePath: "x.js",
		Patches: []map[string]any{
			{"search": "DOES_NOT_EXIST", "replace": "WHATEVER"},
		},
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "patch_anchor_not_found" {
		t.Fatalf("a no-match patch must fail with patch_anchor_not_found, got %+v", res)
	}
	if !contains(res.Error, "copy the anchor verbatim") {
		t.Errorf("patch_anchor_not_found must give the re-get-and-patch recovery first, got: %s", res.Error)
	}
	if !contains(res.Error, "re-save the whole corrected body") {
		t.Errorf("patch_anchor_not_found must still name the re-save fallback, got: %s", res.Error)
	}
}
