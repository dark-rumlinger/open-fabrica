package engine

// Tool names the engine keys specific behaviour on (edit-shape counters, the
// task-completion count, plan-coherence checks). Named once here so a rename is
// one edit, and TestEngineToolNamesAreRegistered fails the build if a constant
// stops naming a registered tool. Policy that generalises across tools is a
// declared trait instead (see tool_traits.go); the keyword inference table in
// build_tools.go is the one deliberate central list of names.
const (
	toolFiles         = "manage_files"
	toolTasks         = "manage_tasks"
	toolAuth          = "manage_auth"
	toolOAuth         = "manage_oauth"
	toolAuthoritative = "manage_authoritative"
)

// engineToolNames lists the constants above for the registration guard.
var engineToolNames = []string{toolFiles, toolTasks, toolAuth, toolOAuth, toolAuthoritative}
