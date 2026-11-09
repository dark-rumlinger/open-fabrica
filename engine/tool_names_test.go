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

// TestToolTraitsDeclared pins the trait-backed policy to the tools it served
// when it was a name list, so moving it onto tool metadata changed nothing.
func TestToolTraitsDeclared(t *testing.T) {
	for _, name := range []string{"manage_testing", "manage_diagnostics"} {
		if !isReadOnlyProbe(name) {
			t.Errorf("%s should be a read-only probe", name)
		}
	}
	if isReadOnlyProbe(toolFiles) {
		t.Errorf("%s writes; it must not be a probe", toolFiles)
	}
	for choice, want := range map[string]string{
		"relay": "manage_relay", "tick": "manage_tick", "authoritative": "manage_authoritative",
		"matchmaking": "manage_matchmaking", "webrtc": "manage_signaling",
	} {
		if got, ok := realtimeChoiceTool(choice); !ok || got != want {
			t.Errorf("realtimeChoiceTool(%q) = %q, %v; want %q", choice, got, ok, want)
		}
		if !isRealtimeModelTool(want) {
			t.Errorf("%s should be a realtime model tool", want)
		}
	}
	if _, ok := realtimeChoiceTool(""); ok {
		t.Error("an unlocked realtime choice settles no tool")
	}
}
