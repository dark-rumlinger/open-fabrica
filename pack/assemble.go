package pack

import (
	"fmt"
	"sort"
	"strings"

	"openfabrica/contracts"
	"openfabrica/knowledge"
)

// AssembleInput is the per-call bundle Assemble consumes.
type AssembleInput struct {
	Stage     string            // build | plan | live | expand
	Wave      string            // cache-stability tag stamped at the top
	RoleLine  string            // one-line role header (falls back to the stage pack title)
	Selection Selection         // the partitioned packs (from Select)
	Sentinels map[string]string // {{NAME}} substitutions in the stage body
	ToolIndex string            // compact tool catalogue for plan/live (optional)
	Extra     string            // appended verbatim to the system prompt (optional)
	// HiddenGuides are slugs to leave out of the guide index: capabilities this
	// install cannot serve. Listing one advertises a feature whose tools are all
	// gated off, which reads as an invitation to go looking for them. The caller
	// owns the rule, since only it can see the tool registry and the provider
	// capabilities.
	HiddenGuides map[string]bool
	// Conditions are what the caller knows about the project, for the stage body's
	// `<!-- when:NAME -->` blocks (see conditions.go). Unset names keep their blocks.
	Conditions map[string]bool
	// OmitLaws are contract ids whose law section the mental model drops because it
	// cannot apply to this project (see omitLaws).
	OmitLaws map[string]bool
}

// Assembled is the output: the stage's system prompt.
type Assembled struct {
	System string // wave + mental model + laws + guide index + role frame + stage body + fragments
}

// guideIndexToken is replaced in the mental model with the rendered guide index.
const guideIndexToken = "{{GUIDE_INDEX}}"

// CatalogueCompleteNote is the one-sentence guarantee shared by the build and
// live tool-catalogue preambles: the rendered deferred index is exhaustive, so a
// missing capability is truly absent and no search surfaces it. Single-homed here
// so the two preambles cannot drift.
const CatalogueCompleteNote = "This list is complete, so a capability absent from it does not exist here and no search will make one available."

// ToolCataloguePreamble introduces a rendered DeferredIndex wherever one is
// emitted. Single-homed so BUILD's Deferred-capabilities block and LIVE's Tool
// catalogue carry identical instructions (only their headings differ; BUILD's
// placement at the prompt tail is deliberate - see engine routeKeyHeadBytes).
const ToolCataloguePreamble = "The catalogue below is everything else that exists, beyond the tools already in your tool list, one line each on what it does. Load what you need with `find_tool(names: [...])` before calling it; its `action` verbs arrive with the loaded schema - pass one of those, never an invented verb. Scan the whole catalogue first and load everything you expect to need in ONE up-front call - a loaded tool stays available for the rest of the run. " + CatalogueCompleteNote

// Assemble produces a stage's system prompt from one selected pack set. The
// always-on platform layer - the mental model, the load-bearing contract "laws"
// (transcluded verbatim), and the guide index - rides at the top, identical
// across every stage and project so it is the shared cached prefix. The index
// is the full category-grouped list for PLAN and LIVE and the pointer form for
// BUILD, whose guides arrive inlined in the seed instead. The stage body and
// mental model have their `{{contract:<id>}}` tokens transcluded and their
// `{{NAME}}` sentinels substituted - both pure, so the output is a
// byte-deterministic function of the inputs (the prompt-cache contract). Guide
// BODIES are not assembled here: RenderGuidesCore and RenderGuidesDecision
// render them for the BUILD seed and the PLAN frame.
func Assemble(in AssembleInput) Assembled {
	var out Assembled

	role := in.RoleLine
	if role == "" && in.Selection.Stage != nil {
		role = in.Selection.Stage.Title
	}
	if role == "" {
		role = fmt.Sprintf("unknown stage %q", in.Stage)
	}

	var sys strings.Builder
	if in.Wave != "" {
		fmt.Fprintf(&sys, "<!-- %s -->\n\n", in.Wave)
	}

	// Always-on platform layer (mental model + laws + guide index). Project- and
	// stage-independent for plan/build/live, so it maximises the shared cache
	// prefix. EXPAND is the exception: it runs once, calls no tools, and emits a prose
	// brief, so it gets a LEAN mental model - the artifact kinds, helper modules,
	// wire-shape Platform laws, and guide index are dropped: a behaviour-level
	// rewrite never builds against a wire shape, and its frame bans the transport
	// nouns those sections carry.
	if mm := strings.TrimSpace(renderMentalModel(in.Selection.Guides, in.Stage == "expand", in.HiddenGuides, in.Stage, in.OmitLaws)); mm != "" {
		sys.WriteString(mm)
		sys.WriteString("\n\n")
	}

	fmt.Fprintf(&sys, "%s\n\n", role)

	if in.Selection.Stage != nil {
		body := substitute(applyConditions(in.Selection.Stage.Body, in.Conditions), in.Sentinels)
		sys.WriteString(strings.TrimSpace(body))
		sys.WriteString("\n")
	}
	for _, f := range in.Selection.Fragments {
		sys.WriteString("\n")
		sys.WriteString(strings.TrimSpace(substitute(applyConditions(f.Body, in.Conditions), in.Sentinels)))
		sys.WriteString("\n")
	}
	if idx := strings.TrimSpace(in.ToolIndex); idx != "" {
		sys.WriteString("\n## Tool catalogue\n\n")
		// What each line renders is Registry.DeferredIndex: name and one-liner (no
		// action verbs - those arrive with the schema), for the tools NOT already loaded. The preloaded set is left out on
		// purpose - it arrives as wire schemas plus its own routing block, so listing it
		// here again would bill the same names and verbs a third time every turn. The
		// when-to-use guidance is not inlined either (it loads via find_tool), so this
		// preamble must not promise it.
		sys.WriteString(ToolCataloguePreamble + "\n\n")
		sys.WriteString(idx)
		sys.WriteString("\n")
	}
	if extra := strings.TrimSpace(in.Extra); extra != "" {
		sys.WriteString("\n")
		sys.WriteString(extra)
		sys.WriteString("\n")
	}
	out.System = sys.String()
	return out
}

// renderMentalModel returns the always-on platform overview: mental-model.md
// with its `{{contract:<id>}}` laws transcluded and its `{{GUIDE_INDEX}}` token
// replaced by the category-grouped guide index. If the mental model is missing,
// it degrades to the bare index so guides stay discoverable. Deterministic.
func renderMentalModel(guides []Pack, lean bool, hidden map[string]bool, stage string, omit map[string]bool) string {
	body, err := knowledge.MentalModel()
	if err != nil || strings.TrimSpace(body) == "" {
		if lean {
			return ""
		}
		return "## Guide index\n\n" + guideIndexFor(guides, hidden, stage)
	}
	body = strings.TrimSpace(body)

	// EXPAND (lean): drop everything from the "The artifacts you create" section
	// down - the artifact kinds, helper modules, tool/guide shelves, wire-shape
	// Platform laws, and the guide index. That removes every {{contract:}} law and
	// the {{GUIDE_INDEX}} token in one cut (they all live below that heading), and
	// with them the endpoint/helper transport nouns the EXPAND frame bans, leaving
	// only what the platform IS - the behaviour context a brief rewrite uses.
	if lean {
		if i := strings.Index(body, "\n## The artifacts you create"); i >= 0 {
			body = strings.TrimSpace(body[:i])
		}
		return substituteContracts(body) // no-op: no tokens remain above the cut
	}

	idx := guideIndexFor(guides, hidden, stage)
	body = substituteContracts(omitLaws(body, omit))
	body = strings.ReplaceAll(body, shorthandGlossaryToken, ShorthandGlossary())
	if strings.Contains(body, guideIndexToken) {
		return strings.ReplaceAll(body, guideIndexToken, idx)
	}
	return body + "\n\n## Guide index\n\n" + idx
}

// The two forms the index takes, each with its own lead-in so neither inherits a
// sentence written for the other.
const (
	// guideIndexPointer stands in for the full list on BUILD, whose guides arrive
	// prefetched in the seed: it says the shelf exists and how to reach it, without
	// the per-guide catalogue that every turn would re-read.
	guideIndexPointer = "Any guides your plan implies are inlined below as `<guide>` blocks - that is your shelf, " +
		"and their extended sections are named under each one. For anything else, `manage_guides action=list` " +
		"names every guide on the platform and `manage_guides action=recall slug=<slug>` pulls one in."

	// liveGuideIndexPointer stands in for the full list on LIVE, which rides every
	// owner-chat turn. LIVE prefetches no guide cores, so (unlike BUILD) it names no
	// inlined shelf; it points at the recall route instead. The full per-guide
	// catalogue is dropped from LIVE because manage_guides list serves it on demand,
	// and most owner-chat turns never need a guide, so carrying it every turn is pure
	// re-read.
	liveGuideIndexPointer = "The platform has a guide for each capability. `manage_guides action=list` names every " +
		"guide and when to use it; `manage_guides action=recall slug=<slug>` pulls one into context before you build with it."

	// guideIndexPreamble leads the full list, which PLAN carries because it has no
	// tools and must pick each task's guides from the catalogue itself. No recall
	// instruction here: PLAN has no tools to recall with.
	guideIndexPreamble = "The platform's guides, grouped by category:\n\n"
)

// guideIndexFor renders the guide index in the shape this stage asked for: BUILD and
// LIVE get a short pointer (both reach guides on demand - BUILD from its prefetched
// shelf, LIVE from manage_guides recall), while PLAN carries the full catalogue it
// needs to choose each task's guides with no tools of its own.
func guideIndexFor(guides []Pack, hidden map[string]bool, stage string) string {
	switch stage {
	case "build":
		return guideIndexPointer
	case "live":
		return liveGuideIndexPointer
	default:
		return renderGuideIndex(guides, hidden)
	}
}

// renderGuideIndex renders every capability guide grouped by category - the
// always-on "what exists / when to use it" layer. Each guide is one line
// (slug + summary); categories carry their optional CATEGORY.md one-liner.
// Slugs in hidden are dropped, and a category left empty by that drops with
// them. Deterministic: categories sorted alphabetically, guides in input
// (position, slug) order.
func renderGuideIndex(guides []Pack, hidden map[string]bool) string {
	visible := make([]Pack, 0, len(guides))
	for _, g := range guides {
		if !hidden[g.Slug] {
			visible = append(visible, g)
		}
	}
	if len(visible) == 0 {
		return "_(no guides)_"
	}
	notes, _ := knowledge.CategoryNotes()
	seen := map[string]bool{}
	var cats []string
	for _, g := range visible {
		c := guideCategory(g)
		if !seen[c] {
			seen[c] = true
			cats = append(cats, c)
		}
	}
	sort.Strings(cats)

	var b strings.Builder
	b.WriteString(guideIndexPreamble)
	for _, c := range cats {
		if note := strings.TrimSpace(notes[c]); note != "" {
			fmt.Fprintf(&b, "### %s - %s\n", c, note)
		} else {
			fmt.Fprintf(&b, "### %s\n", c)
		}
		for _, g := range visible {
			if guideCategory(g) != c {
				continue
			}
			summary := g.Summary
			if summary == "" {
				summary = g.Title
			}
			fmt.Fprintf(&b, "- `%s` - %s\n", g.Slug, summary)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderGuidesCore renders each selected capability guide's CORE (the always-inline
// slice before `<!-- extended -->`, contracts transcluded), wrapped in
// <guide slug=...>, followed by a one-line index of its recall-on-demand EXTENDED
// sections. The planner locks load-bearing architecture from the core (which holds
// the `## Decision:` blocks and wire shapes - TestNoDecisionInExtended keeps them
// there); the builder pulls an extended section only on demand, avoiding the ~40%
// token cost of inlining every guide's full body. Deterministic and
// project-independent, so the block is byte-stable for the prompt cache; it rides
// the plan/build stage BODY after the shared mental-model prefix, so the
// cross-stage cache prefix is unaffected.
func RenderGuidesCore(guides []Pack) string {
	return RenderGuidesCoreLocked(guides, nil)
}

// RenderGuidesCoreLocked is RenderGuidesCore with the plan's locked decisions
// applied: a `## Decision:` block whose topic the plan already locked renders
// only its chosen branch plus the block's non-bullet mechanics - BUILD cannot
// act on the unchosen branches or the selection matrix, and the lock is
// restated as a binding constraint in the intent seed. locked maps a
// lowercased topic to its lowercased choice; nil (PLAN, the ratchets) renders
// everything. `## Choose:` blocks are BUILD-time choices and never trimmed.
func RenderGuidesCoreLocked(guides []Pack, locked map[string]string) string {
	if len(guides) == 0 {
		return "_(no guides)_"
	}
	var b strings.Builder
	seenContracts := map[string]bool{}
	for _, g := range guides {
		core := strings.TrimSpace(substituteContractsOnce(g.CoreBody(), seenContracts))
		if len(locked) > 0 {
			core = trimLockedDecisions(core, locked)
		}
		idx := g.SectionIndex()
		// A guide with neither an inline core nor recallable sections has nothing
		// to say; emitting `<guide slug="x">\n\n</guide>` would be malformed,
		// zero-guidance markup the planner would read as an empty capability.
		// TestNoEmptyGuideCores fails the build if a real guide ever trims to empty,
		// so this skip is a belt-and-suspenders runtime guard, not the first defense.
		if core == "" && len(idx) == 0 {
			continue
		}
		fmt.Fprintf(&b, "<guide slug=%q>\n%s\n", g.Slug, core)
		if len(idx) > 0 {
			fmt.Fprintf(&b, "\nMore in this guide - recall a section when you reach its case (`manage_guides action=recall slug=%s section=<anchor>`):\n", g.Slug)
			for _, s := range idx {
				fmt.Fprintf(&b, "- `%s` - %s\n", s["anchor"], s["when"])
			}
		}
		b.WriteString("</guide>\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// trimLockedDecisions drops the unchosen top-level bullets of every
// `## Decision:` block whose topic is locked, keeping the heading, the intro
// and trailer prose, and the chosen bullet, plus a one-line omission note.
// A block whose chosen bullet cannot be found (no `choice: "<x>"` token)
// renders in full - dropping the wrong branch would lose binding wire
// mechanics, so the fallback is always the whole block. Every decision bullet
// should therefore carry its choice token; TestEveryDecisionBlockIsPlanRecordable
// guards the blocks, and a tokenless bullet costs every build the full block.
func trimLockedDecisions(core string, locked map[string]string) string {
	lines := strings.Split(core, "\n")
	var out []string
	i := 0
	for i < len(lines) {
		line := lines[i]
		topic, isDecision := decisionTopic(line)
		choice, isLocked := locked[topic]
		if !isDecision || !isLocked {
			out = append(out, line)
			i++
			continue
		}
		// Collect the block: heading to the next `## ` heading or end.
		end := i + 1
		for end < len(lines) && !strings.HasPrefix(lines[end], "## ") {
			end++
		}
		block := lines[i:end]
		trimmed, ok := trimDecisionBlock(block, choice)
		if ok {
			out = append(out, trimmed...)
		} else {
			out = append(out, block...)
		}
		i = end
	}
	return strings.Join(out, "\n")
}

// decisionTopic extracts the lowercased topic of a `## Decision:` heading
// ("## Decision: realtime model (record in ...)" -> "realtime model").
func decisionTopic(line string) (string, bool) {
	const h = "## Decision:"
	if !strings.HasPrefix(line, h) {
		return "", false
	}
	t := line[len(h):]
	if j := strings.IndexByte(t, '('); j >= 0 {
		t = t[:j]
	}
	return strings.ToLower(strings.TrimSpace(t)), true
}

// trimDecisionBlock keeps the block's non-bullet lines and the one top-level
// bullet carrying `choice: "<choice>"`, in order, inserting an omission note
// after the heading. Reports false when no bullet names the choice.
func trimDecisionBlock(block []string, choice string) ([]string, bool) {
	marker := `choice: "` + choice + `"`
	type group struct {
		lines    []string
		isBullet bool
	}
	var groups []group
	for _, line := range block {
		if strings.HasPrefix(line, "- ") {
			groups = append(groups, group{lines: []string{line}, isBullet: true})
			continue
		}
		if n := len(groups); n > 0 && groups[n-1].isBullet && strings.HasPrefix(line, " ") && strings.TrimSpace(line) != "" {
			groups[n-1].lines = append(groups[n-1].lines, line)
			continue
		}
		groups = append(groups, group{lines: []string{line}})
	}
	found := false
	var out []string
	out = append(out, block[0],
		"_(Locked in your plan: `"+choice+"`. The other branches are omitted; the lock is binding.)_")
	for _, g := range groups[1:] {
		if g.isBullet && !strings.Contains(strings.Join(g.lines, "\n"), marker) {
			continue
		}
		if g.isBullet {
			found = true
		}
		out = append(out, g.lines...)
	}
	return out, found
}

// decisionHeadings are the two core headings a planner must act on: an
// architecture choice it locks into the plan's `decisions`, and an implementation
// choice BUILD makes but PLAN scopes. TestNoDecisionInExtended keeps them above the
// divider, so scanning the core finds every one.
var decisionHeadings = []string{"## Decision:", "## Choose:"}

// choiceBlocks returns each `## Decision:` / `## Choose:` block in a core, verbatim,
// from its heading to the next `## ` heading or the end of the core.
func choiceBlocks(core string) []string {
	lines := strings.Split(core, "\n")
	var out []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.TrimSpace(strings.Join(cur, "\n")))
			cur = nil
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			flush()
			for _, h := range decisionHeadings {
				if strings.HasPrefix(line, h) {
					cur = []string{line}
					break
				}
			}
			continue
		}
		if cur != nil {
			cur = append(cur, line)
		}
	}
	flush()
	return out
}

// RenderGuidesDecision renders the PLAN-time facet of each selected guide: the
// choices it forces, plus the scope of what the capability covers. PLAN emits a
// JSON plan and carries no tools, so it can neither write code nor recall a
// section - the wire shapes and build traps in a guide's core are addressed to a
// stage that can act on them, and reach the planner as instructions it cannot
// follow. What a planner must take from a guide is the block it has to lock into
// `decisions` and a sense of the capability's shape; BUILD gets the full core.
//
// Every guide's one-line summary already rides the always-on guide index, so it is
// not repeated here. Same determinism contract as RenderGuidesCore.
func RenderGuidesDecision(guides []Pack) string {
	if len(guides) == 0 {
		return "_(no guides)_"
	}
	var b strings.Builder
	for _, g := range guides {
		blocks := choiceBlocks(strings.TrimSpace(substituteContracts(g.CoreBody())))
		if len(blocks) == 0 && len(g.Sections) == 0 {
			continue
		}
		fmt.Fprintf(&b, "<guide slug=%q>\n", g.Slug)
		for _, blk := range blocks {
			b.WriteString(blk)
			b.WriteString("\n\n")
		}
		if len(g.Sections) > 0 {
			titles := make([]string, 0, len(g.Sections))
			for _, s := range g.Sections {
				titles = append(titles, s.Title)
			}
			b.WriteString("Also covers (the build stage gets the core, the rest on recall): ")
			b.WriteString(strings.Join(titles, "; "))
			b.WriteString("\n")
		}
		b.WriteString("</guide>\n\n")
	}
	if b.Len() == 0 {
		return "_(no guides)_"
	}
	return strings.TrimRight(b.String(), "\n")
}

func guideCategory(g Pack) string {
	if c := strings.TrimSpace(g.Category); c != "" {
		return c
	}
	return "general"
}

// substitute transcludes `{{contract:<id>}}` tokens (from the contracts package)
// and then replaces `{{NAME}}` sentinels. Both are pure, so the result is a
// deterministic function of the body and inputs.
func substitute(body string, sentinels map[string]string) string {
	body = substituteContracts(body)
	for k, v := range sentinels {
		body = strings.ReplaceAll(body, "{{"+k+"}}", v)
	}
	return body
}

// scanContractTokens is the ONE parser for `{{contract:id}}`, shared by
// substituteContracts (which renders each id, leaving an unknown one verbatim for a
// consistency test to flag) and ContractTokenIDs (which enumerates them), so a guard
// cannot disagree with the assembler about what a token is. text receives each
// non-token run; token receives the trimmed id plus the raw source to re-emit.
func scanContractTokens(body string, text func(string), token func(id, raw string)) {
	const open = "{{contract:"
	for {
		i := strings.Index(body, open)
		if i < 0 {
			text(body)
			return
		}
		text(body[:i])
		rest := body[i+len(open):]
		j := strings.Index(rest, "}}")
		if j < 0 {
			text(body[i:]) // unterminated - leave verbatim
			return
		}
		token(strings.TrimSpace(rest[:j]), open+rest[:j]+"}}")
		body = rest[j+2:]
	}
}

func substituteContracts(body string) string {
	var b strings.Builder
	scanContractTokens(body,
		func(s string) { b.WriteString(s) },
		func(id, raw string) {
			if rendered := contracts.Render(id); rendered != "" {
				b.WriteString(rendered)
			} else {
				b.WriteString(raw) // unknown id - leave verbatim
			}
		})
	return b.String()
}

// substituteContractsOnce renders each contract id in full the FIRST time it
// appears across a guide set, replacing later occurrences with a short pointer
// line: co-selected guides share contracts (endpoint-routes rides three of
// them), and a second verbatim copy in one seed is pure token cost. Guides
// render in deterministic order, so a given selection set always produces
// identical bytes.
func substituteContractsOnce(body string, seen map[string]bool) string {
	var b strings.Builder
	scanContractTokens(body,
		func(s string) { b.WriteString(s) },
		func(id, raw string) {
			rendered := contracts.Render(id)
			if rendered == "" {
				b.WriteString(raw) // unknown id - leave verbatim
				return
			}
			if seen[id] {
				b.WriteString("(contract " + id + " is rendered in full in an earlier guide above)")
				return
			}
			seen[id] = true
			b.WriteString(rendered)
		})
	return b.String()
}

// ContractTokenIDs returns every `{{contract:id}}` id in body, in order, recognized
// exactly as the assembler recognizes it. Exposed for the drift guards: enumerating
// with a separate pattern is how a token silently stops being covered.
func ContractTokenIDs(body string) []string {
	var ids []string
	scanContractTokens(body, func(string) {}, func(id, _ string) { ids = append(ids, id) })
	return ids
}
