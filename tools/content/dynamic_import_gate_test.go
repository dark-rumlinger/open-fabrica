package content

import (
	"strings"
	"testing"

	"openfabrica/analysis"
)

// The save-gate reject for a wrong-module dynamic import: explorer.js destructures
// registerDiscovery from explorer.discovery.js (which does not export it); it lives
// in explorer.state.js. The gate rejects with a did-you-mean steer, turning a
// post-build probe failure + fix-pass into a one-shot save-time fix.
func TestDynamicImportGate_FlagsWrongModuleWithSteer(t *testing.T) {
	bodies := map[string]string{
		"explorer.discovery.js": `export const CREDIT_REWARDS = {};`,
		"explorer.state.js":     `export function registerDiscovery(){}`,
		"explorer.js":           `import('./explorer.discovery.js').then(({ CREDIT_REWARDS, registerDiscovery }) => { registerDiscovery(); });`,
	}
	rej := dynamicImportResult("explorer.js", "", bodies, analysis.AnalyzeModules(bodies))
	if rej == nil || rej.Code != "dynamic_import_unresolved" {
		t.Fatalf("expected a dynamic_import_unresolved reject, got %+v", rej)
	}
	if !strings.Contains(rej.Error, "registerDiscovery") || !strings.Contains(rej.Error, "explorer.state.js") {
		t.Fatalf("expected a did-you-mean pointing at explorer.state.js, got %q", rej.Error)
	}
}

func TestDynamicImportGate_CleanWhenExported(t *testing.T) {
	bodies := map[string]string{
		"x.js": `export const A = 1;`,
		"f.js": `const p = import('./x.js').then(({ A }) => A);`,
	}
	if rej := dynamicImportResult("f.js", "", bodies, analysis.AnalyzeModules(bodies)); rej != nil {
		t.Fatalf("a correct dynamic import must not reject, got %+v", rej)
	}
}

// A pre-existing bad dynamic import that THIS edit did not introduce must not be
// trapped, so an in-progress repair of something else is left free to proceed.
func TestDynamicImportGate_NotTrappedByPreexisting(t *testing.T) {
	bad := `import('./x.js').then(({ gone }) => gone);`
	bodies := map[string]string{
		"x.js": `export const A = 1;`,
		"f.js": bad + "\nexport const extra = 1;",
	}
	if rej := dynamicImportResult("f.js", bad, bodies, analysis.AnalyzeModules(bodies)); rej != nil {
		t.Fatalf("a pre-existing dynamic miss (unchanged by this edit) must not be newly trapped, got %+v", rej)
	}
}
