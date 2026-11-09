package engine

// Build-loop policy keyed on what a tool declares about itself (tools.Tool.Probe,
// tools.Tool.RealtimeModel) instead of on hard-coded tool names, so a new probe or
// realtime tool joins the policy by declaring the trait where it is defined.
//
// The lookups read the process tool registry: guard and preload code runs where no
// Deps are in reach, and every tool registers into the default registry at init
// (engine tests register fixtures into a Clone, which shares these defs).

import "openfabrica/tools"

// isReadOnlyProbe reports whether callName is a project probe that reads, never
// writes, project state. Its arguments are stable across retries, so a successful
// write elsewhere clears its identical-result streaks (see clearProbeStreaks): the
// write may have fixed what the probe was failing on.
func isReadOnlyProbe(callName string) bool {
	def, err := tools.DefaultRegistry().Get(callName)
	return err == nil && def.Probe
}

// isReadOnlyProbeAction is isReadOnlyProbe narrowed to one action: a probe tool that
// also has writing actions (manage_testing write_flows saves the flow script,
// test_auth_flow registers a user) is a probe only for the actions it declares
// read-only. A write there can fix what the probes were failing on, so it must clear
// their streaks, not feed them.
func isReadOnlyProbeAction(callName, action string) bool {
	def, err := tools.DefaultRegistry().Get(callName)
	if err != nil || !def.Probe {
		return false
	}
	if len(def.ReadOnlyActions) == 0 || action == "" {
		return true
	}
	return def.ReadOnlyActions[action]
}

// isRealtimeModelTool reports whether name is one of the per-model realtime
// creation tools.
func isRealtimeModelTool(name string) bool {
	def, err := tools.DefaultRegistry().Get(name)
	return err == nil && def.RealtimeModel != ""
}

// realtimeChoiceTool returns the tool that a locked realtime-model choice
// (normalizeRealtimeChoice) settles, if one declares that model.
func realtimeChoiceTool(choice string) (string, bool) {
	if choice == "" {
		return "", false
	}
	for _, def := range tools.DefaultRegistry().List() {
		if def.RealtimeModel == choice {
			return def.Name, true
		}
	}
	return "", false
}
