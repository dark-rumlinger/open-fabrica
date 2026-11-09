package project

import (
	"strings"

	"openfabrica/pack"
	"openfabrica/store"
	"openfabrica/tools"
)

// manage_guides is the agent's READ-ONLY window onto the platform knowledge
// packs (the embedded capability guides). list/recall/search let the build/live
// agent pull a contract right before it writes the matching artifact. Guides are
// platform reference material shipped in the repo (knowledge/guides/) - they are not
// editable at runtime. Anything project-specific (owner preferences, this
// project's data/brand, a lesson for THIS build) belongs in manage_memory.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_guides",
		Category: "project",
		// recall returns one guide's full body (contracts transcluded). The biggest
		// reference guides (realtime_signaling, e2ee, 3d_rendering) far exceed the
		// session's default tool-result cap, so without this the model recalls a
		// fragment of the contract it's meant to author from. Headroom over the
		// largest transcluded guide body + JSON envelope (guarded by a test).
		// NOTE: realtime_signaling is the largest guide; splitting it into per-model
		// guides (relay/tick/authoritative/matchmaking) would let this cap drop again.
		MaxResult: 65536,
		OneLiner:  "Read the platform guides: load-bearing build contracts and reference docs for every capability. Read-only.",
		WhenToUse: `recall slug=<slug> returns a guide's core plus the list of its extended sections;
section=<anchor> pulls one of those sections, section=all the whole guide. list shows every
slug, search finds one by keyword when you do not know the slug.

Guides ship in the repo and are read-only. Anything project-specific (owner preferences, this
project's data or brand, a lesson worth keeping) belongs in manage_memory instead; application
data is manage_data and credentials are manage_secrets.`,
		Actions: []tools.Action{
			&GuideList{},
			&GuideRecall{},
			&GuideSearch{},
		},
	})
}

// capabilityPacks returns the platform capability guides (the recallable guide
// surface; stage/path/fragment packs are engine internals), minus any the
// session cannot use because no provider backs their tools.
func capabilityPacks(hidden map[string]bool) ([]pack.Pack, error) {
	ps, err := pack.Embedded()
	if err != nil {
		return nil, err
	}
	caps := pack.Capabilities(ps)
	if len(hidden) == 0 {
		return caps, nil
	}
	out := make([]pack.Pack, 0, len(caps))
	for _, g := range caps {
		if !hidden[g.Slug] {
			out = append(out, g)
		}
	}
	return out, nil
}

func guideCatalogueRows(gs []pack.Pack) []map[string]any {
	out := make([]map[string]any, 0, len(gs))
	for _, g := range gs {
		out = append(out, map[string]any{
			"slug": g.Slug, "title": g.Title, "summary": g.Summary, "category": g.Category,
		})
	}
	return out
}

// GuideList returns the platform guide catalogue (slug/title/summary/category).
type GuideList struct{}

func (a *GuideList) Doc() string {
	return "List the platform guide catalogue (slug, title, one-line summary) so you can recall a relevant guide's full body."
}
func (a *GuideList) ReadOnly() bool { return true }
func (a *GuideList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	gs, err := capabilityPacks(ctx.HiddenGuides)
	if err != nil {
		return tools.Fail("failed to list guides: %v", err), nil
	}
	return tools.Ok(map[string]any{"guides": guideCatalogueRows(gs), "count": len(gs)}), nil
}

// GuideRecall returns a guide by slug, sliced for progressive disclosure: the
// core body + section index by default, one EXTENDED section when
// section=<anchor>, or the whole guide when section=all.
type GuideRecall struct {
	Slug    string `arg:"slug,required" desc:"Guide slug to fetch (from the guide index in the system prompt, or list)."`
	Section string `arg:"section" desc:"EXTENDED section anchor from the guide's section index. Omit for the core body plus the list of sections; pass 'all' for the whole guide."`
}

func (a *GuideRecall) Doc() string {
	return "Fetch a guide by slug: core body + section index (default), one EXTENDED section (section=<anchor>), or the whole guide (section=all)."
}
func (a *GuideRecall) ReadOnly() bool { return true }
func (a *GuideRecall) Example() any {
	return &GuideRecall{Slug: "realtime_signaling", Section: "matchmaking"}
}

// resolveGuide returns the pack matching slug, tolerating the near-miss forms
// a model actually produces: surrounding whitespace, wrong case, and hyphens
// for underscores. pack.BySlug stays the strict primitive (engine prefetch is
// drift-tested against exact slugs); the tolerance lives here at the tool
// boundary where sloppy input arrives.
func resolveGuide(gs []pack.Pack, slug string) *pack.Pack {
	s := strings.TrimSpace(slug)
	if g := pack.BySlug(gs, s); g != nil {
		return g
	}
	norm := strings.ToLower(strings.ReplaceAll(s, "-", "_"))
	for i := range gs {
		if strings.ToLower(gs[i].Slug) == norm {
			return &gs[i]
		}
	}
	return nil
}

// guideSuggestions returns up to three slugs whose slug, title, or summary
// contains the missed query, for the did-you-mean line on a recall miss.
func guideSuggestions(gs []pack.Pack, missed string) []string {
	q := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(missed), "-", "_"))
	if q == "" {
		return nil
	}
	var out []string
	for _, g := range gs {
		hay := strings.ToLower(g.Slug + " " + g.Title + " " + g.Summary)
		if strings.Contains(strings.ReplaceAll(hay, "-", "_"), q) {
			out = append(out, g.Slug)
			if len(out) == 3 {
				break
			}
		}
	}
	return out
}

// maxRelatedGuides bounds the related list: bases and dependents come first,
// then category siblings fill what remains, so a large category cannot bloat
// every recall result.
const maxRelatedGuides = 5

// relatedGuides returns one-line pointers (slug + summary) for the guides a
// recalled guide leans on: its requires bases, the guides that require it, and
// its same-category siblings. A single-guide recall otherwise leaves an
// "owned by <other guide>" cross-reference dangling - this hands the model the
// slug to pull next.
func relatedGuides(gs []pack.Pack, g *pack.Pack) []map[string]string {
	seen := map[string]bool{g.Slug: true}
	var out []map[string]string
	add := func(p *pack.Pack) {
		if p == nil || seen[p.Slug] || len(out) >= maxRelatedGuides {
			return
		}
		seen[p.Slug] = true
		out = append(out, map[string]string{"slug": p.Slug, "summary": p.Summary})
	}
	for _, req := range g.Requires {
		add(pack.BySlug(gs, req))
	}
	for i := range gs {
		for _, req := range gs[i].Requires {
			if req == g.Slug {
				add(&gs[i])
			}
		}
	}
	for i := range gs {
		if gs[i].Category == g.Category {
			add(&gs[i])
		}
	}
	return out
}

func (a *GuideRecall) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	all, err := capabilityPacks(nil)
	if err != nil {
		return tools.Fail("failed to read guides: %v", err), nil
	}
	if g := resolveGuide(all, a.Slug); g != nil && ctx.HiddenGuides[g.Slug] {
		return tools.Fail("guide %s is unavailable in this configuration: the tools it covers need a provider that is not set up. Build the project without it.", g.Slug), nil
	}
	gs, err := capabilityPacks(ctx.HiddenGuides)
	if err != nil {
		return tools.Fail("failed to read guides: %v", err), nil
	}
	g := resolveGuide(gs, a.Slug)
	if g == nil {
		if sugg := guideSuggestions(gs, a.Slug); len(sugg) > 0 {
			return tools.Fail("guide not found: %s - did you mean %s? (action=list shows every slug)", a.Slug, strings.Join(sugg, ", ")), nil
		}
		return tools.Fail("guide not found: %s (action=list shows every slug, action=search finds one by keyword)", a.Slug), nil
	}
	sec := strings.TrimSpace(a.Section)
	// A guide whose core is already inlined in the session seed must not be
	// re-served in full: the core would land as a near-duplicate of text the
	// model already holds. Serve the extended half instead.
	inlined := ctx.SeedGuides[g.Slug]
	switch {
	case sec == "":
		out := map[string]any{
			"slug": g.Slug, "title": g.Title, "summary": g.Summary, "category": g.Category,
		}
		if inlined {
			out["note"] = "This guide's core is already inlined in your build context above; body omitted. Pull a section with section=<anchor>, or section=all for the extended sections."
		} else {
			// Core body + an index of what else can be pulled on demand. Transclude
			// canonical facts so the model sees the rendered contract, not a token.
			out["body"] = pack.TranscludeContracts(g.CoreBody())
		}
		if idx := g.SectionIndex(); idx != nil {
			out["sections"] = idx
			if !inlined {
				out["note"] = "This is the core. Pull a section with section=<anchor>, or section=all for the whole guide."
			}
		} else if inlined {
			out["note"] = "This guide's core is already inlined in your build context above, and it has no extended sections, so there is nothing more to pull."
		}
		if rel := relatedGuides(gs, g); len(rel) > 0 {
			out["related"] = rel
		}
		return tools.Ok(out), nil
	case strings.EqualFold(sec, "all"):
		body := g.Body
		note := ""
		if inlined {
			var parts []string
			for _, s := range g.Sections {
				parts = append(parts, s.Body)
			}
			body = strings.Join(parts, "\n\n")
			note = "This guide's core is already inlined in your build context above; these are the extended sections only."
		}
		out := map[string]any{
			"slug": g.Slug, "title": g.Title, "summary": g.Summary, "body": pack.TranscludeContracts(body), "category": g.Category,
		}
		if note != "" {
			out["note"] = note
		}
		if rel := relatedGuides(gs, g); len(rel) > 0 {
			out["related"] = rel
		}
		return tools.Ok(out), nil
	default:
		s := g.Section(sec)
		if s == nil {
			if g.AnchorList() == "" {
				if inlined {
					return tools.Fail("guide %s has no sections and its core is already inlined in your build context above", g.Slug), nil
				}
				return tools.Fail("guide %s has no sections - recall it without section= for the full body", g.Slug), nil
			}
			hint := ""
			var anchors []string
			for _, sc := range g.Sections {
				anchors = append(anchors, sc.Anchor)
			}
			if m := store.SuggestName(sec, anchors, 2); len(m) > 0 {
				hint = " Did you mean " + strings.Join(m, " or ") + "?"
			}
			return tools.Fail("section %q not found in %s (available: %s).%s", sec, g.Slug, g.AnchorList(), hint), nil
		}
		return tools.Ok(map[string]any{
			"slug": g.Slug, "section": s.Anchor, "title": s.Title, "body": pack.TranscludeContracts(s.Body),
		}), nil
	}
}

// GuideSearch returns guides matching a free-text query (keyword over the
// platform set).
type GuideSearch struct {
	Query string `arg:"query,required" desc:"Free-text query matched against slug/title/summary/body."`
}

func (a *GuideSearch) ActionName() string { return "search" }
func (a *GuideSearch) Doc() string {
	return "Search the platform guides for entries matching a query."
}
func (a *GuideSearch) ReadOnly() bool { return true }
func (a *GuideSearch) Example() any   { return &GuideSearch{Query: "three.js"} }
func (a *GuideSearch) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	gs, err := capabilityPacks(ctx.HiddenGuides)
	if err != nil {
		return tools.Fail("failed to search guides: %v", err), nil
	}
	const limit = 10
	terms := strings.Fields(strings.ToLower(a.Query))
	matched := make([]pack.Pack, 0, limit)
	// Every term must appear; when nothing matches them all, any one term will do.
	for _, all := range []bool{true, false} {
		matched = matched[:0]
		for _, g := range gs {
			if guideMatches(g, terms, all) {
				matched = append(matched, g)
			}
		}
		if len(matched) > 0 {
			break
		}
	}
	total := len(matched)
	if total > limit {
		matched = matched[:limit]
	}
	rows := make([]map[string]any, 0, len(matched))
	for _, g := range matched {
		row := map[string]any{
			"slug": g.Slug, "title": g.Title, "summary": g.Summary, "category": g.Category,
		}
		if idx := g.SectionIndex(); idx != nil {
			row["sections"] = idx
		}
		rows = append(rows, row)
	}
	out := map[string]any{"guides": rows, "count": len(rows), "mode": "keyword"}
	if total > limit {
		out["truncated"] = true
		out["total"] = total
	}
	return tools.Ok(out), nil
}

// guideMatches reports whether a guide's text contains every term (all) or at
// least one (not all). No terms matches everything.
func guideMatches(g pack.Pack, terms []string, all bool) bool {
	if len(terms) == 0 {
		return true
	}
	hay := strings.ToLower(g.Slug + " " + g.Title + " " + g.Summary + " " + g.Body)
	for _, t := range terms {
		has := strings.Contains(hay, t)
		if all && !has {
			return false
		}
		if !all && has {
			return true
		}
	}
	return all
}
