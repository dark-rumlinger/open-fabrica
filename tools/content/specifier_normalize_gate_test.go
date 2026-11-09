package content

import (
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// A save whose imports use relative specifiers for saved siblings is normalized
// to the bare style at write time, with the rewrites surfaced on the echo. The
// stored body converges on the one import style the platform teaches, snapshots,
// and maps in the importmap.
func TestSave_RelativeSpecifier_NormalizedToBare(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "js/world.state.js", `export let net = null;
export function setNet(n){ net = n; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "js/world.hud.js",
		Body: `import { net } from './world.state.js';
export function draw(){ return net; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("normalizable save should land, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	rws, _ := data["normalized_specifiers"].([]string)
	if len(rws) != 1 || !contains(rws[0], "'./world.state.js' -> 'world.state'") {
		t.Fatalf("echo should report the specifier rewrite, got %+v", data["normalized_specifiers"])
	}
	got, _ := store.ArtifactGet(db, "file", "js/world.hud.js")
	if got == nil || !contains(got.Body, "from 'world.state'") || contains(got.Body, "./world.state.js") {
		t.Fatalf("stored body should carry the bare specifier, got:\n%v", got)
	}
}

// A relative import of a sibling NOT saved yet stays as written: rewriting it
// would launder the mid-split pending signal into what looks like an external
// library. The importmap's URL-key remap keeps the relative form safe at runtime.
func TestSave_RelativeSpecifier_PendingSiblingStaysRelative(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "js/world.hud.js",
		Body: `import { net } from './world.state.js';
export function draw(){ return net; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a pending-sibling import is advisory, the save should land, got %+v", res)
	}
	got, _ := store.ArtifactGet(db, "file", "js/world.hud.js")
	if got == nil || !contains(got.Body, "from './world.state.js'") {
		t.Fatalf("a pending-sibling relative import must stay as written, got:\n%v", got)
	}
}

// A basename collision has no unambiguous bare name; both relative imports stay
// as written rather than guessing.
func TestSave_RelativeSpecifier_CollisionStaysRelative(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "a/state.js", `export const one = 1;`)
	seedJS(t, db, "b/state.js", `export const two = 2;`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "a/game.js",
		Body: `import { one } from './state.js';
export function run(){ return one; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("save should land, got %+v", res)
	}
	got, _ := store.ArtifactGet(db, "file", "a/game.js")
	if got == nil || !contains(got.Body, "from './state.js'") {
		t.Fatalf("a colliding-basename import must stay relative, got:\n%v", got)
	}
}

// A patch that introduces a relative import gets the same normalization, and the
// rewrite is visible in the echo so the model's held bytes stay honest.
func TestPatch_RelativeSpecifier_Normalized(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "js/world.state.js", `export let net = null;
export function setNet(n){ net = n; }`)
	seedJS(t, db, "js/world.hud.js", `export function draw(){ return 1; }`)

	res, err := (&ArtPatch{
		Kind:     "file",
		FilePath: "js/world.hud.js",
		Patches: []map[string]any{{
			"search":  "export function draw(){ return 1; }",
			"replace": "import { net } from './world.state.js';\nexport function draw(){ return net; }",
		}},
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("patch should land, got %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	rws, _ := data["normalized_specifiers"].([]string)
	if len(rws) != 1 {
		t.Fatalf("patch echo should report the specifier rewrite, got %+v", data["normalized_specifiers"])
	}
	got, _ := store.ArtifactGet(db, "file", "js/world.hud.js")
	if got == nil || !contains(got.Body, "from 'world.state'") {
		t.Fatalf("patched body should carry the bare specifier, got:\n%v", got)
	}
	if strings.Contains(got.Body, "./world.state.js") {
		t.Fatalf("relative specifier must be gone after normalization, got:\n%s", got.Body)
	}
}
