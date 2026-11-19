package analysis

import "testing"

// UnresolvedModuleSpecifiers flags a BARE import provided by neither a project
// module nor a declared importmap library - the runtime "Failed to resolve module
// specifier" that esbuild and the page probe miss. Subpath PREFIX specifiers are honored.
func TestUnresolvedModuleSpecifiers(t *testing.T) {
	bodies := map[string]string{
		"files/app.js":       `import * as THREE from 'three'; import { OrbitControls } from 'three/addons/controls/OrbitControls.js'; import { state } from './app.state.js';`,
		"files/app.view.js":  `import { createHelper } from 'missing-lib'; import { state } from './app.state.js';`,
		"files/app.state.js": `export const state = {};`,
	}
	// three + its addons prefix are declared; missing-lib is NOT.
	libSpecs := []string{"three", "three/addons/"}

	got := UnresolvedModuleSpecifiers(bodies, libSpecs)
	if len(got) != 1 || got[0].File != "files/app.view.js" || got[0].Spec != "missing-lib" {
		t.Fatalf("expected exactly the unresolved `missing-lib` import, got %+v", got)
	}

	// Declaring it (and any other shape) clears the finding: exact match for the
	// lib, prefix match for the addon, relative imports skipped, project module
	// specifier covered.
	if g := UnresolvedModuleSpecifiers(bodies, []string{"three", "three/addons/", "missing-lib"}); len(g) != 0 {
		t.Fatalf("a declared specifier must be covered, got %+v", g)
	}

	// A bare specifier that names a PROJECT module (logical-name import) is covered
	// without any library declaration.
	proj := map[string]string{
		"files/main.js":   `import { tick } from 'engine';`,
		"files/engine.js": `export function tick() {}`,
	}
	if g := UnresolvedModuleSpecifiers(proj, nil); len(g) != 0 {
		t.Fatalf("a bare specifier naming a project module must be covered, got %+v", g)
	}

	// A comment-only reference must never register.
	cmt := map[string]string{"files/a.js": "// import x from 'ghost-lib'\nexport const a = 1;"}
	if g := UnresolvedModuleSpecifiers(cmt, nil); len(g) != 0 {
		t.Fatalf("a commented-out import must not be flagged, got %+v", g)
	}
}
