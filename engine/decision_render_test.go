package engine

import (
	"strings"
	"testing"
)

// TestRenderLockedDecisionsVerifiedOnlyWhenEnforceable pins the verified-annotation rule: the build intent
// seed may only annotate a decision "(verified: …)" when decision_conformance can
// actually enforce the check. A check on an artifact/field outside the validator's
// vocabulary, or with an op it cannot evaluate, silently skips-as-pass post-build,
// so "verified" would be a false assurance to the build model. Every decision must
// still render as a locked constraint either way.
func TestRenderLockedDecisionsVerifiedOnlyWhenEnforceable(t *testing.T) {
	backed := PlanDecision{ // websocket.tick_hz gt 0: resolvable field + known op -> enforced
		Topic: "realtime model", Choice: "tick",
		Check: &DecisionCheck{Artifact: "websocket", ID: "game", Field: "tick_hz", Op: "gt", Value: 0},
	}
	unresolvableArtifact := PlanDecision{ // table.* is outside resolveDecisionField's vocabulary
		Topic: "persistence", Choice: "sqlite",
		Check: &DecisionCheck{Artifact: "table", ID: "scores", Field: "high", Op: "eq", Value: 1},
	}
	unknownOp := PlanDecision{ // resolvable field, but "approx" is not a validDecisionOp
		Topic: "fulfillment", Choice: "trigger",
		Check: &DecisionCheck{Artifact: "trigger", ID: "payment.completed", Field: "count", Op: "approx", Value: 1},
	}

	out := renderLockedDecisions([]PlanDecision{backed, unresolvableArtifact, unknownOp})

	// The enforceable check is annotated verified.
	if !strings.Contains(out, "(verified: websocket `game`.tick_hz gt 0)") {
		t.Errorf("an enforceable check must render (verified: …):\n%s", out)
	}
	// Every decision still renders as a locked constraint.
	for _, want := range []string{"**realtime model** = `tick`", "**persistence** = `sqlite`", "**fulfillment** = `trigger`"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing locked-constraint line %q in:\n%s", want, out)
		}
	}
	// Neither unenforceable check may claim verification.
	if strings.Contains(out, "verified: table") {
		t.Errorf("an unresolvable-artifact check must NOT be annotated verified:\n%s", out)
	}
	if strings.Contains(out, "verified: trigger") {
		t.Errorf("an unknown-op check must NOT be annotated verified:\n%s", out)
	}
}

// TestRenderLockedDecisionsEmpty keeps the no-decisions case a no-op (no header),
// matching the pre-extraction inline behavior.
func TestRenderLockedDecisionsEmpty(t *testing.T) {
	if got := renderLockedDecisions(nil); got != "" {
		t.Errorf("no decisions must render nothing, got %q", got)
	}
}
