package engine

// Detector single-home guard: save-time gate codes and post-build validator
// categories.
//
// A deterministic detector's MESSAGE is the single home of the rule it enforces
// (knowledge/stages/AUTHORING.md): a frame or guide gets "at most one pointer
// line", never a paraphrased restatement. TestNoCrossPackDuplication is
// verbatim-only, so a *reworded* restatement slips past it. The detector's CODE
// token is the anchor the prose lacks: it stays byte-identical even when the
// sentence is paraphrased. So each code must appear in AT MOST ONE pack, its
// declared pointer home; a code absent from detectorPointerHome must appear in
// ZERO packs. Both lists are grepped at test time so a newly added gate or
// validator is guarded without anyone remembering to register it.
//
// Limit: this anchors on the code TOKEN, so a rule restated in pure prose
// carries no token and passes here; that class belongs in ruleRegistry
// (rule_singlehome_test.go), which anchors on identifiers instead.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"openfabrica/contracts"
	"openfabrica/pack"
)

// gateCodeRe matches the reject-code literals the content gates return, in
// either shape: a `Code: "snake_case"` struct field or a `FailCode("snake_case", ...)` call.
var gateCodeRe = regexp.MustCompile(`(?:Code:\s*|FailCode\()"([a-z0-9_]+)"`)

// validatorCategoryRe matches the issue categories the post-build validators emit:
// a `Category: "snake_case"` field on a store.Issue literal.
var validatorCategoryRe = regexp.MustCompile(`Category:\s*"([a-z0-9_]+)"`)

// detectorPointerHome names the ONE pack allowed to carry a pointer that mentions a
// detector's code. A code NOT listed here must appear in zero packs (pure
// detector-owned rule, no pointer). Keep this small: a growing list means detector
// rules are creeping back into the frames instead of being taught by the message.
// The capability guide that owns the client-JS split is the home for every
// split/import/window gate.
var detectorPointerHome = map[string]string{
	"window_global_bridge":    "client_js_split",
	"window_dead_global":      "client_js_split",
	"cyclic_import":           "client_js_split",
	"undefined_reference":     "client_js_split",
	"unresolved_import":       "client_js_split",
	"entry_exports":           "client_js_split",
	"module_too_large_client": "client_js_split",
	"oversized_body":          "client_js_split",
	// The row-ownership choice is a plan-time judgement the validator only backstops,
	// so the API guide names the gate it will hit.
	"rls_scope": "api_response_contract",
}

// codeLiterals greps distinct snake_case code tokens out of one package's non-test
// sources, so the guard tracks new detectors with no hand-maintained list. Tokens
// with no underscore are skipped: a one-word category like "runtime" is an ordinary
// English word that occurs all over the knowledge tree and would only ever produce
// false positives.
func codeLiterals(t *testing.T, re *regexp.Regexp, min int, dir ...string) []string {
	t.Helper()
	path := filepath.Join(append([]string{".."}, dir...)...)
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	set := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(path, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if strings.Contains(m[1], "_") {
				set[m[1]] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	if len(out) < min {
		t.Fatalf("only %d codes found in %s - did that package move?", len(out), path)
	}
	return out
}

// detectorCodes is the union of both deterministic layers: save-time reject codes
// and post-build validator categories.
func detectorCodes(t *testing.T) []string {
	set := map[string]bool{}
	for _, c := range codeLiterals(t, gateCodeRe, 10, "tools", "content") {
		set[c] = true
	}
	for _, c := range codeLiterals(t, validatorCategoryRe, 10, "engine", "validators") {
		set[c] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func TestDetectorCodeSingleHome(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	bodies := make(map[string]string, len(ps))
	for _, p := range ps {
		bodies[p.Slug] = contracts.Normalize(p.Body)
	}
	if len(bodies) < 5 {
		t.Fatalf("only %d packs loaded - did the knowledge tree move?", len(bodies))
	}

	for _, code := range detectorCodes(t) {
		needle := contracts.Normalize(code)
		var found []string
		for slug, body := range bodies {
			if strings.Contains(body, needle) {
				found = append(found, slug)
			}
		}
		sort.Strings(found)
		switch {
		case len(found) > 1:
			t.Errorf("detector code %q is named in %v - a rule with a deterministic detector has its "+
				"single home in that detector's message; at most ONE pack may carry a pointer, the rest "+
				"defer to it (knowledge/stages/AUTHORING.md). Collapse the extra restatements to a "+
				"pointer at %q.", code, found, detectorPointerHome[code])
		case len(found) == 1:
			home, ok := detectorPointerHome[code]
			switch {
			case !ok:
				t.Errorf("detector code %q is named in pack %q but has no declared pointer home. Either "+
					"reword the mention to drop the token (its home is the detector's message) or add "+
					"{%q: %q} to detectorPointerHome as a deliberate single-home pointer.",
					code, found[0], code, found[0])
			case found[0] != home:
				t.Errorf("detector code %q is named in %q but its declared pointer home is %q - "+
					"update detectorPointerHome if the pointer moved on purpose.", code, found[0], home)
			}
		}
	}
}
