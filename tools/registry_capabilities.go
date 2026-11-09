package tools

import (
	"fmt"
	"sort"
	"strings"

	"openfabrica/providers/llm"
)

// Availability classifies why a tool is or isn't callable, so the engine tool
// loop can give the model an actionable message instead of always telling it to
// load a schema that gating will keep dropping.
type Availability int

const (
	// AvailUnknown: no tool with this name is registered.
	AvailUnknown Availability = iota
	// AvailLoadable: registered and passes capability + stage gating; the model
	// just hasn't loaded its schema via find_tool yet.
	AvailLoadable
	// AvailWrongStage: registered but not exposed in the current stage.
	AvailWrongStage
	// AvailMissingCapability: registered but its required provider/capability
	// is not configured for this project.
	AvailMissingCapability
)

// Availability reports whether name is callable and, if not, why. stage gates by
// per-tool Stages (empty = no stage filter).
func (r *Registry) Availability(name string, caps llm.Capabilities, stage string) Availability {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return AvailUnknown
	}
	if !caps.Has(t.RequiresCapability) {
		return AvailMissingCapability
	}
	if !t.AvailableInStage(stage) {
		return AvailWrongStage
	}
	return AvailLoadable
}

// AnyCallable reports whether any of the named tools is callable under caps, and names
// the ones that are not with the provider each needs. An empty list is always callable,
// and an unknown name counts as callable so a stale reference cannot hide a working
// feature. Used to decide whether a capability guide is worth showing at all.
func (r *Registry) AnyCallable(names []string, caps llm.Capabilities) (alive bool, dead []string) {
	alive = len(names) == 0
	for _, name := range names {
		def, err := r.Get(strings.ToLower(strings.TrimSpace(name)))
		if err != nil || caps.Has(def.RequiresCapability) {
			alive = true
			continue
		}
		dead = append(dead, fmt.Sprintf("%s (needs a %s provider)", def.Name, def.RequiresCapability))
	}
	return alive, dead
}

// ExplainUnavailable returns an actionable reason a registered tool can't be loaded right
// now, or "" when the tool is loadable or unknown (the caller treats "" as not_found).
// find_tool uses this so a capability-gated tool reads as "exists but needs configuration
// X" instead of a bare "not found".
func (r *Registry) ExplainUnavailable(name string, caps llm.Capabilities, stage string) string {
	switch r.Availability(name, caps, stage) {
	case AvailMissingCapability:
		cap := ""
		if d, err := r.Get(name); err == nil {
			cap = d.RequiresCapability
		}
		return capabilityGuidance(cap)
	case AvailWrongStage:
		return fmt.Sprintf("registered, but not offered during the %q stage; do not use it here.", stage)
	default:
		return "" // loadable or unknown
	}
}

// capabilityGuidance maps a RequiresCapability tag to a one-line, actionable
// explanation the model can act on: what is missing, then what to do instead.
// The two halves are composed rather than written out per site so the fallback
// has one home and reads the same wherever it is surfaced.
func capabilityGuidance(capability string) string {
	return capabilityMissing(capability) + " " + CapabilityFallback(capability)
}

// capabilityMissing names the provider a capability needs and states that it is
// not configured.
func capabilityMissing(capability string) string {
	switch capability {
	case "embeddings":
		return "requires an embeddings provider, but no LLM provider has an embedding model configured."
	case "image":
		return "requires an image-generation provider, which is not configured."
	case "image_source":
		return "transforms an image already in /files, and nothing here can put one there (no image-generation or stock-photo provider)."
	case "search":
		return "requires a web-search provider, which is not configured."
	case "stock":
		return "requires a stock-photo provider, which is not configured."
	case "push":
		return "requires Web Push to be configured, which it is not."
	default:
		if capability != "" {
			return fmt.Sprintf("requires the %q capability/provider, which is not configured for this project.", capability)
		}
		return "needs a provider/capability that is not configured for this project."
	}
}

// CapabilityFallback returns what to build instead when a capability is absent.
// Stated as a positive instruction, not an absence: an unconfigured provider is
// a settled fact, and the useful half is the alternative. Exported so the build
// seed can carry the same line up front, before anything goes looking.
func CapabilityFallback(capability string) string {
	switch capability {
	case "embeddings":
		return "Build a non-retrieval alternative, such as keyword search over the table, or ask the owner to configure one (load manage_chat via find_tool, then manage_chat action=ask): set embedding_model on the existing provider, add an OpenAI provider with text-embedding-3-small, or Ollama with nomic-embed-text. Do not keep searching; the tool stays hidden until embeddings exist."
	case "image", "image_source", "stock":
		return "Author the artwork yourself as inline SVG, a CSS gradient, or a canvas drawing, sized for where it sits. Do not search for a generator; there is none."
	case "search":
		return "Proceed without live web search, using content you write into the project."
	case "push":
		return "Use in-app notifications instead."
	default:
		return "Ask the owner to configure it, or build the feature a different way."
	}
}

// GatedMatches ranks the tools a query WOULD have matched if their capability
// were configured, each with the reason and the fallback to take instead. A
// search that returns nothing reads as "try different words", so the miss path
// uses this to answer "that capability is off here, do this instead" and end the
// search rather than leaving the model to reword the query.
func (r *Registry) GatedMatches(query string, caps llm.Capabilities, limit int, stage string) []ToolUnavailable {
	hits := r.keywordRankFiltered(query, stage, func(t *ToolDef) bool {
		return !caps.Has(t.RequiresCapability) && t.AvailableInStage(stage)
	})
	limit = clampLimit(limit)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]ToolUnavailable, 0, len(hits))
	for _, h := range hits {
		out = append(out, ToolUnavailable{Name: h.name, Reason: r.ExplainUnavailable(h.name, caps, stage)})
	}
	return out
}

// MissingCapabilities lists the capability tags this install cannot serve that
// some registered, stage-available tool needs, each with its fallback. Fixed for
// a given configuration, so a search that matched nothing can name what is off
// even when the query hit no tool text at all.
func (r *Registry) MissingCapabilities(caps llm.Capabilities, stage string) []ToolUnavailable {
	r.mu.RLock()
	seen := map[string]bool{}
	var tags []string
	for _, name := range r.sortedNames() {
		t := r.tools[name]
		if t.RequiresCapability == "" || caps.Has(t.RequiresCapability) || !t.AvailableInStage(stage) {
			continue
		}
		if !seen[t.RequiresCapability] {
			seen[t.RequiresCapability] = true
			tags = append(tags, t.RequiresCapability)
		}
	}
	r.mu.RUnlock()
	sort.Strings(tags)
	out := make([]ToolUnavailable, 0, len(tags))
	for _, tag := range tags {
		out = append(out, ToolUnavailable{Name: tag, Reason: capabilityGuidance(tag)})
	}
	return out
}
