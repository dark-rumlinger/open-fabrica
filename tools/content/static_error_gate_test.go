package content

import (
	"testing"

	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// A const-reassign defect: a module declares a binding with `const` and then
// reassigns it. The single-file syntax parse (esbuild Transform) reports this
// clean, so without this gate it reaches the browser and only throws "Assignment to constant
// variable" when the browser runs it. The whole-project bundle the import gate
// already runs proves it statically; the save must be refused as a static_error
// naming the line.
func TestSave_StaticError_ConstReassignRejected(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "state.js", `export const PLANET_RADIUS = 10;`)

	body := `import { PLANET_RADIUS } from './state.js';
function makeGeom(r, s){ return { r: r, s: s }; }
function flatShade(g){ return g; }
export function buildTerrain(seed) {
  const geom = makeGeom(PLANET_RADIUS, seed);
  geom = flatShade(geom);
  return geom;
}`
	res, err := (&ArtSave{Kind: "file", FilePath: "planet.js", Body: body}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("reassigning a const must be rejected, got %+v", res)
	}
	if res.Code != "static_error" {
		t.Errorf("Code = %q, want static_error", res.Code)
	}
	if !contains(res.Error, "constant") {
		t.Errorf("reject should name the binding error, got: %s", res.Error)
	}
}

// The clean form of the same module (let instead of const) must save without a
// false positive, so the gate does not block legitimate reassignment.
func TestSave_StaticError_LetReassignAllowed(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "state.js", `export const PLANET_RADIUS = 10;`)

	body := `import { PLANET_RADIUS } from './state.js';
function makeGeom(r, s){ return { r: r, s: s }; }
function flatShade(g){ return g; }
export function buildTerrain(seed) {
  let geom = makeGeom(PLANET_RADIUS, seed);
  geom = flatShade(geom);
  return geom;
}`
	res, err := (&ArtSave{Kind: "file", FilePath: "planet.js", Body: body}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a clean let-reassignment must save, got %+v", res)
	}
	if d, ok := res.Data.(map[string]any); ok {
		if d["parsed_clean"] != true || d["imports_resolved"] != true {
			t.Errorf("clean save should report parsed_clean+imports_resolved, got %+v", d)
		}
	}
}

// In-progress-repair safety: a const-reassign already present in the stored body
// is not a defect THIS edit introduced, so an edit that leaves it in place while
// changing something else must not be trapped (mirrors the import/cycle gates'
// subtract-prior-issues rule). Without that, a partial fix gets stuck.
func TestSave_StaticError_PreexistingNotTrapped(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	// Seed the broken body directly (bypassing the gate) so it is the prior state.
	seedJS(t, db, "planet.js", `export function buildTerrain() {
  const geom = make();
  geom = flat(geom);
  return geom;
}`)
	// Re-save with the SAME const-reassign plus an unrelated edit. The defect was
	// not introduced here, so it must proceed (so the model can repair in steps).
	body := `export function buildTerrain() {
  const geom = make();
  geom = flat(geom);
  return geom; // touched
}`
	res, err := (&ArtSave{Kind: "file", FilePath: "planet.js", Body: body}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a pre-existing static error must not trap an unrelated edit, got %+v", res)
	}
}
