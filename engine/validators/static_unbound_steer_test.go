package validators

import (
	"strings"
	"testing"

	"openfabrica/analysis"
)

// The post-build steer must match the save gate's: when a sibling already
// imports the binding from an importmap specifier, the message carries the
// copy-paste import line, never the "define it in this module" fallback that
// misleads for a library binding.
func TestRenderUnboundRefsPrefersLibrarySteer(t *testing.T) {
	bodies := map[string]string{
		"scene.js":   "import * as THREE from 'three';\nexport const scene = new THREE.Scene();",
		"terrain.js": "export function grassColor(h) { return new THREE.Color(0.2, h, 0.1); }",
	}
	var unbound []analysis.UndefinedRef
	for _, r := range analysis.UndefinedRefs("terrain.js", bodies["terrain.js"]) {
		if r.Name == "THREE" && (r.Kind == analysis.RefMember || r.Kind == analysis.RefAssign) {
			unbound = append(unbound, r)
		}
	}
	if len(unbound) == 0 {
		t.Fatal("premise changed: the member scan no longer reports THREE")
	}
	msg := renderUnboundRefs("terrain.js", unbound, bodies,
		analysis.ExportIndex(bodies), analysis.DeclaredTopLevelNames(bodies), analysis.ModuleSpecifierIndex(bodies))
	if !strings.Contains(msg, "import * as THREE from 'three'") {
		t.Errorf("message must carry the copy-paste import line, got: %s", msg)
	}
	if strings.Contains(msg, "define it in this module") {
		t.Errorf("message must not give the project-export steer for a library binding, got: %s", msg)
	}
}
