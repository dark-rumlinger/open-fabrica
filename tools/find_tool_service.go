package tools

import (
	"context"
	"strings"

	"openfabrica/providers/llm"
)

// FindToolArgs is one parsed find_tool call. Names loads schemas; Query searches
// the catalogue; neither browses it.
type FindToolArgs struct {
	Names []string
	Query string
	Limit int
}

// ServiceFindTool answers one find_tool call against reg for the given stage and
// capabilities. It is the single implementation shared by the engine pipeline and
// the live-chat surface, so the two cannot drift.
//
// loaded reports whether a tool name is already in the caller's live set, so the
// names path returns only genuinely new schemas as the second value (order-stable
// for the provider cache); the caller appends them to its own tool set. embed
// enables semantic ranking of a query; a nil embed, or no embeddings capability,
// falls back to keyword search. The repeat-search guard is the caller's concern
// (it needs per-segment state); everything else lives here.
func ServiceFindTool(ctx context.Context, reg *Registry, caps llm.Capabilities, stage string, args FindToolArgs, loaded func(string) bool, embed llm.EmbedFn) (FindToolResult, []llm.ToolDef) {
	if len(args.Names) > 0 {
		args.Names = normalizeToolNames(args.Names)
		schemas := reg.LookupFull(args.Names, caps, stage)
		found := make(map[string]bool, len(schemas))
		for _, sc := range schemas {
			found[sc.Name] = true
		}
		var notFound []string
		var unavailable []ToolUnavailable
		for _, n := range args.Names {
			if found[n] {
				continue
			}
			// Distinguish "exists but capability/stage-gated" from "unknown" so a
			// hidden tool reads as an actionable config gap, not a dead end.
			if reason := reg.ExplainUnavailable(n, caps, stage); reason != "" {
				unavailable = append(unavailable, ToolUnavailable{Name: n, Reason: reason})
			} else {
				notFound = append(notFound, n)
			}
		}

		// Append only genuinely new schemas - already-loaded names come back bare
		// as a "just call it" signal, and the tool ORDER never changes within a
		// segment (the bytes the provider's cache keyed on).
		var newSchemas []llm.ToolDef
		newlyLoaded := make([]string, 0, len(schemas))
		var alreadyLoaded []string
		for _, sc := range schemas {
			if loaded != nil && loaded(sc.Name) {
				alreadyLoaded = append(alreadyLoaded, sc.Name)
				continue
			}
			newlyLoaded = append(newlyLoaded, sc.Name)
			newSchemas = append(newSchemas, sc)
		}
		var docs []ToolDoc
		if len(newlyLoaded) > 0 {
			docs = reg.LookupDocs(newlyLoaded, caps, stage)
		}
		out := FindToolResult{Success: true, Loaded: newlyLoaded, AlreadyLoaded: alreadyLoaded, Docs: docs, NotFound: notFound, Unavailable: unavailable}
		out.Note = reg.didYouMeanNote(notFound)
		return out, newSchemas
	}

	if strings.TrimSpace(args.Query) != "" {
		// With an embeddings provider, fuse semantic ranking with keyword scoring so
		// a paraphrased query still finds the right tool; without one (or on any
		// embed failure) semanticOrder is nil and HybridSearch is exactly keyword
		// Search.
		var semanticOrder []string
		if caps.HasEmbeddings && embed != nil {
			profiles := reg.EmbeddingProfiles(caps, stage)
			names := make([]string, len(profiles))
			texts := make([]string, len(profiles))
			for i, p := range profiles {
				names[i] = p.Name
				texts[i] = p.Text
			}
			semanticOrder = llm.RankToolsBySimilarity(ctx, embed, names, texts, args.Query)
		}
		matches := reg.HybridSearch(args.Query, semanticOrder, caps, args.Limit, stage)
		out := FindToolResult{Success: true, Matches: matches}
		if len(matches) == 0 {
			out.Note, out.Unavailable = searchMissVerdict(reg, args.Query, caps, args.Limit, stage)
		}
		return out, nil
	}

	// Browse: the whole stage catalogue, rendered WITHOUT when_to_use - a browse
	// asks "what exists", and every tool's full routing prose is large; name,
	// one-liner and action verbs are enough to choose, and the prose arrives with
	// the schema when the model loads what it picked.
	matches := reg.Catalogue(caps, stage)
	for i := range matches {
		matches[i].WhenToUse = ""
	}
	return FindToolResult{Success: true, Matches: matches,
		Note: "Names, one-liners and action verbs only. Load the ones you need with find_tool(names: [...]) - their schemas and when-to-use guidance arrive with them."}, nil
}

// normalizeToolNames trims, lowercases and de-duplicates requested tool names, keeping
// first-seen order: a repeated name must not append the same schema twice, and a tool
// name is lowercase by construction.
func normalizeToolNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// didYouMeanNote names the closest registered tool for each requested name that
// matched none, so a typo costs one correction, not a catalogue browse.
func (r *Registry) didYouMeanNote(notFound []string) string {
	var parts []string
	r.mu.RLock()
	names := r.sortedNames()
	r.mu.RUnlock()
	for _, n := range notFound {
		best, bestDist := "", 4
		for _, cand := range names {
			if d := editDistance(n, cand); d < bestDist {
				best, bestDist = cand, d
			}
		}
		if best != "" {
			parts = append(parts, "`"+n+"` is not a tool; did you mean `"+best+"`?")
		}
	}
	return strings.Join(parts, " ")
}

// searchMissVerdict explains a query that matched nothing: the tools it would
// have hit if their provider were configured (with the fallback each names), or
// failing that the capabilities this install lacks. Without it a miss is a bare
// success, which reads as an invitation to reword and retry.
func searchMissVerdict(reg *Registry, query string, caps llm.Capabilities, limit int, stage string) (string, []ToolUnavailable) {
	const closed = "No tool matched those words. The tool list in your system prompt is the complete set; if none of its names covers this, build it with what you have. At most one more search, on a single key word."
	gated := reg.GatedMatches(query, caps, limit, stage)
	if len(gated) > 0 {
		return "No AVAILABLE tool matches that. The tools below cover it but their provider is not configured on this install, and no search will make them appear. Take the fallback each one names.", gated
	}
	missing := reg.MissingCapabilities(caps, stage)
	if len(missing) == 0 {
		return closed, nil
	}
	return closed + " For reference, these capabilities are not configured here, so nothing that needs them can be built.", missing
}
