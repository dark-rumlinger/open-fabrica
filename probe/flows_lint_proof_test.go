package probe

import (
	"strings"
	"testing"
)

func lintOf(t *testing.T, src string) []string {
	t.Helper()
	suite, err := ParseFlowSource(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return suite.Lint()
}

func hasWarning(ws []string, part string) bool {
	for _, w := range ws {
		if strings.Contains(w, part) {
			return true
		}
	}
	return false
}

// A flow named like a journey between people must act as a second user; one that does is
// clean, and a single-user flow with an ordinary name is never asked to.
func TestLintProof_MultiUserNameNeedsASecondSession(t *testing.T) {
	ws := lintOf(t, `export default { flows: [
		{ name: "two players see each other", steps: [{ goto: "/" }, { click: "#join" }, { expectVisible: "#roster-count" }] },
		{ name: "chat between two users", steps: [{ goto: "/" }, { goto: "/", as: "bob" }, { fill: "#m", value: "hi" }, { press: "Enter" }, { expectText: "hi", as: "bob" }] },
		{ name: "join and wander", steps: [{ goto: "/" }, { click: "#join" }, { expectVisible: "#stage" }] },
	] }`)
	if !hasWarning(ws, `flow "two players see each other" is named like a journey between several people`) {
		t.Errorf("a multi-user name with no second session must warn, got %v", ws)
	}
	if hasWarning(ws, `"chat between two users"`) || hasWarning(ws, `"join and wander"`) {
		t.Errorf("flows that act as a second user, or promise nothing of the kind, must not warn: %v", ws)
	}
}

// A presence suite that acts as a second user must prove the despawn side somewhere: a
// peer leaves and the others see them gone. A join-only suite warns; one with a leave
// followed by a gone-assertion (even in a separate flow) does not.
func TestLintProof_PresenceNeedsDespawn(t *testing.T) {
	joinOnly := lintOf(t, `export default { flows: [
		{ name: "two players see each other", steps: [
			{ goto: "/" }, { click: "#join" },
			{ goto: "/", as: "bob" }, { click: "#join", as: "bob" },
			{ expectText: "2 online", in: "#count" } ] },
	] }`)
	if !hasWarning(joinOnly, "no flow proves the despawn side") {
		t.Errorf("a join-only presence suite must warn about despawn, got %v", joinOnly)
	}

	withLeave := lintOf(t, `export default { flows: [
		{ name: "two players see each other", steps: [
			{ goto: "/" }, { click: "#join" },
			{ goto: "/", as: "bob" }, { click: "#join", as: "bob" },
			{ expectText: "2 online", in: "#count" },
			{ leave: true, as: "bob" },
			{ expectText: "1 online", in: "#count" } ] },
	] }`)
	if hasWarning(withLeave, "no flow proves the despawn side") {
		t.Errorf("a suite that asserts the peer is gone after a leave must not warn: %v", withLeave)
	}

	// A leave with only a wait after it does not prove despawn (the Steel Thunder gap).
	leaveNoAssert := lintOf(t, `export default { flows: [
		{ name: "two tanks see each other", steps: [
			{ goto: "/" }, { click: "#join" },
			{ goto: "/", as: "bob" }, { click: "#join", as: "bob" },
			{ expectText: "ALPH", in: "#roster" },
			{ leave: true, as: "bob" },
			{ wait: 1500 } ] },
	] }`)
	if !hasWarning(leaveNoAssert, "no flow proves the despawn side") {
		t.Errorf("a leave with only a wait must still warn, got %v", leaveNoAssert)
	}

	// A single-user suite never gets the despawn warning.
	solo := lintOf(t, `export default { flows: [
		{ name: "join and wander", steps: [{ goto: "/" }, { click: "#join" }, { expectVisible: "#stage" }] },
	] }`)
	if hasWarning(solo, "no flow proves the despawn side") {
		t.Errorf("a single-user suite must not warn about despawn: %v", solo)
	}
}

// A flow named like an interaction needs a step that acts; a plain page tour does not.
func TestLintProof_InteractionNameNeedsAnAction(t *testing.T) {
	ws := lintOf(t, `export default { flows: [
		{ name: "add an item", steps: [{ goto: "/" }, { expectVisible: "#list" }] },
		{ name: "browse every page", steps: [{ goto: "/" }, { expectText: "Home" }, { goto: "/about" }, { expectText: "About" }] },
		{ name: "send a message", steps: [{ goto: "/" }, { fill: "#m", value: "hi" }, { press: "Enter" }, { expectText: "hi" }] },
	] }`)
	if !hasWarning(ws, `flow "add an item" is named like an interaction`) {
		t.Errorf("an interaction name with no acting step must warn, got %v", ws)
	}
	if hasWarning(ws, `"browse every page"`) || hasWarning(ws, `"send a message"`) {
		t.Errorf("a page tour and a flow that acts must not warn: %v", ws)
	}
}

// Assertions that hold for any state, read a test-only global, or inspect browser
// storage are called out; an assertion on what the page shows is not.
func TestLintProof_WeakExpectJs(t *testing.T) {
	ws := lintOf(t, `export default { flows: [{ name: "play", steps: [
		{ goto: "/" },
		{ expectJs: "window.__coins >= 0" },
		{ expectJs: "document.querySelectorAll('.row').length >= 0" },
		{ expectJs: "true" },
		{ expectJs: "JSON.parse(localStorage.getItem('app.v1')).items.length === 1" },
		{ expectJs: "document.querySelectorAll('.row').length >= 1" },
		{ expectJs: "parseFloat(document.getElementById('x').textContent) >= 0.5" },
	] }] }`)
	for _, want := range []string{"step 2", "step 3", "step 4", "reads a test-only global", "asserts on browser storage"} {
		if !hasWarning(ws, want) {
			t.Errorf("want a warning mentioning %q, got %v", want, ws)
		}
	}
	if hasWarning(ws, "step 6") || hasWarning(ws, "step 7") {
		t.Errorf("a positive count or a real threshold must not warn: %v", ws)
	}
}
