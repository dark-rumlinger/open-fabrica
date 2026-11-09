package content

import (
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// The defect this gate closes: a `window.__x` READ that nothing in the project ever
// assigns. The bridge gate needs the symbol on two sides, so a read written nowhere has
// no second side and sails past it - the save lands silently and the cost only arrives
// later, when a sibling closes the bridge (two files to rework instead of one) or
// post-build in the window_dead_global validator. Catching it on the write that
// introduces it is the same defect at the cheapest moment.

// seedDoc seeds a non-`file` artifact (page, layout, component) so the producer scan
// can be exercised across every kind.
func seedDoc(t *testing.T, db dbh.Handle, kind, path, body string) {
	t.Helper()
	_, _, err := store.ArtifactSave(db, &store.Artifact{
		Kind:        kind,
		Path:        path,
		Storage:     store.StorageInline,
		Body:        body,
		ContentType: "text/html",
		Scope:       "global",
		Status:      "published",
	}, "test")
	if err != nil {
		t.Fatalf("seed %s %s: %v", kind, path, err)
	}
}

// The case: a chat leaf delegates its projection to a global the entry was
// supposed to set, but nothing assigns it. Guarding the read does not make it wired, so
// the guard must not suppress the reject.
func TestSave_WindowDeadGlobal_GuardedDanglingReadRejected(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "world.state.js", `export const SHARED_STATE = {};
export function getPlayer(){ return null; }`)
	seedJS(t, db, "world.js", `import { SHARED_STATE } from 'world.state';
export function boot(){ return SHARED_STATE; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "world.chat.js",
		Body: `import { SHARED_STATE } from 'world.state';
function worldToScreen(x, y, z) {
  if (window.__projectWorld3D) {
    return window.__projectWorld3D(x, y, z);
  }
  return null;
}
export function initChat(){ return worldToScreen(0, 0, 0) || SHARED_STATE; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("a window.__ read nothing assigns must be rejected, got %+v", res)
	}
	if res.Code != "window_dead_global" {
		t.Fatalf("Code = %q, want window_dead_global", res.Code)
	}
	if !contains(res.Error, "window.__projectWorld3D") {
		t.Errorf("reject should name the dead symbol, got: %s", res.Error)
	}
	if !contains(res.Error, "world.state.js") {
		t.Errorf("reject should name the zero-import store module to move it into, got: %s", res.Error)
	}
	if !contains(res.Error, "zero-import store module") {
		t.Errorf("reject should steer to the zero-import store module fix, got: %s", res.Error)
	}
	// The obvious wrong fix (add the assignment) just closes a bridge the next gate
	// rejects, so the message must block it and carry the setter idiom instead.
	if !contains(res.Error, "do NOT fix it by adding") {
		t.Errorf("reject must block the add-the-assignment fix, got: %s", res.Error)
	}
	if !contains(res.Error, "setProjectWorld3D") {
		t.Errorf("reject should carry the live-binding setter idiom, got: %s", res.Error)
	}
}

// The false-positive floor: a producer in ANY artifact kind wires the symbol, so the
// reader must not be called dead. Pages, layouts and components are invisible to the
// JS-module scan the bridge gate uses, which is why the producer scan is its own.
func TestSave_WindowDeadGlobal_ProducerScanCoversEveryKind(t *testing.T) {
	// Each kind carries its own path/body constraints; the producer scan is what is
	// under test, not the seeding shape.
	kinds := []struct{ kind, path, body string }{
		{"page", "/", `<script>window.__hud = { show(){} };</script>`},
		{"layout", "default", `{{content}}<script>window.__hud = { show(){} };</script>`},
		{"component", "shell", `<script>window.__hud = { show(){} };</script>`},
	}
	for _, k := range kinds {
		t.Run(k.kind, func(t *testing.T) {
			db := setupBulkDB(t)
			ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
			seedDoc(t, db, k.kind, k.path, k.body)

			res, err := (&ArtSave{
				Kind:     "file",
				FilePath: "hud.reader.js",
				Body:     `export function show(){ return window.__hud.show(); }`,
			}).Run(ctx)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if res == nil || !res.Success {
				t.Fatalf("a symbol assigned by a %s must not read as dead, got %+v", k.kind, res)
			}
		})
	}

	// The `file` control: a JS sibling assigning it makes this a real cross-module
	// bridge, which is the OTHER gate's defect, not a dead read.
	t.Run("file", func(t *testing.T) {
		db := setupBulkDB(t)
		ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
		seedJS(t, db, "hud.js", `window.__hud = { show(){} };
export function boot(){ return 1; }`)

		res, _ := (&ArtSave{
			Kind:     "file",
			FilePath: "hud.reader.js",
			Body:     `export function show(){ return window.__hud.show(); }`,
		}).Run(ctx)
		if res == nil || res.Success {
			t.Fatalf("a cross-file window.__ share must still be rejected, got %+v", res)
		}
		if res.Code != "window_global_bridge" {
			t.Errorf("Code = %q, want window_global_bridge (the symbol IS assigned)", res.Code)
		}
	})
}

// A `window.__` name assigned and read in the SAME body is wired, not dead. Nothing is
// passed between modules, so neither window gate has anything to say.
func TestSave_WindowDeadGlobal_SameFileProducerNotFlagged(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "solo.js",
		Body: `window.__soloCache = {};
export function put(k, v){ window.__soloCache[k] = v; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a window.__ name confined to one file is not a dead bridge, got %+v", res)
	}
}

// Scope is `__`-prefixed only. A BARE `window.Foo` may be supplied by a classic script
// tag the static scan cannot see, so hard-blocking it at write would false-reject a
// correct build; the library-aware post-build validator owns that case.
func TestSave_WindowDeadGlobal_BareNameNotBlocked(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "chart.js",
		Body:     `export function draw(el){ return window.MysteryLib.render(el); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a bare window global must not be hard-blocked at write, got %+v", res)
	}
}

// The platform assigns its reserved globals at runtime, invisibly to the producer scan,
// so they are never dead.
func TestSave_WindowDeadGlobal_ReservedNotFlagged(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "route.js",
		Body:     `export function id(){ return window.__routeParams.id; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("platform-injected reserved globals must stay exempt, got %+v", res)
	}
}

// In-progress repairs are not trapped: a dead read already in the stored body is a
// pre-existing state this edit did not create, so an unrelated change still saves.
func TestSave_WindowDeadGlobal_PreExistingNotTrapped(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "hud.js", `export function show(){ return window.__hud.show(); }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "hud.js",
		Body: `export function show(){ return window.__hud.show(); }
export function hide(){ return 0; }`,
		Overwrite: true,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a pre-existing dead read must not trap an unrelated edit, got %+v", res)
	}
}

// The producer scan matches the bracket assignment form too. Producers are scanned
// wider than reads on purpose: a missed producer would false-reject a live wire.
func TestSave_WindowDeadGlobal_BracketAssignmentCountsAsProducer(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedDoc(t, db, "page", "/", `<script>window['__hud'] = { show(){} };</script>`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "hud.reader.js",
		Body:     `export function show(){ return window.__hud.show(); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a bracket-form assignment must count as a producer, got %+v", res)
	}
}

// A name of exactly `__` strips to an empty symbol when the message builds its setter
// example. It must still reject, not panic.
func TestSave_WindowDeadGlobal_BareUnderscoreNameDoesNotPanic(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "odd.js",
		Body:     `export function get(){ return window.__.thing; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "window_dead_global" {
		t.Fatalf("want a window_dead_global reject, got %+v", res)
	}
}

// The gate rides the shared write resolver, so a patch that introduces the same read is
// rejected and the artifact is left byte-unchanged.
func TestPatch_WindowDeadGlobal_Rejected(t *testing.T) {
	db := setupFullArtifactsDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	body := "export function show(){\n  return 0;\n}\n"
	if res, _ := artifactActionSaveP(ctx, "file", "js/hud.js", body, "", "", nil, "", false); !res.Success {
		t.Fatalf("seed save failed: %q", res.Error)
	}

	res, _ := artifactActionPatch(ctx, "file", "js/hud.js", []map[string]any{
		{"search": "  return 0;", "replace": "  return window.__hudState.count;"},
	})
	if res == nil || res.Success {
		t.Fatalf("a patch introducing a dead window.__ read must be rejected, got %+v", res)
	}
	if res.Code != "window_dead_global" {
		t.Fatalf("Code = %q, want window_dead_global", res.Code)
	}
	a, _ := store.ArtifactGet(db, "file", "js/hud.js")
	if a == nil || a.Body != body {
		t.Fatalf("a rejected patch must leave the file unchanged, got %+v", a)
	}
}
