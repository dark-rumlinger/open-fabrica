package engine

import (
	"strings"
	"testing"
)

// The decision-conformance validator skips any check it cannot evaluate ("never
// blocks on an uncomparable assertion") - an unknown op, or an artifact/field
// outside its vocabulary - so a typo silently disables that check unless the op
// is canonicalized first. ParsePlan canonicalizes the common op synonyms and
// planCheckWarnings surfaces whatever remains unevaluable (op OR artifact/field).

func TestParsePlanNormalizesDecisionOps(t *testing.T) {
	// All three checks target the validator's real vocabulary (websocket +
	// trigger), so only op normalization is under test - no resolvability warning.
	raw := `{
		"goal": "g",
		"decisions": [
			{"topic": "tick rate", "choice": "20hz", "check": {"artifact": "websocket", "id": "/ws/game", "field": "tick_hz", "op": ">=", "value": 20}},
			{"topic": "fulfillment", "choice": "server", "check": {"artifact": "trigger", "id": "payment.completed", "field": "count", "op": "EQUALS", "value": 0}},
			{"topic": "authority", "choice": "server", "check": {"artifact": "websocket", "id": "/ws/game", "field": "authoritative", "op": " Exists "}}
		]
	}`
	p, err := ParsePlan(raw)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	want := []string{"gte", "eq", "exists"}
	for i, w := range want {
		if got := p.Decisions[i].Check.Op; got != w {
			t.Errorf("decision %d: op = %q, want %q", i, got, w)
		}
	}
	if warns := planCheckWarnings(p); len(warns) != 0 {
		t.Errorf("normalized plan over the real vocabulary should carry no warnings, got %v", warns)
	}
}

func TestPlanCheckWarningsFlagUnknownOps(t *testing.T) {
	raw := `{
		"goal": "g",
		"decisions": [
			{"topic": "tick rate", "choice": "20hz", "check": {"artifact": "websocket", "id": "/ws/game", "field": "tick_hz", "op": "at_most", "value": 20}},
			{"topic": "auth", "choice": "password"}
		]
	}`
	p, err := ParsePlan(raw)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	warns := planCheckWarnings(p)
	if len(warns) != 1 {
		t.Fatalf("want exactly 1 warning for the unknown op, got %d: %v", len(warns), warns)
	}
	if !strings.Contains(warns[0], `"at_most"`) || !strings.Contains(warns[0], "tick rate") {
		t.Errorf("warning should name the op and the decision topic, got %q", warns[0])
	}
}

// TestPlanCheckWarningsFlagUnresolvableArtifact reproduces a planner failure mode:
// a model can fabricate a check for the guide-less "client-js split" decision
// over an artifact the validator never implemented (`task`), which Validate skips
// with NO other trace. A valid op (`eq`) over an unsupported artifact
// (`external_libraries`) is the same silent skip. Both must warn.
func TestPlanCheckWarningsFlagUnresolvableArtifact(t *testing.T) {
	raw := `{
		"goal": "g",
		"decisions": [
			{"topic": "client-js split", "choice": "subsystem-split", "check": {"artifact": "task", "id": "core-state", "field": "paths", "op": "includes", "value": "/files/x.js"}},
			{"topic": "rendering approach", "choice": "three.js", "check": {"artifact": "external_libraries", "id": "three.js", "field": "module_name", "op": "eq", "value": "three"}}
		]
	}`
	p, err := ParsePlan(raw)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	warns := planCheckWarnings(p)
	// "client-js split" trips BOTH an unknown-op AND an unresolvable-artifact
	// warning; "rendering approach" trips only the unresolvable-artifact one.
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, "client-js split") || !strings.Contains(joined, `artifact "task"`) {
		t.Errorf("expected an unresolvable-artifact warning naming the fabricated task check, got %v", warns)
	}
	if !strings.Contains(joined, "rendering approach") || !strings.Contains(joined, `artifact "external_libraries"`) {
		t.Errorf("expected a warning for the valid-op-but-unsupported-artifact check, got %v", warns)
	}
	if !strings.Contains(joined, `"includes"`) {
		t.Errorf("expected the unknown-op warning to still fire for the fabricated check, got %v", warns)
	}
}
