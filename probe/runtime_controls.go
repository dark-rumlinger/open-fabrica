package probe

// Dead-control census: a common failure is a visible button that does
// nothing. The page-load probe never clicks (a click fires destructive actions),
// so this catches handler-less controls WITHOUT clicking, via
// DOMDebugger.getEventListeners. A control is alive when it or any ancestor
// carries a click-ish listener, an inline onclick, or a native action; framework
// delegation is caught as an ancestor/global listener, so it never
// false-positives. FP bias, like the rest of the probe: a missed dead control is
// acceptable, flagging a working one is not - every uncertainty resolves toward
// "alive". A listener attached only after a post-load async step is a known
// blind spot. The pure verdict (analyzeControls) is split from the Chrome
// plumbing so it is unit-tested browser-free.

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/domdebugger"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// clickListenerTypes are the event types that make a control actionable by pointer.
// Anything outside this set (a keydown-only handler) is deliberately not treated as
// wiring, since a mouse/touch user could not trigger it - and the set is kept generous
// so this only ever risks a false negative.
var clickListenerTypes = map[string]bool{
	"click":       true,
	"mousedown":   true,
	"mouseup":     true,
	"pointerdown": true,
	"pointerup":   true,
	"touchstart":  true,
	"touchend":    true,
}

// controlCandidate is one visible control that page JS could not prove alive on
// its own (no inline onclick, no native action) - so it needs a real click
// listener somewhere on itself or an ancestor, which only CDP can see. Backend is
// its DOM backendNodeId; the rest is for the owner-facing report.
type controlCandidate struct {
	Index   int    `json:"i"`
	Tag     string `json:"tag"`
	Text    string `json:"text"`
	Sel     string `json:"sel"`
	Backend int64  `json:"-"` // filled from the DOM tree, keyed by the data-ofdeadctl index
}

// controlScan is the JS census payload: how many visible controls the page has in
// total, which of them still need a listener to be alive, and how many controls
// exist but are not visible. Hidden is counted because the visible census is
// vacuously satisfied by a page that has no visible controls at all, which is
// exactly what an app whose whole UI sits in a display:none subtree looks like.
type controlScan struct {
	Total      int                `json:"total"`
	Candidates []controlCandidate `json:"candidates"`
	Hidden     int                `json:"hidden"`
	HiddenSel  []string           `json:"hiddenSel"`
	// Canvas counts visible canvases big enough to be the page's main surface (a game
	// or drawing area), which a keyboard or pointer drives without any control.
	Canvas int `json:"canvas"`
}

// controlCensusJS runs in the page: it finds every visible interactive control,
// counts them, and returns the subset not already actionable from markup alone -
// those still need a JS click listener, which the Go side proves via
// getEventListeners. Each candidate is tagged data-ofdeadctl=<index> so the DOM tree
// joins back to it by backendNodeId. Defensive: any element that throws is skipped,
// never the whole scan.
//
//go:embed js/control_census.js
var controlCensusJS string

// probeDeadControls runs the census on an already-loaded page and returns any
// dead-control findings. Called only after a clean load (a page that already threw is
// not judged - its listeners may never have attached), reusing that browser context.
// Any CDP error yields no findings, never a false positive from a browser hiccup.
func probeDeadControls(ctx context.Context, pagePath string) []ValidationIssue {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(controlCensusJS, &raw)); err != nil {
		return nil
	}
	var scan controlScan
	if err := json.Unmarshal([]byte(raw), &scan); err != nil {
		return nil
	}
	// Decided before the early return below and before any CDP work: it needs only
	// the census counts, and the "no visible controls" case is exactly the one that
	// early return treats as nothing to judge.
	if is := unreachableUIIssue(pagePath, scan.Total, scan.Hidden, scan.HiddenSel, scan.Canvas); is != nil {
		return []ValidationIssue{*is}
	}
	if scan.Total == 0 || len(scan.Candidates) == 0 {
		return nil // no controls, or every control is actionable from markup
	}

	var (
		clickNodes  map[int64]bool
		parentOf    map[int64]int64
		backendByIx map[int]int64
		globalClick bool
	)
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		// The click-listener set across the whole document subtree.
		docRO, _, err := runtime.Evaluate("document").Do(ctx)
		if err != nil || docRO == nil || docRO.ObjectID == "" {
			return err
		}
		docListeners, err := domdebugger.GetEventListeners(docRO.ObjectID).WithDepth(-1).WithPierce(false).Do(ctx)
		if err != nil {
			return err
		}
		clickNodes = map[int64]bool{}
		for _, l := range docListeners {
			if clickListenerTypes[l.Type] {
				clickNodes[int64(l.BackendNodeID)] = true
			}
		}
		// A click listener on window or on the document node itself is global
		// delegation - it catches every bubbled click, so nothing on the page is
		// dead. Detect both and short-circuit.
		if winRO, _, werr := runtime.Evaluate("window").Do(ctx); werr == nil && winRO != nil && winRO.ObjectID != "" {
			if winListeners, lerr := domdebugger.GetEventListeners(winRO.ObjectID).Do(ctx); lerr == nil {
				for _, l := range winListeners {
					if clickListenerTypes[l.Type] {
						globalClick = true
					}
				}
			}
		}
		// The DOM tree: parent chains + the backendNodeId of each tagged candidate.
		root, err := dom.GetDocument().WithDepth(-1).WithPierce(false).Do(ctx)
		if err != nil || root == nil {
			return err
		}
		if clickNodes[int64(root.BackendNodeID)] {
			globalClick = true
		}
		parentOf = map[int64]int64{}
		backendByIx = map[int]int64{}
		walkControlTree(root, 0, parentOf, backendByIx)
		return nil
	}))
	if err != nil || clickNodes == nil {
		return nil
	}

	// Attach each candidate's backendNodeId from the DOM join.
	for i := range scan.Candidates {
		scan.Candidates[i].Backend = backendByIx[scan.Candidates[i].Index]
	}
	return analyzeControls(pagePath, scan.Total, scan.Candidates, clickNodes, parentOf, globalClick)
}

// walkControlTree records, for every node, its parent's backendNodeId, and maps
// each data-ofdeadctl index to the node's backendNodeId. Iterative DFS over the
// cdp tree (Parent pointers are not guaranteed populated, so the chain is built
// here from Children).
func walkControlTree(n *cdp.Node, parentBackend int64, parentOf map[int64]int64, backendByIx map[int]int64) {
	if n == nil {
		return
	}
	be := int64(n.BackendNodeID)
	if parentBackend != 0 {
		parentOf[be] = parentBackend
	}
	if ix, ok := attrValue(n, "data-ofdeadctl"); ok {
		var idx int
		if _, err := fmt.Sscanf(ix, "%d", &idx); err == nil {
			backendByIx[idx] = be
		}
	}
	for _, c := range n.Children {
		walkControlTree(c, be, parentOf, backendByIx)
	}
}

// attrValue reads an attribute off a cdp node whose Attributes are a flat
// [name1, value1, name2, value2, ...] array.
func attrValue(n *cdp.Node, name string) (string, bool) {
	for i := 0; i+1 < len(n.Attributes); i += 2 {
		if n.Attributes[i] == name {
			return n.Attributes[i+1], true
		}
	}
	return "", false
}

// analyzeControls is the pure verdict (no Chrome, no CDP - unit-tested directly):
// given the candidates, the click-listener node set, the parent chains, and whether a
// global delegated listener exists, decide which controls are dead. A candidate is
// dead when neither it nor any ancestor carries a click listener and there is no
// global handler. If EVERY visible control is dead the page is inert (blocks the
// deploy); some-dead-but-one-live ships with the dead ones flagged for a fix-pass.
func analyzeControls(pagePath string, total int, candidates []controlCandidate, clickNodes map[int64]bool, parentOf map[int64]int64, globalClick bool) []ValidationIssue {
	if globalClick {
		return nil
	}
	var dead []controlCandidate
	for _, c := range candidates {
		if !hasListenerUpChain(c.Backend, clickNodes, parentOf) {
			dead = append(dead, c)
		}
	}
	if len(dead) == 0 {
		return nil
	}
	sort.SliceStable(dead, func(i, j int) bool { return dead[i].Index < dead[j].Index })

	names := make([]string, 0, len(dead))
	sigParts := make([]string, 0, len(dead))
	for _, c := range dead {
		desc := c.Tag
		if c.Text != "" {
			desc += ` "` + c.Text + `"`
		} else if c.Sel != "" {
			desc += " (" + c.Sel + ")"
		}
		names = append(names, desc)
		sigParts = append(sigParts, c.Sel)
	}
	sort.Strings(sigParts)
	signature := strings.Join(sigParts, "|")

	inert := total > 0 && len(dead) == total
	if inert {
		return []ValidationIssue{{
			Severity:      "error",
			Category:      "inert_page",
			Page:          pagePath,
			Deterministic: true,
			Signature:     signature,
			Message: fmt.Sprintf(
				"page %q is inert: every interactive control on it is dead - it has no click handler on itself or any ancestor, so nothing the user can click does anything. Dead controls: %s. Wire each with addEventListener (or an onclick), or use event delegation on a container.",
				pagePath, strings.Join(names, "; ")),
		}}
	}
	return []ValidationIssue{{
		Severity:      "error",
		Category:      "dead_control",
		Page:          pagePath,
		Deterministic: true,
		Signature:     signature,
		Message: fmt.Sprintf(
			"page %q has visible control(s) with no click handler on themselves or any ancestor, so clicking them does nothing: %s. Wire each with addEventListener (or an onclick), or delegate on a container. (Other controls on the page do work.)",
			pagePath, strings.Join(names, "; ")),
	}}
}

// hasListenerUpChain reports whether backend, or any ancestor reachable through
// parentOf, is in the click-listener set. Bounded by a visited guard so a
// malformed cycle can never loop.
func hasListenerUpChain(backend int64, clickNodes map[int64]bool, parentOf map[int64]int64) bool {
	seen := map[int64]bool{}
	for n := backend; n != 0 && !seen[n]; n = parentOf[n] {
		seen[n] = true
		if clickNodes[n] {
			return true
		}
	}
	return false
}

// unreachableUIIssue fires when a page has controls but not one of them is visible:
// the whole interactive surface sits in a display:none / visibility:hidden subtree,
// so on open there is nothing to click and no way to reach the rest of the app.
//
// This is the hole the dead-control census cannot see. That census asks whether the
// VISIBLE controls are wired, and a page with zero visible controls satisfies it
// vacuously - which is exactly what an app whose UI mounts inside a hidden container
// looks like. It loads clean, throws nothing, and ships dead.
//
// The deadlock shape it targets: a join/menu overlay mounted into a slot
// inside a hidden HUD, where the only code unhiding the HUD is behind a button in
// that overlay. Nothing can ever run it.
//
// A canvas game driven entirely by the keyboard, with a pause menu that starts
// hidden, is a real page with zero visible controls: its surface is the canvas. So a
// page showing a large canvas is never judged unreachable (canvas > 0), and
// requiring total == 0 keeps every ordinary page (a modal behind a visible button)
// well clear.
func unreachableUIIssue(pagePath string, total, hidden int, hiddenSel []string, canvas int) *ValidationIssue {
	if total > 0 || hidden == 0 || canvas > 0 {
		return nil
	}
	sample := append([]string(nil), hiddenSel...)
	sort.Strings(sample)
	return &ValidationIssue{
		Severity:      "error",
		Category:      "hidden_ui",
		Page:          pagePath,
		Deterministic: true,
		Signature:     strings.Join(sample, "|"),
		Message: fmt.Sprintf(
			"page %q has %d interactive control(s) and NOT ONE of them is visible on load - every one is inside a display:none / visibility:hidden subtree, so the page opens with nothing the user can click. Hidden controls: %s. "+
				"This is usually an overlay mounted inside a container that is itself hidden until after the overlay is used, which can never happen: check that the element the entry UI renders into is not a descendant of one that starts hidden, and move that mount point out of the hidden subtree (or reveal the ancestor at load).",
			pagePath, hidden, strings.Join(sample, "; ")),
	}
}
