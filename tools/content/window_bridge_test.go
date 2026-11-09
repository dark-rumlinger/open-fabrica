package content

import (
	"os"
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// The defect this gate closes: two leaves communicating through a `window.__`
// global instead of an import. No import edge exists, so the cyclic_import and
// unresolved_import gates both pass - yet the page ships wiring that resolves
// nowhere statically (the global is read before, or without, ever being assigned)
// and can boot silently dead. The save that CLOSES the bridge must be rejected
// with window_global_bridge and steered to the zero-import store module fix. This is the
// exact bug this pins: surface reads window.__gameKeys, game.js exposes it.

// Closing from the reader side: the entry already exposes window.__gameKeys; the
// leaf save that reads it closes the bridge and is rejected.
func TestSave_WindowBridge_RejectedOnReaderClosingEdge(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "game.js", `window.__gameKeys = { up: false };
export function boot(){ return 1; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "game.surface.js",
		Body:     `export function step(){ return window.__gameKeys.up; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("a leaf bridging to the entry through window.__ must be rejected, got %+v", res)
	}
	if res.Code != "window_global_bridge" {
		t.Errorf("Code = %q, want window_global_bridge", res.Code)
	}
	if !contains(res.Error, "window.__gameKeys") {
		t.Errorf("reject should name the global, got: %s", res.Error)
	}
	if !contains(res.Error, "game.surface.js") || !contains(res.Error, "game.js") {
		t.Errorf("reject should name both coupled files, got: %s", res.Error)
	}
	if !contains(res.Error, "zero-import store module") {
		t.Errorf("reject should steer to the zero-import store module fix, got: %s", res.Error)
	}
}

// Closing from the writer side: a leaf already reads window.__gameKeys; the entry
// save that exposes it closes the bridge from the other direction and is rejected.
func TestSave_WindowBridge_RejectedOnWriterClosingEdge(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "game.surface.js", `export function step(){ return window.__gameKeys.up; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "game.js",
		Body: `window.__gameKeys = { up: false };
export function boot(){ return 1; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("exposing window.__ for a leaf that already reads it must be rejected, got %+v", res)
	}
	if res.Code != "window_global_bridge" {
		t.Errorf("Code = %q, want window_global_bridge", res.Code)
	}
}

// A window.__ name confined to ONE file is not a cross-module bridge - both the
// assignment and the read live in the same module, so nothing is passed between
// modules. The gate must not fire.
func TestSave_WindowBridge_SingleFileNotFlagged(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "app.js",
		Body: `window.__appDebug = {};
export function show(){ return window.__appDebug; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a single-file window.__ use is not a bridge and must pass, got %+v", res)
	}
}

// The auth helpers (requireRole/requireAuth/apiFetch/...) are ES-module exports
// imported by the specifier 'auth' - NOT window globals. A cross-file `window.requireRole`
// read is therefore a real bridge (the name resolves nowhere statically) and MUST be
// rejected, steering the model to `import { requireRole } from 'auth'`.
func TestSave_WindowBridge_FormerAuthGlobalRejected(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "helpers.js", `window.requireRole = function(){ return true; };
export function init(){ return 1; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "page.js",
		Body:     `export function guard(){ return window.requireRole(); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("a cross-file window.requireRole read must be rejected (auth is a module now), got %+v", res)
	}
	if res.Code != "window_global_bridge" {
		t.Errorf("Code = %q, want window_global_bridge", res.Code)
	}
}

// A browser/BOM built-in read as window.X from two files (here window.location and
// window.localStorage) is not a project bridge - the allowlist (analysis.IsJSGlobal +
// browserWindowGlobals) must keep the gate off it under the broadened match.
func TestSave_WindowBridge_BrowserBuiltinNotFlagged(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "a.js", `export function href(){ return window.location.href; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "b.js",
		Body:     `export function save(v){ window.localStorage.setItem('k', v); return window.location.pathname; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("browser built-ins (window.location/localStorage) across files must pass, got %+v", res)
	}
}

// A project's OWN bare `window.WA` namespace (a docking/shade manager published by
// a scope=global IIFE) is read across leaves. It carries no `__` prefix and is NOT
// allowlisted, so the gate must reject the leaf save that closes the bridge here
// rather than let it reach the runtime probe.
func TestSave_WindowBridge_BareProjectNamespaceRejected(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "app.global.js", `window.WA = {};
window.WA.dock = function(ids){ return ids; };`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "main.leaf.js",
		Body:     `export function boot(){ return window.WA.dock(['main']); }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("a bare project window.WA namespace bridged across files must be rejected, got %+v", res)
	}
	if res.Code != "window_global_bridge" {
		t.Errorf("Code = %q, want window_global_bridge", res.Code)
	}
	if !contains(res.Error, "window.WA") {
		t.Errorf("reject should name the bare global window.WA, got: %s", res.Error)
	}
	if !contains(res.Error, "app.global.js") || !contains(res.Error, "main.leaf.js") {
		t.Errorf("reject should name both coupled files, got: %s", res.Error)
	}
	if !contains(res.Error, "zero-import store module") {
		t.Errorf("reject should steer to the zero-import store module fix, got: %s", res.Error)
	}
}

// A platform-injected window.__ global (here __routeParams) read by TWO project
// files is NOT a project-authored bridge - the platform exposes it and pages are
// meant to read it. The allowlist must keep the gate off it.
func TestSave_WindowBridge_ReservedGlobalNotFlagged(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "home.js", `export function init(){ return window.__routeParams.id; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "detail.js",
		Body:     `export function show(){ return window.__routeParams.slug; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a platform-injected window.__ global read across files must pass, got %+v", res)
	}
}

// Drift guard: every allowlisted reserved `window.__*` name must still be injected by
// its platform source. If one is renamed or removed there, this fails - forcing the
// allowlist to be updated in lockstep so it never silently shields a name the platform
// no longer provides (which would let a real project bridge through the gate). The
// platform helper APIs (auth/ofnet/e2ee) are ES modules imported by specifier now, so
// they are no longer window globals and are not covered here; platformWindowGlobals is
// empty by design.
func TestReservedWindowGlobals_StillInjected(t *testing.T) {
	assertInjected := func(src, token string) {
		t.Helper()
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if !strings.Contains(string(b), token) {
			t.Errorf("platform global %q no longer found in %s - update the allowlist to match the platform", token, src)
		}
	}

	// reserved window.__NAME -> a platform source file (relative to this package dir).
	reserved := map[string]string{
		"routeParams":       "../../server/public/render.go",
		"ofProbeViolations": "../../probe/runtime.go",
	}
	for _, name := range store.ReservedWindowGlobalNames() {
		src, ok := reserved[name]
		if !ok {
			t.Errorf("reserved global %q has no documented injection source in this test", name)
			continue
		}
		assertInjected(src, "__"+name)
	}

	// The platform bare-global set is empty by design (all platform helpers are ES
	// modules imported by specifier). If a bare platform window global is ever
	// reintroduced, add its injection-source assertion here.
	if store.PlatformWindowGlobalCount() != 0 {
		t.Errorf("platform bare window globals expected empty (helpers are ESM modules), got %d entries - add drift-guard coverage for the new bare global", store.PlatformWindowGlobalCount())
	}
}

// A bridge already present in BOTH files is an in-progress state - editing one of
// them for an unrelated reason (here, adding a second exported function) must NOT
// be trapped, mirroring the cyclic gate's repair-safe asymmetry. The fresh-bridge
// subtraction keeps the already-coupled pair editable.
func TestSave_WindowBridge_PreExistingNotTrapped(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "game.js", `window.__gameKeys = { up: false };
export function boot(){ return 1; }`)
	seedJS(t, db, "game.surface.js", `export function step(){ return window.__gameKeys.up; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "game.js",
		Body: `window.__gameKeys = { up: false };
export function boot(){ return 1; }
export function tick(){ return 2; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("editing a file in an already-bridged pair must not be trapped, got %+v", res)
	}
}

// A save that closes SEVERAL fresh bridges at once must name every symbol AND every
// file reading each, in one reject - so the model relocates the whole shared surface in
// a single turn instead of tripping the gate once per symbol (the relocate-one-at-a-time
// loop). Here state.js reads two globals; the terrain save that also reads both closes
// both bridges together.
func TestSave_WindowBridge_NamesAllFreshSymbolsAndReaders(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}
	seedJS(t, db, "state.js", `window.__terrainEdits = [];
window.__seed = 42;
export function boot(){ return 1; }`)

	res, err := (&ArtSave{
		Kind:     "file",
		FilePath: "terrain.js",
		Body: `export function edit(){ return window.__terrainEdits.length; }
export function s(){ return window.__seed; }`,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success || res.Code != "window_global_bridge" {
		t.Fatalf("a save closing two fresh bridges must be rejected as window_global_bridge, got %+v", res)
	}
	// Both bridged symbols named in the one reject.
	if !contains(res.Error, "window.__terrainEdits") || !contains(res.Error, "window.__seed") {
		t.Errorf("reject must name EVERY fresh bridged symbol, got: %s", res.Error)
	}
	// Every file reading them named, so all readers get converted in one turn.
	if !contains(res.Error, "state.js") || !contains(res.Error, "terrain.js") {
		t.Errorf("reject must list every file reading the bridged symbols, got: %s", res.Error)
	}
	// The one-leaf invariant the model most often misses.
	if !contains(res.Error, "zero-import store module") {
		t.Errorf("reject should steer to the zero-import store module fix, got: %s", res.Error)
	}
}
