package engine

// Guide front-matter <-> registry consistency guard.
//
// Guide auto-prefetch is driven entirely by GUIDE.md front-matter (`tools:`,
// `libraries:`, `requires:`); a typo'd entry silently never fires, and the
// build proceeds without the contract it needed. These tests assert: every
// `tools:` entry names a registered tool; every `requires:` entry names a real
// capability guide; every `tools:`/`libraries:` entry routes back to the guide
// that declared it (the maps are many-to-many, so sharing an entry is legal,
// but losing one silently kills that guide's prefetch route); and
//   - every structural signal the engine raises is claimed by a guide, and no
//     guide claims one the engine never raises - guide_reference_test only scans
//     for `slug=` string patterns, which misses this routing layer entirely.

import (
	"strings"
	"testing"

	"openfabrica/pack"
)

// TestGuideFrontMatterToolsAreRegistered: every tool a guide's `tools:`
// front-matter names must exist in the tool registry, or the tool->guide
// prefetch route for that guide is dead and the build never sees its contract.
func TestGuideFrontMatterToolsAreRegistered(t *testing.T) {
	known, _ := registryTools(t)
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	for _, g := range pack.Capabilities(ps) {
		for _, name := range g.Tools {
			raw := strings.TrimSpace(name)
			name = strings.ToLower(raw)
			if name == "" {
				t.Errorf("guide %q has an empty entry in its tools: front-matter", g.Slug)
				continue
			}
			// Registry keys are the tool's own name verbatim. guideToolMap looks the
			// entry up as written, so an entry that only matches once lowercased is
			// already a dead route regardless of whether the name resolves here.
			if raw != name {
				t.Errorf("guide %q front-matter names tool %q with uppercase characters - registry names are lowercase, so its tool->guide prefetch route is dead; write it as %q", g.Slug, raw, name)
			}
			if !known[name] {
				t.Errorf("guide %q front-matter names unregistered tool %q - its tool->guide prefetch route is dead", g.Slug, name)
			}
		}
	}
}

// TestGuideFrontMatterRequiresResolve: every `requires:` base must be a real
// capability guide, or withRequiredBases silently drops it and the dependent
// guide is injected without the base contract it builds on.
func TestGuideFrontMatterRequiresResolve(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	valid := validCapabilitySlugs(ps)
	for _, g := range pack.Capabilities(ps) {
		for _, req := range g.Requires {
			raw := strings.TrimSpace(req)
			req = strings.ToLower(raw)
			if req == "" {
				t.Errorf("guide %q has an empty entry in its requires: front-matter", g.Slug)
				continue
			}
			// Capability slugs are lowercase and withRequiredBases resolves the entry
			// as written, so an uppercase entry never matches its base.
			if raw != req {
				t.Errorf("guide %q requires base %q with uppercase characters - capability slugs are lowercase, so withRequiredBases drops it; write it as %q", g.Slug, raw, req)
			}
			if !valid[req] {
				t.Errorf("guide %q requires unknown guide %q - withRequiredBases drops it silently", g.Slug, req)
			}
		}
	}
}

// TestGuideFrontMatterMapsRoundTrip: guideToolMap and guideLibraryMap are DERIVED
// from front-matter, so the invariant worth pinning is that the derivation is
// lossless - every entry a guide declares maps back to that guide. The maps are
// many-to-many, so a shared tool is legal (that is what lets one large capability
// split into per-model guides), but an entry that silently fails to route is not.
func TestGuideFrontMatterMapsRoundTrip(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	claims := func(slugs []string, want string) bool {
		for _, s := range slugs {
			if s == want {
				return true
			}
		}
		return false
	}
	toolMap := guideToolMap(ps)
	libMap := guideLibraryMap(ps)
	for _, g := range pack.Capabilities(ps) {
		for _, name := range g.Tools {
			if name = strings.ToLower(strings.TrimSpace(name)); name == "" {
				continue
			}
			if !claims(toolMap[name], g.Slug) {
				t.Errorf("guide %q declares tool %q but guideToolMap does not route it back - the tool->guide prefetch for it is dead", g.Slug, name)
			}
		}
		for _, lib := range g.Libraries {
			if lib = strings.ToLower(strings.TrimSpace(lib)); lib == "" {
				continue
			}
			if !claims(libMap[lib], g.Slug) {
				t.Errorf("guide %q declares library %q but guideLibraryMap does not route it back", g.Slug, lib)
			}
		}
	}
}

// TestStructuralSignalsAreClaimed: every structural signal the engine can raise
// must be claimed by at least one guide's `structural:` front-matter. The engine
// detects the plan shape but names no guide, so an unclaimed signal fires into
// nothing and split builds lose the import-tree / asset-scope contracts with no
// signal anywhere.
func TestStructuralSignalsAreClaimed(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	bySignal := guideStructuralMap(ps)
	for _, sig := range []string{signalClientSplit, signalHasUI} {
		if len(bySignal[sig]) == 0 {
			t.Errorf("structural signal %q is claimed by no guide - raising it injects nothing", sig)
		}
	}
	// The reverse: a guide declaring a signal the engine never raises is dead
	// routing metadata that reads as working.
	known := map[string]bool{signalClientSplit: true, signalHasUI: true}
	for sig, slugs := range bySignal {
		if !known[sig] {
			t.Errorf("guides %v declare structural signal %q, which the engine never raises", slugs, sig)
		}
	}
}
