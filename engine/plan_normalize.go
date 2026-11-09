package engine

// Deterministic plan repairs applied BEFORE the coherence check (plan_coherence.go).
// Where a contradiction has one mechanical, unambiguous fix the engine can compute
// itself, it applies it here rather than bouncing the whole plan back through a
// fresh LLM re-plan turn. These functions MUTATE the plan; they are conservative -
// each fixes ONLY the unambiguous case and leaves anything debatable to the guard's
// LLM repair.

import (
	"fmt"
	"strings"
)

// normalizeEntryExports applies the deterministic half of entryExportsProblem's own
// advice. A spine task that owns the entry AND declares `exports` with no zero-import
// store module to home them trips the guard (exports on the unimported-root entry
// would close a leaf -> entry -> leaf cycle). The model slips this constantly - its
// bundled-app prior wants to export from the entry - and the fix is purely
// mechanical: add the conventional store module (`/files/app.js` -> `/files/app.state.js`)
// to the same task's `paths`, where the exports then home and the entry stays export-free.
//
// This runs before the coherence check, so the common slip never costs a re-plan turn.
// It is conservative: it fires only when the guard itself would (spine owns a page
// route + an entry module + declares exports + has no store home) AND the derived
// store path is UNCLAIMED by any task's paths or edits. When the store path is already
// owned or edited (a genuine ambiguity the engine must not guess through), it leaves the
// plan untouched and the guard's LLM repair still handles it. Returns one line per fix
// applied, for logging; mutates p in place.
func normalizeEntryExports(p *Plan) []string {
	if p == nil {
		return nil
	}
	var applied []string
	for i := range p.Tasks {
		t := p.Tasks[i]
		if len(t.Exports) == 0 || !taskOwnsPageRoute(t) {
			continue
		}
		entryModule, entryDisplay := spineEntryModule(p, i)
		if entryModule == "" {
			continue
		}
		if taskExportHome(p, i, entryModule) != "" {
			continue // already has a store home: valid, nothing to do
		}
		store := stateLeafPath(entryDisplay)
		normStore := normPlanPath(store)
		if normStore == entryModule || planModuleClaimed(p, normStore) {
			continue // ambiguous: leave it to the guard's LLM repair
		}
		p.Tasks[i].Paths = append(p.Tasks[i].Paths, store)
		applied = append(applied, fmt.Sprintf("task %q: added store module %q to home its exports (%s)",
			planTaskSlug(t), store, strings.Join(t.Exports, "; ")))
	}
	return applied
}

// planModuleClaimed reports whether any task OWNs (paths) or EDITS the client module
// at normalized path norm - i.e. the path is already spoken for, so a normalization
// must not add it to a different task.
func planModuleClaimed(p *Plan, norm string) bool {
	for _, t := range p.Tasks {
		for _, pth := range t.Paths {
			if normPlanPath(pth) == norm {
				return true
			}
		}
		if taskEditsModule(t, norm) {
			return true
		}
	}
	return false
}
