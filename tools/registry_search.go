package tools

import (
	"fmt"
	"sort"
	"strings"

	"openfabrica/providers/llm"
)

// CatalogueEntry is the lightweight descriptor surfaced to the LLM in the
// system-prompt tool catalogue and as the result shape of find_tool's keyword
// query mode. Schemas are deliberately omitted; the LLM calls find_tool with
// names: [...] to fetch the full JSON Schema once it has chosen a tool.
type CatalogueEntry struct {
	Name      string   `json:"name"`
	OneLiner  string   `json:"one_liner"`
	WhenToUse string   `json:"when_to_use,omitempty"`
	Category  string   `json:"category"`
	Actions   []string `json:"actions,omitempty"`
}

// actionNames returns this tool's action names in sorted order. Shared by the
// catalogue, search, and the prompt index so the model is shown the exact verbs
// it must pass as the "action" arg, rather than guessing them.
func (d *ToolDef) actionNames() []string {
	out := make([]string, 0, len(d.Actions))
	for a := range d.Actions {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// catalogueEntry renders this tool's lightweight descriptor. Single source for
// the shape Catalogue, Search, and HybridSearch all surface, so the four fields
// the model routes on can never drift between those paths.
func (d *ToolDef) catalogueEntry() CatalogueEntry {
	return CatalogueEntry{
		Name:      d.Name,
		OneLiner:  d.OneLiner,
		WhenToUse: strings.TrimSpace(d.WhenToUse),
		Category:  d.Category,
		Actions:   d.actionNames(),
	}
}

// clampLimit normalizes a caller-supplied result cap: 0 (unset) -> 10, and the
// hard ceiling is 25. Shared by Search and HybridSearch.
func clampLimit(limit int) int {
	if limit <= 0 {
		return 10
	}
	if limit > 25 {
		return 25
	}
	return limit
}

// Catalogue returns every capability-available tool as a flat list of
// {name, one_liner, when_to_use, category} entries, sorted by category then name. Used by
// the prompt assembler to render the catalogue every stage sees.
//
// stage filters by per-tool Stages declaration: "" disables stage gating; the lowercase
// current stage name ("build", "live") hides tools that don't list it.
func (r *Registry) Catalogue(caps llm.Capabilities, stage string) []CatalogueEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]*ToolDef, 0, len(r.tools))
	for _, name := range r.sortedNames() {
		t := r.tools[name]
		if !caps.Has(t.RequiresCapability) {
			continue
		}
		if !t.AvailableInStage(stage) {
			continue
		}
		defs = append(defs, t)
	}
	sort.SliceStable(defs, func(i, j int) bool {
		if defs[i].Category != defs[j].Category {
			return defs[i].Category < defs[j].Category
		}
		return defs[i].Name < defs[j].Name
	})
	out := make([]CatalogueEntry, 0, len(defs))
	for _, t := range defs {
		out = append(out, CatalogueEntry{
			Name:      t.Name,
			OneLiner:  t.OneLiner,
			WhenToUse: strings.TrimSpace(t.WhenToUse),
			Category:  t.Category,
			Actions:   t.actionNames(),
		})
	}
	return out
}

// CoreBuildToolNames are the tools nearly every BUILD task needs: editing artifacts
// (manage_files), resolving the task (manage_tasks), verifying a page (manage_testing),
// and pulling a contract (manage_guides). The build loop pre-loads their schemas on every
// task so the model invokes them directly instead of spending a turn on find_tool to
// discover them. Everything else stays lazily loaded via find_tool.
//
// manage_guides is here because the recall protocol is advertised everywhere the model
// reads - the mental model, the guide-index pointer, and the section index under every
// inlined guide - so a deferred one turns the platform's own instruction into a schema
// append that re-serialises the cached tool prefix. Its schema is three actions wide.
// manage_tasks declares Stages: ["build"], so LIVE and scheduled runs drop it from this
// tier without the call sites knowing.
var CoreBuildToolNames = []string{"manage_files", "manage_tasks", "manage_testing", "manage_guides"}

// BuildBackboneToolNames are the data-backbone tools a task reaches for right after the
// core set: manage_schema (tables), manage_api (CRUD endpoints), manage_data (seed/inspect
// rows). Not every task needs them, but a data task's first action is almost always
// create_table, so pre-loading extends the CoreBuildToolNames optimisation to this tier for
// a slightly larger cached tool block. LookupFull drops any name a project's capabilities
// don't allow, so this can never preload a blocked tool.
//
// manage_libraries is deliberately NOT in this set: it is preloaded in every build but
// called in a small fraction, so it stays discoverable via find_tool and the rare build
// that needs it pays one discovery hop. Everything beyond this tier stays lazy.
var BuildBackboneToolNames = []string{"manage_schema", "manage_api", "manage_data"}

// LiveExtraToolNames are preloaded ONLY in LIVE mode, on top of the build core/backbone
// set. manage_codebase (resolve/where_used/grep/project_tree) is the cross-artifact
// discovery surface an owner-reported fix reaches for first, so preloading it avoids the
// find_tool cache-bust that discovering it mid-session would cost. It stays deferred in
// BUILD, where the planner already declares the files a task touches. manage_diagnostics is
// the read-only signal the live frame prescribes FIRST when a report points at a runtime
// symptom, so it gets the same treatment; BUILD reads runtime state through the page and
// module probes instead. LookupFull drops any name a project's capabilities disallow, so
// this can never preload a blocked tool.
var LiveExtraToolNames = []string{"manage_codebase", "manage_diagnostics"}

// Index renders the compact, byte-stable tool catalogue embedded in the cached BUILD/LIVE
// system prompt: one line per capability/stage-available tool, grouped by category, with
// the tool name, one-liner, and exact action verbs. Unlike find_tool's browse mode (uncached
// tool output every call), the index rides the cached system block, so the model neither
// browses repeatedly nor guesses a wrong action verb. Schemas still load lazily via
// find_tool(names: [...]).
//
// stage gates by per-tool Stages (empty = no filter), matching Catalogue.
func (r *Registry) Index(caps llm.Capabilities, stage string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]*ToolDef, 0, len(r.tools))
	for _, name := range r.sortedNames() {
		t := r.tools[name]
		if !caps.Has(t.RequiresCapability) || !t.AvailableInStage(stage) {
			continue
		}
		defs = append(defs, t)
	}
	sort.SliceStable(defs, func(i, j int) bool {
		if defs[i].Category != defs[j].Category {
			return defs[i].Category < defs[j].Category
		}
		return defs[i].Name < defs[j].Name
	})
	var b strings.Builder
	cat := ""
	for _, t := range defs {
		if t.Category != cat {
			cat = t.Category
			fmt.Fprintf(&b, "\n### %s\n", cat)
		}
		// ASCII-only separators on purpose: a weak model mirrors the prompt's punctuation,
		// and non-ASCII glyphs prime it to emit Unicode look-alikes (like U+2212 minus) into
		// generated code, which is a SyntaxError. Keep this line plain.
		//
		// One line per tool: name, one-liner, action verbs. The verbose when-to-use guidance
		// is NOT inlined (it is the bulk of the catalogue and would be re-billed every turn
		// without prompt caching); it loads on demand via find_tool(names: [...]) alongside
		// the schema (see LookupDocs).
		fmt.Fprintf(&b, "- `%s`: %s | actions: %s\n",
			t.Name, strings.TrimSpace(t.OneLiner), strings.Join(t.actionNames(), ", "))
	}
	return strings.TrimSpace(b.String())
}

// DeferredIndex renders the tools a stage can load but has NOT preloaded, one line each
// with its one-liner, grouped by category. Names alone leave the model unable to tell
// which name does what, so it searches instead of loading, and each search costs a turn
// and a full context re-read. Action verbs are deliberately NOT inlined: an unloaded
// tool call is rejected until find_tool delivers the schema (and its action enum), so
// the verbs here would ride every turn to say what the schema says anyway. Preloaded
// tools are skipped because their schemas are already in the request. Deterministic:
// categories and names sorted.
func (r *Registry) DeferredIndex(preloaded []string, caps llm.Capabilities, stage string) string {
	skip := make(map[string]bool, len(preloaded))
	for _, n := range preloaded {
		skip[strings.ToLower(strings.TrimSpace(n))] = true
	}
	r.mu.RLock()
	byCat := map[string][]*ToolDef{}
	for _, name := range r.sortedNames() {
		t := r.tools[name]
		if skip[t.Name] || !caps.Has(t.RequiresCapability) || !t.AvailableInStage(stage) {
			continue
		}
		cat := t.Category
		if cat == "" {
			cat = "general"
		}
		byCat[cat] = append(byCat[cat], t)
	}
	r.mu.RUnlock()
	cats := make([]string, 0, len(byCat))
	for c := range byCat {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	var b strings.Builder
	for _, c := range cats {
		defs := byCat[c]
		sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
		fmt.Fprintf(&b, "**%s**\n", c)
		for _, t := range defs {
			fmt.Fprintf(&b, "- `%s`: %s\n", t.Name, strings.TrimSpace(t.OneLiner))
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// resolveAvailable maps the requested names to their registry records, dropping any that
// are unknown, capability-gated, or stage-gated. It is the single gating path LookupFull
// and LookupDocs share, so the wire schema and routing doc for one find_tool call can never
// disagree about which tools resolved. ToolDef fields are immutable after registration, so
// callers may read the returned pointers without holding the lock.
func (r *Registry) resolveAvailable(names []string, caps llm.Capabilities, stage string) []*ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ToolDef, 0, len(names))
	for _, n := range names {
		t, ok := r.tools[n]
		if !ok || !caps.Has(t.RequiresCapability) || !t.AvailableInStage(stage) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// LookupFull returns full llm.ToolDef wire-format definitions for each named
// tool that exists and whose required capability is available. Unknown or
// capability-gated names are silently dropped; the caller (find_tool) reports
// which inputs resolved by comparing returned Names against the input list.
// stage gates by per-tool Stages (empty = no stage filter).
func (r *Registry) LookupFull(names []string, caps llm.Capabilities, stage string) []llm.ToolDef {
	defs := r.resolveAvailable(names, caps, stage)
	out := make([]llm.ToolDef, 0, len(defs))
	for _, t := range defs {
		out = append(out, llm.ToolDef{
			Name:        t.Name,
			Description: t.OneLiner,
			Parameters:  t.Parameters(),
		})
	}
	return out
}

// ToolDoc carries the LLM-facing routing prose for one loaded tool: the when-to-use
// guidance and the rendered action doc (summaries + examples). find_tool returns these
// alongside the wire schemas so the model gets the guidance the schema's bare 130-char
// description can't hold, in the same step it loads the tool.
type ToolDoc struct {
	Name      string `json:"name"`
	WhenToUse string `json:"when_to_use,omitempty"`
	Doc       string `json:"doc,omitempty"`
}

// LookupDocs returns the routing doc for each named tool, applying the exact
// same capability + stage gating as LookupFull (via resolveAvailable) so the
// docs slice lines up one-for-one with the schemas slice.
func (r *Registry) LookupDocs(names []string, caps llm.Capabilities, stage string) []ToolDoc {
	defs := r.resolveAvailable(names, caps, stage)
	out := make([]ToolDoc, 0, len(defs))
	for _, t := range defs {
		out = append(out, ToolDoc{
			Name:      t.Name,
			WhenToUse: strings.TrimSpace(t.WhenToUse),
			Doc:       strings.TrimSpace(t.examplesDoc() + "\n" + t.SchemaNotes()),
		})
	}
	return out
}

// searchFields returns the haystacks Search ranks against, split by field strength: name
// (strongest), the action vocabulary (action names + summaries), and the descriptive prose
// (one-liner, when-to-use, category). Folding in the action layer lets a query like
// "aggregate" or "rss" find a tool whose capability lives in an action, not the one-liner.
// All returned strings are lower-cased.
func (d *ToolDef) searchFields() (name, actions, prose string) {
	// Sorted: the same text feeds the catalogue embedding cache key, so a map-order
	// haystack would give every call a fresh key and re-embed the whole catalogue.
	var ab strings.Builder
	ans := make([]string, 0, len(d.ActionMeta))
	for an := range d.ActionMeta {
		ans = append(ans, an)
	}
	sort.Strings(ans)
	for _, an := range ans {
		ab.WriteString(an)
		ab.WriteByte(' ')
		ab.WriteString(d.ActionMeta[an].Summary)
		ab.WriteByte(' ')
	}
	name = strings.ToLower(d.Name)
	actions = strings.ToLower(ab.String())
	prose = strings.ToLower(d.OneLiner + " " + d.Category + " " + d.WhenToUse)
	return
}

// keywordHit is one tool scored against a keyword query: its name (for fusion
// keying), the catalogue entry to surface, and the substring score.
type keywordHit struct {
	name  string
	entry CatalogueEntry
	score int
}

// keywordRank scores every capability/stage-available tool against the query's
// normalised terms (case-insensitive substring; stopwords dropped and plurals folded
// as in findQueryTerms; a tool must match at least two thirds of them), weighting
// where each term hits (name > action vocabulary > prose), and returns
// the matches in descending score with an alphabetical tiebreak. Shared by
// Search and HybridSearch so the two paths rank keywords identically.
func (r *Registry) keywordRank(query string, caps llm.Capabilities, stage string) []keywordHit {
	return r.keywordRankFiltered(query, stage, func(t *ToolDef) bool {
		return caps.Has(t.RequiresCapability) && t.AvailableInStage(stage)
	})
}

// keywordRankFiltered is keywordRank over an arbitrary tool predicate, so the
// scoring lives in one place and a caller that wants the hidden half of the
// registry ranks it identically.
func (r *Registry) keywordRankFiltered(query, stage string, include func(*ToolDef) bool) []keywordHit {
	terms := findQueryTermList(query)
	if len(terms) == 0 {
		terms = strings.Fields(strings.ToLower(query))
	}
	// A tool must carry most of the query, not all of it: one stray word ("customers"
	// in "send emails to customers") must not hide the tool every other word names.
	need := (2*len(terms) + 2) / 3

	r.mu.RLock()
	var hits []keywordHit
	for _, name := range r.sortedNames() {
		t := r.tools[name]
		if !include(t) {
			continue
		}
		nameHay, actionHay, proseHay := t.searchFields()
		matched, score := 0, 0
		for _, term := range terms {
			switch {
			case strings.Contains(nameHay, term):
				score += 3
			case strings.Contains(actionHay, term):
				score += 2
			case strings.Contains(proseHay, term):
				score += 1
			default:
				continue
			}
			matched++
		}
		if matched < need || matched == 0 {
			continue
		}
		// Terms matched dominate where they matched, so a tool covering every word
		// outranks one that hits the name on a single word.
		hits = append(hits, keywordHit{name: t.Name, entry: t.catalogueEntry(), score: matched*10 + score})
	}
	r.mu.RUnlock()

	// Rank by score descending; ties broken by name ascending (sortedNames
	// already feeds them in name order, so a stable sort on score alone keeps
	// the alphabetical tiebreak).
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	return hits
}

// HybridSearch returns catalogue entries for query, blending keyword scoring with a
// caller-supplied semantic ranking (semanticOrder: tool names in descending embedding
// similarity) via Reciprocal Rank Fusion. RRF rewards a tool either path ranks highly, so a
// semantic-only hit the strict-AND keyword pass drops still surfaces, and vice versa. Pass a
// nil/empty semanticOrder for pure keyword ranking.
//
// Keyword scoring matches name, one-liner, when-to-use, category, and action
// names/summaries, ranked by where the terms hit (name > action vocabulary > prose). limit
// clamps the count (0 -> default 10, capped at 25); stage gates by per-tool Stages. Used by
// find_tool.
func (r *Registry) HybridSearch(query string, semanticOrder []string, caps llm.Capabilities, limit int, stage string) []CatalogueEntry {
	limit = clampLimit(limit)
	hits := r.keywordRank(query, caps, stage)
	if len(semanticOrder) == 0 {
		if len(hits) > limit {
			hits = hits[:limit]
		}
		out := make([]CatalogueEntry, 0, len(hits))
		for _, h := range hits {
			out = append(out, h.entry)
		}
		return out
	}

	// Reciprocal Rank Fusion: fused(t) = sum over each list of 1/(k + rank(t)). k=60 is the
	// canonical RRF constant; it damps deep ranks so the head of each list dominates without
	// any single list monopolizing the result.
	const rrfK = 60.0
	score := make(map[string]float64)
	entry := make(map[string]CatalogueEntry)
	for rank, h := range hits {
		score[h.name] += 1.0 / (rrfK + float64(rank))
		entry[h.name] = h.entry
	}
	// semanticOrder is pre-gated by the caller, but re-resolve each name so a
	// semantic-only hit gets a catalogue entry and any tool that has since gone
	// stale or capability/stage-gated is dropped rather than surfaced empty.
	r.mu.RLock()
	for rank, name := range semanticOrder {
		t, ok := r.tools[name]
		if !ok || !caps.Has(t.RequiresCapability) || !t.AvailableInStage(stage) {
			continue
		}
		score[name] += 1.0 / (rrfK + float64(rank))
		if _, seen := entry[name]; !seen {
			entry[name] = t.catalogueEntry()
		}
	}
	r.mu.RUnlock()

	names := make([]string, 0, len(score))
	for n := range score {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if score[names[i]] != score[names[j]] {
			return score[names[i]] > score[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > limit {
		names = names[:limit]
	}
	out := make([]CatalogueEntry, 0, len(names))
	for _, n := range names {
		out = append(out, entry[n])
	}
	return out
}

// ToolProfile pairs a tool name with the text used to compute its catalogue
// embedding for semantic find_tool search.
type ToolProfile struct {
	Name string
	Text string
}

// EmbeddingProfiles returns the embedding input text for every capability/stage-available
// tool: name, one-liner, when-to-use, category, and action vocabulary concatenated (the
// same haystacks keyword search uses, so the two stay aligned). The engine embeds these once
// and caches them so a find_tool(query) can rank by semantic similarity, not only substring
// overlap. Gating matches Search/Catalogue.
func (r *Registry) EmbeddingProfiles(caps llm.Capabilities, stage string) []ToolProfile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ToolProfile, 0, len(r.tools))
	for _, name := range r.sortedNames() {
		t := r.tools[name]
		if !caps.Has(t.RequiresCapability) || !t.AvailableInStage(stage) {
			continue
		}
		nameHay, actionHay, proseHay := t.searchFields()
		text := strings.TrimSpace(nameHay + ". " + proseHay + " " + actionHay)
		out = append(out, ToolProfile{Name: t.Name, Text: text})
	}
	return out
}
