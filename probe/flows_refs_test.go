package probe

import (
	"strings"
	"testing"
)

func refSuite(t *testing.T, steps string) *FlowSuite {
	t.Helper()
	suite, err := ParseFlowSource(`export default { flows: [{ name: "f", steps: [{ goto: "/" }, ` + steps + `] }] }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return suite
}

const refCorpus = `<main id="board"><button class="btn tile-go" data-nav="work">Place the order</button>
<h1>What I do &amp; how</h1></main><script>el.dataset.milk = 'oat'; const n = '’ quote';</script>`

func TestUnmatchedReferencesFlagsMissingTokens(t *testing.T) {
	suite := refSuite(t, `
		{ click: ".chip[data-cat='x']" },
		{ click: "#board .tile-go" },
		{ click: "#missing-id" },
		{ expectText: "Cappuccino" },
	`)
	got := strings.Join(suite.UnmatchedReferences(refCorpus), "\n")
	for _, want := range []string{`".chip"`, `"data-cat"`, `"#missing-id"`, `"Cappuccino"`} {
		if !strings.Contains(got, want) {
			t.Errorf("want a warning naming %s, got:\n%s", want, got)
		}
	}
	for _, quiet := range []string{`"#board"`, `".tile-go"`, `"x"`} {
		if strings.Contains(got, quiet) {
			t.Errorf("%s exists in the corpus or is a quoted value, must not warn:\n%s", quiet, got)
		}
	}
}

func TestUnmatchedReferencesAcceptsRealReferences(t *testing.T) {
	suite := refSuite(t, `
		{ click: "text=Place the order" },
		{ expectText: "what i do & how" },
		{ click: "[data-nav='work']" },
		{ click: "[data-milk='oat']" },
		{ expectText: "Order {{run}} placed" },
		{ expectText: "OK" },
		{ click: ".btn:has-text('Order {{run}}') #board" },
	`)
	if got := suite.UnmatchedReferences(refCorpus); len(got) != 0 {
		t.Fatalf("every reference exists or is exempt, got: %v", got)
	}
}

func TestUnmatchedReferencesSkipsEmptyCorpus(t *testing.T) {
	suite := refSuite(t, `{ click: "#nowhere" }`)
	if got := suite.UnmatchedReferences(""); got != nil {
		t.Fatalf("with no files to check against nothing can be said, got %v", got)
	}
}
