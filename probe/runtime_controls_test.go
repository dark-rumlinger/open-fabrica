package probe

import (
	"strings"
	"testing"

	"github.com/chromedp/cdproto/cdp"
)

// The census verdict (analyzeControls) is pure - fabricated listener sets and
// parent chains, no browser - so every branch is exercised here, matching this
// package's Chrome-free test convention.

func cand(ix int, backend int64, text string) controlCandidate {
	return controlCandidate{Index: ix, Tag: "button", Text: text, Sel: "button.x", Backend: backend}
}

func TestAnalyzeControls_GlobalListenerMakesNothingDead(t *testing.T) {
	// A window/document-level click listener catches every bubbled click, so no
	// control can be dead even with an empty per-node listener set.
	got := analyzeControls("/p", 2,
		[]controlCandidate{cand(0, 10, "A"), cand(1, 11, "B")},
		map[int64]bool{}, map[int64]int64{}, true)
	if len(got) != 0 {
		t.Fatalf("global listener must suppress all findings, got %+v", got)
	}
}

func TestAnalyzeControls_OwnListenerIsAlive(t *testing.T) {
	got := analyzeControls("/p", 1,
		[]controlCandidate{cand(0, 10, "Save")},
		map[int64]bool{10: true}, map[int64]int64{}, false)
	if len(got) != 0 {
		t.Fatalf("a control with its own click listener is alive, got %+v", got)
	}
}

func TestAnalyzeControls_AncestorListenerIsAlive(t *testing.T) {
	// Delegation: the listener is on an ancestor (node 5), the button is node 10,
	// 10's parent is 5. Clicking 10 bubbles to 5 - alive.
	got := analyzeControls("/p", 1,
		[]controlCandidate{cand(0, 10, "Row")},
		map[int64]bool{5: true}, map[int64]int64{10: 5}, false)
	if len(got) != 0 {
		t.Fatalf("a control under a delegating ancestor is alive, got %+v", got)
	}
}

func TestAnalyzeControls_AllDeadIsInertAndBlocking(t *testing.T) {
	got := analyzeControls("/p", 2,
		[]controlCandidate{cand(0, 10, "One"), cand(1, 11, "Two")},
		map[int64]bool{}, map[int64]int64{10: 5, 11: 6}, false)
	if len(got) != 1 {
		t.Fatalf("want one finding, got %+v", got)
	}
	if got[0].Category != "inert_page" {
		t.Errorf("every control dead should be inert_page, got %q", got[0].Category)
	}
	if got[0].Severity != "error" || !got[0].Deterministic {
		t.Errorf("inert_page must be a deterministic error, got sev=%q det=%v", got[0].Severity, got[0].Deterministic)
	}
	if got[0].Signature == "" {
		t.Errorf("inert_page needs a signature so the fix-loop can fingerprint it")
	}
}

func TestAnalyzeControls_SomeDeadIsFixPassNotBlocking(t *testing.T) {
	// total=3 (one actionable control counted but not a candidate); both candidates
	// dead. dead(2) < total(3) -> the page has a working control, so this is a
	// fix-pass finding, not an inert page.
	got := analyzeControls("/p", 3,
		[]controlCandidate{cand(0, 10, "One"), cand(1, 11, "Two")},
		map[int64]bool{}, map[int64]int64{}, false)
	if len(got) != 1 {
		t.Fatalf("want one finding, got %+v", got)
	}
	if got[0].Category != "dead_control" {
		t.Errorf("some-but-not-all dead should be dead_control, got %q", got[0].Category)
	}
}

func TestAnalyzeControls_MixedLiveAndDead(t *testing.T) {
	// Two candidates, one wired (node 11 has a listener), one dead (node 10). total
	// equals the two candidates, but only one is dead, so not inert.
	got := analyzeControls("/p", 2,
		[]controlCandidate{cand(0, 10, "Dead"), cand(1, 11, "Live")},
		map[int64]bool{11: true}, map[int64]int64{}, false)
	if len(got) != 1 || got[0].Category != "dead_control" {
		t.Fatalf("want one dead_control, got %+v", got)
	}
}

func TestAnalyzeControls_NoCandidatesNoFinding(t *testing.T) {
	if got := analyzeControls("/p", 4, nil, map[int64]bool{}, map[int64]int64{}, false); got != nil {
		t.Fatalf("no candidates means no finding, got %+v", got)
	}
}

func TestHasListenerUpChain_CycleTerminates(t *testing.T) {
	// A malformed parent map with a cycle must not loop forever, and (no listener
	// in it) must resolve to "no listener".
	if hasListenerUpChain(10, map[int64]bool{}, map[int64]int64{10: 11, 11: 10}) {
		t.Fatalf("no listener in a cyclic chain should read as none")
	}
	if !hasListenerUpChain(10, map[int64]bool{11: true}, map[int64]int64{10: 11, 11: 10}) {
		t.Fatalf("a listener present in a cyclic chain should still be found")
	}
}

func TestAttrValue(t *testing.T) {
	n := &cdp.Node{Attributes: []string{"class", "btn primary", "data-ofdeadctl", "3"}}
	if v, ok := attrValue(n, "data-ofdeadctl"); !ok || v != "3" {
		t.Errorf(`attrValue(data-ofdeadctl) = %q, %v; want "3", true`, v, ok)
	}
	if _, ok := attrValue(n, "missing"); ok {
		t.Errorf("attrValue(missing) should report not-found")
	}
	// An odd-length (malformed) attribute array must not panic or misread.
	odd := &cdp.Node{Attributes: []string{"lonely"}}
	if _, ok := attrValue(odd, "lonely"); ok {
		t.Errorf("a name with no value must not be read as present")
	}
}

// The case this check exists for: an app whose entire UI is inside a
// container that starts display:none. Every control exists, none is visible, so the
// visible census sees nothing and the page ships dead.
func TestUnreachableUIFlagsAllHiddenControls(t *testing.T) {
	is := unreachableUIIssue("/", 0, 3, []string{"button#btnChat", "button#btnJoin", "button#btnRoster"}, 0)
	if is == nil {
		t.Fatal("want an issue for a page whose every control is hidden")
	}
	got := []ValidationIssue{*is}
	if got[0].Category != "hidden_ui" || got[0].Severity != "error" || !got[0].Deterministic {
		t.Errorf("want a deterministic hidden_ui error, got %+v", got[0])
	}
	if !strings.Contains(got[0].Message, "btnJoin") {
		t.Errorf("message should name the hidden controls: %q", got[0].Message)
	}
}

// A page with even one visible control is an ordinary page with a modal or a
// collapsed menu. Never flagged, however many hidden controls it carries.
func TestUnreachableUIIgnoresPageWithAVisibleControl(t *testing.T) {
	if is := unreachableUIIssue("/", 1, 9, []string{"button#inModal"}, 0); is != nil {
		t.Errorf("a page with a visible control must not be flagged: %+v", is)
	}
}

// A page with no controls at all (a pure-canvas game driven by keyboard) has
// nothing hidden either, so there is no evidence of an unreachable UI.
func TestUnreachableUIIgnoresPageWithNoControls(t *testing.T) {
	if is := unreachableUIIssue("/", 0, 0, nil, 0); is != nil {
		t.Errorf("a page with no controls must not be flagged: %+v", is)
	}
}

// A keyboard-driven canvas game whose pause menu starts hidden: every control is
// hidden, but the canvas IS the page's surface, so it is not unreachable.
func TestUnreachableUIIgnoresCanvasGame(t *testing.T) {
	if is := unreachableUIIssue("/", 0, 2, []string{"button#resume", "button#quit"}, 1); is != nil {
		t.Errorf("a page showing a large canvas must not be flagged: %+v", is)
	}
}

// The verdict is taken from the census counts alone, upstream of the listener work,
// so delegation cannot suppress it: a global click listener proves handlers exist,
// not that anything is on screen to click.
func TestUnreachableUIIsIndependentOfListeners(t *testing.T) {
	if is := unreachableUIIssue("/", 0, 2, []string{"button#a", "button#b"}, 0); is == nil {
		t.Fatal("an unreachable UI must be reported regardless of listener wiring")
	}
}

// The signature is order-independent so the fix loop sees one stable finding rather
// than a new one each sweep as the DOM order shifts.
func TestUnreachableUISignatureIsStable(t *testing.T) {
	a := unreachableUIIssue("/", 0, 2, []string{"button#b", "button#a"}, 0)
	b := unreachableUIIssue("/", 0, 2, []string{"button#a", "button#b"}, 0)
	if a.Signature != b.Signature {
		t.Errorf("signature depends on census order: %q vs %q", a.Signature, b.Signature)
	}
}
