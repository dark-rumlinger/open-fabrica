package engine

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"openfabrica/knowledge"
)

// TestWindowBridgeExemptGlobalsMatchGuide keeps the exempt-globals list in the
// injected windowBridgeRuleBlock (used when the full client_js_split guide is NOT
// pulled) in lockstep with the guide's own list. The two never appear in one build
// - the block is the fallback for the guide's absence - so nothing at runtime keeps
// them equal; only this test does. A global exempted in one but not the other would
// let a real window bridge slip a gate on one path and be rejected on the other.
func TestWindowBridgeExemptGlobalsMatchGuide(t *testing.T) {
	guide, err := knowledge.FS.ReadFile("guides/frontend/client_js_split/GUIDE.md")
	if err != nil {
		t.Fatalf("read client_js_split guide: %v", err)
	}
	re := regexp.MustCompile(`window\.__[A-Za-z0-9_]+`)
	fromBlock := uniqSortedTokens(re.FindAllString(windowBridgeRuleBlock, -1))
	fromGuide := uniqSortedTokens(re.FindAllString(string(guide), -1))
	if len(fromBlock) == 0 {
		t.Fatal("windowBridgeRuleBlock names no exempt window.__ globals")
	}
	if strings.Join(fromBlock, ",") != strings.Join(fromGuide, ",") {
		t.Fatalf("window bridge exempt-globals drift:\n  windowBridgeRuleBlock: %v\n  client_js_split guide: %v\nthe injected fallback and the guide must list the same exempt window.__ globals", fromBlock, fromGuide)
	}
}

func uniqSortedTokens(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
