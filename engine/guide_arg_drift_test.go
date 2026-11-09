package engine

// Guide <-> tool ARGUMENT drift guard.
//
// TestDocsReferenceRealTools / ...RealActions check that a doc names a real tool
// and a real `action=` verb; nothing checked the ARGS. populateActionStruct
// iterates the DECLARED fields, so an arg no field claims is dropped in SILENCE
// - the call succeeds, the setting never exists, and no gate fires. A capability
// family is several near-identical tools, so a sibling's setting reads as though
// it belongs; the guard flags an assignment-form arg the guide's DECLARED tools
// do not accept but a SIBLING (same Category) does.
//
// Three narrowings keep false positives at zero: only the assignment form inside
// a code span counts (a bare mention is a cross-reference, not a call);
// same-category only (a deliberate cross-family reference drops out for free);
// only guides that DECLARE tools. Known limit: a cross-CATEGORY arg mix-up
// passes - widen only with evidence that mistake actually happens, since
// widening buys back false positives at the cost of a growing allowlist.

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"openfabrica/pack"
	"openfabrica/tools"
)

// guideArgAssignment matches an assignment-form token opening a code span: a
// snake_case name immediately followed by `=` or `:`. Anchored at the span start so
// `net.send({ type:"move" })` and ordinary prose never match.
var guideArgAssignment = regexp.MustCompile("`([a-z][a-z0-9_]{2,})\\s*[=:]")

// guideArgSiblingRefs are (guide, arg) pairs where the guide deliberately sets a
// SAME-CATEGORY sibling's arg because that other tool is genuinely part of the
// capability being described. Every entry must name a tool the guide tells the model
// to call; an entry that is really an instruction for the guide's OWN tool is the bug
// this test exists to find, and belongs fixed rather than listed.
var guideArgSiblingRefs = map[string]bool{
	// media_generation's whole point is that all three generators land their output
	// through a manage_files save, so the save's own args are intrinsic to it.
	"media_generation|kind":  true,
	"media_generation|scope": true,
}

func TestGuideArgsBelongToDeclaredTools(t *testing.T) {
	// registryTools fails loudly on an unpopulated registry, so this can never pass
	// vacuously through an empty corpus.
	if known, _ := registryTools(t); len(known) == 0 {
		t.Fatal("empty registry")
	}
	reg := tools.DefaultRegistry()

	// argsOf(tool) and the category index, both read off the same registry listing.
	argsOf := map[string]map[string]bool{}
	byCategory := map[string][]string{}
	for _, td := range reg.List() {
		as := map[string]bool{}
		for _, meta := range td.ActionMeta {
			for _, p := range meta.Params {
				as[p.Name] = true
			}
		}
		argsOf[td.Name] = as
		byCategory[td.Category] = append(byCategory[td.Category], td.Name)
	}

	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}

	checked := 0
	for _, g := range pack.Capabilities(ps) {
		if len(g.Tools) == 0 {
			continue
		}
		checked++

		declared := map[string]bool{}
		accepted := map[string]bool{} // args any DECLARED tool takes
		siblings := map[string]bool{} // same-category tools the guide did not declare
		for _, name := range g.Tools {
			name = strings.ToLower(strings.TrimSpace(name))
			td, err := reg.Get(name)
			if err != nil {
				continue // TestGuideFrontMatterToolsAreRegistered owns that failure
			}
			declared[name] = true
			for a := range argsOf[name] {
				accepted[a] = true
			}
			for _, sib := range byCategory[td.Category] {
				siblings[sib] = true
			}
		}
		for name := range declared {
			delete(siblings, name)
		}

		seen := map[string]bool{}
		for _, m := range guideArgAssignment.FindAllStringSubmatch(g.Body, -1) {
			arg := m[1]
			if accepted[arg] || seen[arg] || guideArgSiblingRefs[g.Slug+"|"+arg] {
				continue
			}
			owners := map[string]bool{}
			for sib := range siblings {
				if argsOf[sib][arg] {
					owners[sib] = true
				}
			}
			if len(owners) == 0 {
				continue
			}
			seen[arg] = true
			t.Errorf("guide %q sets `%s=`, but no tool it declares (%s) accepts that arg - it belongs to the sibling %s. "+
				"An arg no action field claims is DROPPED in silence by populateActionStruct, so the build gets a green tool "+
				"result and the setting never exists. Fix the guide, add the arg to the tool, or - if the guide really does "+
				"tell the model to call that sibling - add it to guideArgSiblingRefs with the reason.",
				g.Slug, arg, sortedKeysOf(declared), sortedKeysOf(owners))
		}
	}
	if checked < 8 {
		t.Fatalf("only %d guides declare tools - the front-matter scan looks broken", checked)
	}
}

func sortedKeysOf(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}
