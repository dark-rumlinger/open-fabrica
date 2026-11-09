package content

import (
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// Wrong-source import AUTO-CORRECT. page.js imports { elementLabel } from the state
// module, but it actually lives in helpers.js. When exactly one sibling exports the
// name and re-pointing it makes no cycle, the platform MOVES the import to the right
// module and the write LANDS - turning a reject-retry bounce into a one-shot clean
// write (the wrong-source analog of the forgotten-import auto-wire).
func TestSave_UnresolvedImport_AutoCorrectsToRightModule(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "state.js", `export function rng(s){ return s; }`)
	seedJS(t, db, "helpers.js", `export function elementLabel(id){ return id; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "page.js",
		Body: `import { elementLabel } from './state.js';
export function label(id){ return elementLabel(id); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a single-owner, cycle-free wrong-source import must auto-correct and land, got %+v", res)
	}
	if d, ok := res.Data.(map[string]any); !ok || d["imports_resolved"] != true {
		t.Errorf("imports_resolved should be true after auto-correct, got %+v", res.Data)
	}
	// The stored body now imports from the RIGHT module, not the wrong one - and
	// by bare specifier, the platform's one import style.
	a, _ := store.ArtifactGet(db, "file", "page.js")
	if a == nil || !contains(a.Body, "from 'helpers'") {
		t.Errorf("stored body should import elementLabel from helpers by bare specifier, got:\n%v", a)
	}
	if a != nil && contains(a.Body, "from './state.js'") {
		t.Errorf("stored body should no longer import elementLabel from state.js, got:\n%s", a.Body)
	}
}

// Cycle-aware "did you mean": nms.terrain.js (a leaf) tried
// `import { state, rnd } from './nms.state.js'`, but `rnd` was defined in the entry
// nms.js (which pulls the terrain leaf in via a dynamic import()). The plain hint
// would say "did you mean import { rnd } from './nms.js'" - steering the leaf to
// import UP from the entry, deepening the leaf->entry coupling. The cycle-aware hint
// must instead steer to MOVING rnd into the zero-import state leaf the model reached
// for, which is the clean architecture.
func TestSave_UnresolvedImport_EntryExportSuggestsSharedLeaf(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	// Zero-import state leaf (imports no project module); it lacks rnd.
	seedJS(t, db, "nms.state.js", `export const state = {};`)
	// Entry: defines rnd + register/hooks, and pulls the terrain leaf in dynamically.
	seedJS(t, db, "nms.js", `import * as THREE from 'three';
export function rnd(seed){ return seed; }
export function register(s){}
export const hooks = {};
import('./nms.terrain.js');`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "nms.terrain.js",
		Body: `import { register, hooks } from './nms.js';
import { state, rnd } from './nms.state.js';
export function generatePlanet(){ return rnd(state.seed); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "unresolved_import" {
		t.Fatalf("rnd missing from the state leaf must be rejected as unresolved_import, got %+v", res)
	}
	if !contains(res.Error, "Move `rnd` into") || !contains(res.Error, "nms.state.js") {
		t.Errorf("hint should steer to moving rnd into the zero-import store module, got: %s", res.Error)
	}
	if contains(res.Error, "did you mean `import { rnd } from './nms.js';`") {
		t.Errorf("hint must NOT suggest importing the shared symbol up from the entry, got: %s", res.Error)
	}
}

// FIRST use of a declared module library without its import gets the exact
// import line, not the generic "define it in this module" steer. Before any
// sibling imports the library there is no binding to copy
// (ExplainUnboundExternalRef misses), but the plan's declaration alone proves
// the specifier - otherwise a vague first message leaves the model repeating the
// same slip in the next leaf.
func TestSave_UnboundDeclaredLibrary_FirstUseGetsImportLine(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	if _, err := db.Exec(
		`INSERT INTO of_runs (id, plan_json) VALUES ('run_1', ?)`,
		`{"external_libraries":[{"name":"three","url":"https://cdn.example/three.module.js","kind":"module","module_name":"three"}]}`,
	); err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	// No sibling imports three yet: this is the project's first touch.
	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "world.js",
		Body: `const scene = { add(){} };
export function boot(){ scene.add(new THREE.Vector3(1, 2, 3)); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "undefined_reference" {
		t.Fatalf("bare THREE must be rejected as undefined_reference, got %+v", res)
	}
	if !contains(res.Error, "import * as THREE from 'three';") {
		t.Errorf("first-use reject must carry the exact import line for the declared library, got: %s", res.Error)
	}
	if contains(res.Error, "define it in this module, or import it from the module that should own it") {
		t.Errorf("first-use reject must not fall through to the generic steer, got: %s", res.Error)
	}
}

// Ambiguity is NOT auto-corrected: when TWO siblings export the name, the platform
// can't know which the model meant, so the reject stands (with the did-you-mean
// hint) for the model to resolve deliberately - the same safety rule the
// forgotten-import auto-wire uses.
func TestSave_UnresolvedImport_AmbiguousStillRejects(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "state.js", `export const x = 1;`)
	seedJS(t, db, "helpers.js", `export function elementLabel(id){ return id; }`)
	seedJS(t, db, "widgets.js", `export function elementLabel(id){ return "w" + id; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "page.js",
		Body: `import { elementLabel } from './state.js';
export function label(id){ return elementLabel(id); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "unresolved_import" {
		t.Fatalf("an ambiguous wrong-source import (two exporters) must still reject, got %+v", res)
	}
}

// The get fresh-read escape. Once a body is recorded as delivered, a repeat
// get is stubbed; fresh=true forces a full re-serve so the model can re-anchor an
// edit when it no longer holds the bytes.
func TestGet_FreshBypassesDedup(t *testing.T) {
	db := setupBulkDB(t)
	rt := tools.NewReadTracker()
	ctx := &tools.ToolContext{DB: db, ReadTracker: rt}
	seedJS(t, db, "viewport.js", "a\nb\nc\nd")
	a, _ := store.ArtifactGet(db, "file", "viewport.js")
	rt.RecordDelivered(tools.ReadCacheKey("file", "viewport.js"), tools.ReadFingerprint(a.Body))

	// Default: stubbed (residence still believed).
	res, _ := (&ArtGet{Kind: "file", FilePath: "viewport.js"}).Run(ctx)
	if d := res.Data.(map[string]any); d["unchanged"] != true || d["body"] != nil {
		t.Fatalf("expected a stub without fresh, got %+v", d)
	}
	// fresh=true: full re-serve.
	res, _ = (&ArtGet{Kind: "file", FilePath: "viewport.js", Fresh: true}).Run(ctx)
	if d := res.Data.(map[string]any); d["body"] == nil || d["unchanged"] != nil {
		t.Fatalf("fresh=true must re-serve in full, got %+v", d)
	}
}

// A patch whose anchor matches nothing applies nothing and fails atomically
// (patch_anchor_not_found) rather than reporting a success with an empty
// not_found - a half-applied patch is the shape that corrupts a file silently.
// It still Forgets the read residence so the follow-up get re-serves in full,
// letting the model re-anchor without needing fresh=true.
func TestPatch_NoMatchFailsAtomicAndReservesNextGet(t *testing.T) {
	db := setupBulkDB(t)
	rt := tools.NewReadTracker()
	ctx := &tools.ToolContext{DB: db, ReadTracker: rt}
	seedJS(t, db, "x.js", "const a = 1;\nconst b = 2;\n")
	a, _ := store.ArtifactGet(db, "file", "x.js")
	rt.RecordDelivered(tools.ReadCacheKey("file", "x.js"), tools.ReadFingerprint(a.Body))

	// A patch whose anchor does not exist: atomic failure, nothing changed.
	res, _ := (&ArtPatch{
		Kind:     "file",
		FilePath: "x.js",
		Patches: []map[string]any{
			{"search": "DOES_NOT_EXIST", "replace": "WHATEVER"},
		},
	}).Run(ctx)
	if res == nil || res.Success || res.Code != "patch_anchor_not_found" {
		t.Fatalf("a no-match patch must fail atomically with patch_anchor_not_found, got %+v", res)
	}
	// The next get must re-serve in full (residence was Forgotten).
	g, _ := (&ArtGet{Kind: "file", FilePath: "x.js"}).Run(ctx)
	if d := g.Data.(map[string]any); d["body"] == nil || d["unchanged"] != nil {
		t.Fatalf("after a failed patch, get must re-serve in full, got %+v", d)
	}
}
