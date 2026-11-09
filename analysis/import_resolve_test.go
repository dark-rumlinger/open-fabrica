package analysis

import "testing"

// ResolveNamedImports is a test convenience: ResolveNamedImportsWith against a
// freshly analyzed ModuleFacts for the same bodies. Production checks many focus
// modules over one body set and calls ResolveNamedImportsWith directly to bundle
// once.
func ResolveNamedImports(focus string, bodies map[string]string) []ImportIssue {
	return ResolveNamedImportsWith(focus, bodies, AnalyzeModules(bodies))
}

// A representative regression: sim.state.js exports setState/getState
// and a DEFAULT state, but sim.terrain.js imports the NAMED { state }. The
// runtime probe caught this stages late; this gate catches it at save time.
func TestResolveNamedImports_NamedMissingButDefaultExists(t *testing.T) {
	bodies := map[string]string{
		"sim.state.js": `const state = {};
export function setState(p) {}
export function getState() { return state; }
export default state;`,
		"sim.terrain.js": `import * as THREE from 'three';
import { state, setState } from './sim.state.js';
export class Terrain {}`,
	}
	issues := ResolveNamedImports("sim.terrain.js", bodies)

	var missing *ImportIssue
	for i := range issues {
		if issues[i].Reason == "name_missing" {
			missing = &issues[i]
		}
		if issues[i].Imported == "setState" {
			t.Fatalf("setState IS exported, must not be flagged: %+v", issues[i])
		}
	}
	if missing == nil {
		t.Fatalf("expected a name_missing issue for `state`, got %+v", issues)
	}
	if missing.Imported != "state" || missing.Target != "sim.state.js" {
		t.Fatalf("wrong issue: %+v", *missing)
	}
	if !missing.HasDefault {
		t.Errorf("expected HasDefault=true so the steer can suggest the default import")
	}
	if !contains(missing.Exports, "setState") || !contains(missing.Exports, "getState") {
		t.Errorf("expected target export surface in the issue, got %v", missing.Exports)
	}
	// `import * as THREE from 'three'` is an external namespace import: no named
	// requirement, must not appear.
	for _, is := range issues {
		if is.Spec == "three" {
			t.Errorf("external bare import must not be flagged: %+v", is)
		}
	}
}

// Mid-split: the sibling is not saved yet. This is the normal build flow and must
// be reported as target_absent (advisory), never name_missing (a reject).
func TestResolveNamedImports_TargetNotSavedYetIsAdvisory(t *testing.T) {
	bodies := map[string]string{
		"sim.terrain.js": `import { state } from './sim.state.js';`,
		// sim.state.js intentionally absent
	}
	issues := ResolveNamedImports("sim.terrain.js", bodies)
	if len(issues) != 1 || issues[0].Reason != "target_absent" {
		t.Fatalf("expected one target_absent issue, got %+v", issues)
	}
}

// A correctly wired graph produces no issues, including a re-export and an alias.
func TestResolveNamedImports_CleanGraph(t *testing.T) {
	bodies := map[string]string{
		"state.js":   `export const state = {}; export function setState() {}`,
		"reexp.js":   `export { state } from './state.js';`,
		"consume.js": `import { state as s, setState } from './state.js';\nimport { state } from './reexp.js';`,
	}
	if issues := ResolveNamedImports("consume.js", bodies); len(issues) != 0 {
		t.Fatalf("clean graph should yield no issues, got %+v", issues)
	}
}

// A LOCAL export * is followed by the linker: a name absent from the whole local
// star chain IS flagged - a real ReferenceError the conservative regex era
// silenced. External-star barrels stay silent (see modulefacts_test.go).
func TestResolveNamedImports_LocalExportStarFollowed(t *testing.T) {
	bodies := map[string]string{
		"barrel.js": `export * from './impl.js';`,
		"impl.js":   `export const a = 1;`,
		// `a` is genuinely re-exported via the local star: clean.
		"good.js": `import { a } from './barrel.js'; console.log(a);`,
		// `anything` exists nowhere in the local star chain: flagged.
		"bad.js": `import { anything } from './barrel.js'; console.log(anything);`,
	}
	if issues := ResolveNamedImports("good.js", bodies); len(issues) != 0 {
		t.Fatalf("a name re-exported via a local export * must resolve clean, got %+v", issues)
	}
	issues := ResolveNamedImports("bad.js", bodies)
	if len(issues) != 1 || issues[0].Reason != "name_missing" || issues[0].Imported != "anything" {
		t.Fatalf("a name absent from the local star chain must be flagged, got %+v", issues)
	}
}

// A commented-out import must never register (the gate masks comments).
func TestResolveNamedImports_CommentedImportIgnored(t *testing.T) {
	bodies := map[string]string{
		"a.js":       `export const x = 1;`,
		"consume.js": "// import { ghost } from './a.js';\nimport { x } from './a.js';",
	}
	if issues := ResolveNamedImports("consume.js", bodies); len(issues) != 0 {
		t.Fatalf("commented-out import must not be flagged, got %+v", issues)
	}
}

// Producer side: editing state.js to drop the `setState` export breaks terrain.js
// which still imports it.
func TestBrokenImportersAfterExportChange(t *testing.T) {
	bodies := map[string]string{
		// new state.js body: setState removed
		"sim.state.js":   `export function getState() {}`,
		"sim.terrain.js": `import { setState } from './sim.state.js';`,
		"sim.hud.js":     `import { getState } from './sim.state.js';`,
	}
	removed := RemovedExports(`export function getState() {} export function setState() {}`, `export function getState() {}`)
	if !contains(removed, "setState") {
		t.Fatalf("RemovedExports should report setState, got %v", removed)
	}
	breaks := BrokenImportersAfterExportChange("sim.state.js", removed, bodies)
	if len(breaks) != 1 || breaks[0].Importer != "sim.terrain.js" || breaks[0].Imported != "setState" {
		t.Fatalf("expected one break (terrain/setState), got %+v", breaks)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
