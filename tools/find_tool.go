package tools

import (
	"openfabrica/providers/llm"
)

// FindToolName is the reserved name of the meta-tool the engine tool loop
// intercepts to surface deferred tool schemas to the LLM. It is the only tool
// exposed on every stage that calls tools; everything outside the preload tiers
// (see CoreBuildToolNames) is browsed and loaded on demand through this call.
const FindToolName = "find_tool"

// FindToolDef returns the llm.ToolDef for the find_tool meta-tool. It is
// always exposed to the model, on every stage that emits tool calls. The
// engine tool loop dispatches it locally (no handler in the registry) by
// calling Registry.LookupFull / Registry.Search and appending the returned
// schemas to the live tool set.
func FindToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        FindToolName,
		Description: "Loads other tools so you can call them. Your loadable tools are named in the system prompt: the '## Tool catalogue' (LIVE) or the '## Deferred capabilities' note (BUILD, where your planned tools are already loaded); a no-argument call lists them all. names:[\"manage_api\", ...] makes those tools callable for the rest of the conversation - their parameter schemas then appear in your tool list, so this returns only the when-to-use guidance and call examples. A name already in your tool list comes back as already_loaded - just call it. query:\"send email\" searches the catalogue and returns ranked matches without schemas - a fallback when you cannot name the tool. A real tool needing an unconfigured provider comes back under `unavailable` with a reason - take the stated fallback. `not_found` means no such tool exists. A search that matches nothing returns a `note` with what to do instead: act on it, because rewording the same search is refused.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"names": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Exact tool names to load.",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "Keyword search; returns matches without schemas.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max matches for a query (default 10, max 25).",
				},
			},
		},
	}
}

// FindToolResult is the payload the engine tool loop returns from a find_tool
// call. When the caller supplied names: Loaded lists the tools made callable
// (their schemas go straight into the live tool set, NOT echoed here - a second
// copy re-billed every turn) and Docs carries the when-to-use guidance and
// action examples the wire schema's 130-char description can't hold. Matches is
// populated for a keyword query or browse mode (no args returns the full stage
// catalogue). AlreadyLoaded lists requested names whose schemas were already
// live (loaded earlier this build, e.g. via warm-start); their docs are NOT
// re-emitted, so a redundant find_tool returns near-empty, signalling "already
// callable, just call it".
//
// Note carries the verdict on a search that matched nothing. Every other field
// is omitempty, so without it a miss serialises to a bare success, which reads
// as "try different words" and buys a reworded retry. It is always set when
// Matches is empty.
type FindToolResult struct {
	Success       bool              `json:"success"`
	Loaded        []string          `json:"loaded,omitempty"`
	AlreadyLoaded []string          `json:"already_loaded,omitempty"`
	Docs          []ToolDoc         `json:"docs,omitempty"`
	Matches       []CatalogueEntry  `json:"matches,omitempty"`
	NotFound      []string          `json:"not_found,omitempty"`
	Unavailable   []ToolUnavailable `json:"unavailable,omitempty"`
	Note          string            `json:"note,omitempty"`
	Error         string            `json:"error,omitempty"`
}

// ToolUnavailable explains why a requested tool name exists in the registry but
// cannot be loaded right now - it needs a provider/capability that isn't
// configured, or it isn't offered in the current stage. Distinct from NotFound
// (a name that matches no registered tool) so the model knows the feature is
// real but blocked on configuration, and can surface that to the owner instead
// of fruitlessly re-searching.
type ToolUnavailable struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}
