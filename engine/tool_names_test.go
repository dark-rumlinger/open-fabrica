package engine

import (
	"testing"

	"openfabrica/tools"
)

// TestEngineToolNamesAreRegistered keeps the engine's name-keyed behaviour
// pointed at tools that exist: a renamed tool would otherwise silently switch
// the behaviour off.
func TestEngineToolNamesAreRegistered(t *testing.T) {
	for _, name := range engineToolNames {
		if !tools.DefaultRegistry().Has(name) {
			t.Errorf("engine keys behaviour on %q, which is not a registered tool", name)
		}
	}
}
