package content

import (
	"testing"

	_ "modernc.org/sqlite"
)

// The recurring build defect this gate closes PROACTIVELY: the model authors the
// page entry (the module graph's unimported root) with shared helpers exported
// from it, then a leaf imports one back and closes a cycle. The reactive
// cyclic_import gate only fires on that later leaf save; by then the model must
// relocate the surface OUT of an already-written entry, which re-trips per leaf and
// churns patch anchors. This gate fires when the ENTRY itself is saved, so the
// relocation happens once, before any leaf depends on the bad shape.
func TestSave_EntryExports_RejectedProactivelyOnAttach(t *testing.T) {
	ctx := setupClobberDB(t)
	// Seed the page and the zero-import state leaf (the correct home for shared code).
	if res, err := (&ArtSave{Kind: "page", FilePath: "/", Body: "<div id=game></div>"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed page: err=%v res=%+v", err, res)
	}
	seedJS(t, ctx.DB, "voxel.state.js", `export const state = { camera: null };`)

	// Save the page entry (attach_to='/') importing the state leaf but ALSO exporting
	// a shared helper - the exact shape that later forces a leaf->entry cycle.
	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "voxel.js",
		AttachTo: "/",
		Body: `import { state } from './voxel.state.js';
export function syncCamera(){ return state.camera; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("a page entry that exports shared helpers must be rejected, got %+v", res)
	}
	if res.Code != "entry_exports" {
		t.Errorf("Code = %q, want entry_exports", res.Code)
	}
	// Names the entry, the exported symbol, the zero-import leaf destination, and
	// reassures that no other module needs editing this turn (the entry is caught
	// before any leaf imports back, so a big-bang multi-file relocation is not needed).
	for _, want := range []string{"voxel.js", "syncCamera", "voxel.state.js", "nothing else needs to change this turn"} {
		if !contains(res.Error, want) {
			t.Errorf("reject should mention %q, got: %s", want, res.Error)
		}
	}
}

// Regression for a cross-model failure mode: when a page entry exports a shared
// helper, a leaf imports it back and trips cyclic_import, after which a model may
// dodge with an `import().then()` and loop on patch anchors. Putting the shared
// surface in a zero-import leaf from the start avoids it. This gate makes that
// mistake impossible: the entry save itself is refused before any leaf can import back.
func TestSave_EntryExports_EntryReexportRegression(t *testing.T) {
	ctx := setupClobberDB(t)
	if res, err := (&ArtSave{Kind: "page", FilePath: "/game", Body: "<canvas id=c></canvas>"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed page: err=%v res=%+v", err, res)
	}
	seedJS(t, ctx.DB, "game.state.js", `export const state = {};
export function blockAt(){ return 0; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "game.js",
		AttachTo: "/game",
		Body: `import { state, blockAt } from './game.state.js';
export function remeshAround(x, y, z){ return blockAt(x, y, z) && state; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "entry_exports" {
		t.Fatalf("GLM's entry exporting remeshAround must be rejected entry_exports, got %+v", res)
	}
	for _, want := range []string{"game.js", "remeshAround", "game.state.js"} {
		if !contains(res.Error, want) {
			t.Errorf("reject should mention %q, got: %s", want, res.Error)
		}
	}
}

// The architecturally-correct entry passes cleanly: it imports the leaves and the
// zero-import state leaf, and exports NOTHING (the unimported root has no reachable
// export). This must not be flagged.
func TestSave_EntryNoExports_Accepted(t *testing.T) {
	ctx := setupClobberDB(t)
	if res, err := (&ArtSave{Kind: "page", FilePath: "/", Body: "<div id=game></div>"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed page: err=%v res=%+v", err, res)
	}
	seedJS(t, ctx.DB, "voxel.state.js", `export const state = { camera: null };`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "voxel.js",
		AttachTo: "/",
		Body: `import { state } from './voxel.state.js';
function boot(){ return state; }
boot();`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a page entry that exports nothing must be accepted, got %+v", res)
	}
}

// False-positive guard for the leaves-first build order: a LEAF saved BEFORE the
// entry exists also has in-degree 0 and imports the state leaf and exports its
// functions - but it is NOT page-attached, so it must never be mistaken for the
// entry. Only the page-attached root is the entry.
func TestSave_LeafBeforeEntry_NotFlaggedAsEntry(t *testing.T) {
	ctx := setupClobberDB(t)
	seedJS(t, ctx.DB, "voxel.state.js", `export const state = { camera: null };`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "voxel.terrain.js", // a leaf: no attach_to, imported later by the entry
		Body: `import { state } from './voxel.state.js';
export function initTerrain(){ return state; }
export function updateTerrain(){}`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a leaf exporting its own surface must be accepted (it is not the entry), got %+v", res)
	}
}

// In-flight tolerance: an entry that ALREADY exports (a mid-relocation state) is not
// re-rejected on an unrelated edit - only an export this edit ADDS fires the gate.
// Mirrors the sibling gates' oldBody subtraction so a repair in progress is free to
// proceed.
func TestPatch_EntryPreexistingExports_NotReRejected(t *testing.T) {
	ctx := setupClobberDB(t)
	seedJS(t, ctx.DB, "voxel.state.js", `export const state = { camera: null };`)
	// Seed the entry ALREADY exporting (seedJS bypasses the gate) and attach it.
	seedJS(t, ctx.DB, "voxel.js", `import { state } from './voxel.state.js';
export function syncCamera(){ return state.camera; }`)
	if _, err := ctx.DB.Exec(`INSERT INTO of_page_assets (page_path, file_path, position) VALUES ('/', 'voxel.js', 0)`); err != nil {
		t.Fatalf("attach entry: %v", err)
	}

	// An unrelated edit that keeps the same export surface must not be re-rejected.
	res, err := (&ArtPatch{
		Kind:     "file",
		FilePath: "voxel.js",
		Patches: []map[string]any{
			{"search": "return state.camera;", "replace": "return state.camera; /* eye */"},
		},
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res != nil && !res.Success && res.Code == "entry_exports" {
		t.Fatalf("a pre-existing entry export must not be re-rejected on an unrelated edit, got %+v", res)
	}
}
