package public

import (
	"sort"
	"strings"
	"testing"

	"openfabrica/analysis"
)

// sailingModules is a representative multi-module game split across many sibling
// files, wired the intended way: every sibling is imported by its bare logical
// name, never a relative path. The platform owns the path mapping (importmap), so
// the model authors zero ./ or ../ specifiers - relative specifiers are what drive
// the repeated get/patch reconciliation loops this wiring is designed to avoid.
//
// "three" appears as an external library import (water.js) to prove an external
// specifier is never mistaken for an internal module.
var sailingModules = map[string]string{
	"js/constants.js":     `export const TILE = 16; export const SEA = 0x1188cc;`,
	"js/noise.js":         `export function noise2d(x, y) { return (x ^ y) & 1; }`,
	"js/state.js":         `import { TILE } from "constants"; export const state = { tile: TILE, treasures: 0 };`,
	"js/persistence.js":   `import { state } from "state"; export function save() { return JSON.stringify(state); }`,
	"js/world-gen.js":     `import { noise2d } from "noise"; import { TILE } from "constants"; export function makeWorld() { return noise2d(0, 0) + TILE; }`,
	"js/water.js":         `import * as THREE from "three"; import { SEA } from "constants"; export function makeWater() { return new THREE.Color(SEA); }`,
	"js/sky.js":           `import { TILE } from "constants"; export function makeSky() { return TILE; }`,
	"js/boat.js":          `import { state } from "state"; export function makeBoat() { return state.tile; }`,
	"js/sailing.js":       `import { state } from "state"; export function trim() { return state; }`,
	"js/weather.js":       `import { state } from "state"; export function tickWeather() { return state; }`,
	"js/island-render.js": `import { makeWorld } from "world-gen"; export function renderIslands() { return makeWorld(); }`,
	"js/collectibles.js":  `import { state } from "state"; import { makeWorld } from "world-gen"; export function spawn() { return state.treasures + makeWorld(); }`,
	"js/camera.js":        `import { state } from "state"; export function makeCamera() { return state; }`,
	"js/hud.js":           `import { state } from "state"; export function drawHud() { return state.treasures; }`,
	"js/journal.js":       `import { state } from "state"; import { save } from "persistence"; export function openJournal() { return save(); }`,
	"js/input.js":         `import { state } from "state"; export function bindInput() { return state; }`,
	// Entry: imports the leaves by bare name, exports its own boot(). Nobody
	// imports main, so it is the sole entry script.
	"js/main.js": `import { state } from "state"; import { makeWorld } from "world-gen"; import { makeWater } from "water"; import { makeSky } from "sky"; import { makeBoat } from "boat"; import { trim } from "sailing"; import { tickWeather } from "weather"; import { renderIslands } from "island-render"; import { spawn } from "collectibles"; import { makeCamera } from "camera"; import { drawHud } from "hud"; import { openJournal } from "journal"; import { bindInput } from "input"; export function boot() { return [state, makeWorld, makeWater, makeSky, makeBoat, trim, tickWeather, renderIslands, spawn, makeCamera, drawHud, openJournal, bindInput]; }`,
}

// TestSailingWiring_ModelAuthorsNoPaths is the core Move-1 proof: the platform
// can wire a real multi-module game from logical names alone. If this holds, the
// model never has to author - or reconcile - a single import path.
func TestSailingWiring_ModelAuthorsNoPaths(t *testing.T) {
	// The fixture is the source of truth for "what the model writes". Assert it
	// contains ZERO relative specifiers and uses bare names throughout.
	bareCount := 0
	for path, body := range sailingModules {
		for _, spec := range analysis.ExtractImportSpecifiers(body) {
			if analysis.IsRelativeSpecifier(spec) || strings.HasPrefix(spec, "/") {
				t.Fatalf("%s authors a path-style import %q; the model should only write bare logical names", path, spec)
			}
			bareCount++
		}
	}
	if bareCount == 0 {
		t.Fatal("fixture has no imports; nothing is being proven")
	}
}

// TestSailingWiring_ImportMap proves the importmap exposes every internal module
// under its bare logical name, pointing at the real /files path - the mapping
// that lets `import { makeWorld } from "world-gen"` resolve in the browser.
func TestSailingWiring_ImportMap(t *testing.T) {
	paths := sortedKeys(toBoolSet(sailingModules))
	entries := projectModuleImportEntries(paths, nil)

	// Two entries per module: its logical name and its full basename. Both forms
	// ship so that an import resolves whether it names the module or the file - the
	// browser is handed an entry for exactly what the save gate accepts, which is
	// what stops the gate and the browser disagreeing about what resolves.
	if len(entries) != 2*len(sailingModules) {
		t.Fatalf("expected both specifier forms per module (%d), got %d", 2*len(sailingModules), len(entries))
	}
	spotCheck := map[string]string{
		"world-gen":        "/files/js/world-gen.js",
		"world-gen.js":     "/files/js/world-gen.js",
		"island-render":    "/files/js/island-render.js",
		"island-render.js": "/files/js/island-render.js",
		"state":            "/files/js/state.js",
		"state.js":         "/files/js/state.js",
		"main":             "/files/js/main.js",
		"main.js":          "/files/js/main.js",
	}
	for name, want := range spotCheck {
		if got := entries[name]; got != want {
			t.Errorf("importmap[%q] = %q, want %q", name, got, want)
		}
	}
	// "three" is an external lib, not a project path, so it is not in the
	// internal-module entries (loadImportMap adds it from plan.external_libraries).
	if _, present := entries["three"]; present {
		t.Error(`"three" must not be an internal-module importmap entry`)
	}
	// Version suffix is appended when present (cache-busting imported leaves).
	versioned := projectModuleImportEntries([]string{"js/world-gen.js"}, assetVersions{"js/world-gen.js": "?v=abc123"})
	if versioned["world-gen"] != "/files/js/world-gen.js?v=abc123" {
		t.Errorf("version suffix not applied: %q", versioned["world-gen"])
	}
}

// A versioned map must also carry a URL-key entry per module, remapping the
// UNVERSIONED /files URL (what a stray relative './x.js' import resolves to)
// onto the versioned URL the bare form loads. Without it the browser keys the
// module map on two different URLs and instantiates the module twice - forking
// every live-binding singleton. Unversioned maps need no such entry (the URLs
// are already identical).
func TestImportMap_RelativeImportsCollapseOntoVersionedURL(t *testing.T) {
	versioned := projectModuleImportEntries([]string{"js/world.state.js", "js/world.net.js"}, assetVersions{"js/world.state.js": "?v=abc123", "js/world.net.js": "?v=abc123"})
	for _, p := range []string{"js/world.state.js", "js/world.net.js"} {
		if got := versioned["/files/"+p]; got != "/files/"+p+"?v=abc123" {
			t.Errorf("importmap[%q] = %q, want the versioned URL", "/files/"+p, got)
		}
	}
	// Colliding basenames lose their bare entries but keep the URL-key remap -
	// relative reach is exactly how they are imported, so it matters most there.
	collided := projectModuleImportEntries([]string{"a/state.js", "b/state.js"}, assetVersions{"a/state.js": "?v=1", "b/state.js": "?v=1"})
	if _, ok := collided["state"]; ok {
		t.Error("colliding basename must not get a bare entry")
	}
	if collided["/files/a/state.js"] != "/files/a/state.js?v=1" || collided["/files/b/state.js"] != "/files/b/state.js?v=1" {
		t.Errorf("colliding modules must keep URL-key remaps, got %v", collided)
	}
	unversioned := projectModuleImportEntries([]string{"js/world.state.js"}, nil)
	if _, ok := unversioned["/files/js/world.state.js"]; ok {
		t.Error("unversioned map must not carry redundant self-mapping URL keys")
	}
}

func toBoolSet(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
