package engine

// Broad, self-maintaining duplication guard - the automatic backstop to the
// curated TestRuleSingleHome canaries: scans the WHOLE embedded knowledge corpus
// and fails on ANY substantial prose line that appears verbatim in two packs, or
// any canonical contract line pasted into a pack that does not transclude it. A
// new copy-pasted rule fails here without anyone remembering to add a canary.

import (
	"sort"
	"strings"
	"testing"

	"openfabrica/contracts"
	"openfabrica/pack"
)

// minDupLen is the normalized length above which an identical line shared by two
// packs is treated as duplicated PROSE rather than a coincidental short collision
// (a bare list marker, a `js` fence label, "Rules:", …).
const minDupLen = 55

// dupAllowlist holds normalized prose lines that are legitimately shared across
// packs - structural scaffolding, not a drifting rule. Keep this SMALL and justify
// every entry; the point of the test is that this list stays near-empty.
var dupAllowlist = []string{}

// codeFence reports whether a raw line opens/closes a fenced code block.
func codeFence(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "```") }

// candidateProseLines returns a pack body's dedup-worthy PROSE lines, normalized.
// Fenced code (snippets legitimately repeat across guides), headings, table rows,
// and blank/short lines are skipped, as are lines still carrying a `{{contract:}}`
// or `{{NAME}}` sentinel (those are single-sourced by construction). De-duplicated
// within the pack so a line repeated inside ONE pack is not counted as cross-pack.
func candidateProseLines(body string) map[string]bool {
	out := map[string]bool{}
	inCode := false
	for _, raw := range strings.Split(body, "\n") {
		if codeFence(raw) {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "|") || strings.HasPrefix(t, "```") {
			continue
		}
		if strings.Contains(t, "{{") {
			continue // still-sentinel line: single-sourced at assembly
		}
		n := contracts.Normalize(t)
		if len(n) < minDupLen || allowed(n) {
			continue
		}
		out[n] = true
	}
	return out
}

func allowed(norm string) bool {
	for _, a := range dupAllowlist {
		if contracts.Normalize(a) == norm {
			return true
		}
	}
	return false
}

// TestNoCrossPackDuplication: no substantial prose line appears verbatim in two
// packs. A rule pasted into a second frame (the re-accretion the coherence pass
// removed) fails here automatically - reference the one home instead of restating.
func TestNoCrossPackDuplication(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	homes := map[string][]string{} // normalized line -> pack slugs carrying it
	for _, p := range ps {
		for line := range candidateProseLines(p.Body) {
			homes[line] = append(homes[line], p.Slug)
		}
	}
	var dupes []string
	for line, slugs := range homes {
		if len(slugs) > 1 {
			sort.Strings(slugs)
			dupes = append(dupes, strings.Join(slugs, ", ")+"  ::  "+clip(line, 120))
		}
	}
	sort.Strings(dupes)
	for _, d := range dupes {
		t.Errorf("cross-pack duplication [%s]", d)
	}
	if len(dupes) > 0 {
		t.Logf("%d duplicated prose line(s); give each rule one home and reference it (see knowledge/AUTHORING.md), or add a justified entry to dupAllowlist.", len(dupes))
	}
}

// TestNoContractRestatement: a canonical contract PROSE line must not appear in any
// pack's own prose - neither pasted into a pack that skips the transclusion, nor
// restated in a pack that ALSO transcludes it. The raw pack body carries the
// `{{contract:id}}` sentinel (candidateProseLines skips any `{{`-bearing line), so
// the contract's real text is present in a pack ONLY if it was copied in - which is
// exactly the drift to catch. Checking transcluding packs too closes the blind spot
// that let a guide both transclude a contract and restate it in prose.
func TestNoContractRestatement(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	cs, err := contracts.List()
	if err != nil {
		t.Fatalf("contracts.List: %v", err)
	}
	for _, c := range cs {
		lines := candidateProseLines(c.Body)
		for _, p := range ps {
			for line := range candidateProseLines(p.Body) {
				if lines[line] {
					t.Errorf("pack %q restates a line of contract %q instead of transcluding {{contract:%s}}: %q",
						p.Slug, c.ID, c.ID, clip(line, 120))
				}
			}
		}
	}
}
