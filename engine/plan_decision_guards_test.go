package engine

// Two drift guards for the PLAN decision surface, mirroring the existing
// knowledge/tool drift guards. Both close a "silent bypass" gap: a coherence
// check or a schema field that looks enforced but, because the frame and the
// enforcement disagree, quietly does nothing.

import (
	"regexp"
	"strings"
	"testing"

	"openfabrica/pack"
)

// coherenceTopicKeys are the decision `topic` substrings the plan-coherence
// checks match on (plan_coherence.go: the ownership check keys on "ownership" /
// "access model"; the app-shape route-sprawl check keys on "shape"). Each MUST be
// a topic the plan frame actually instructs the planner to emit - otherwise a
// planner following the frame records a differently-worded topic, the substring
// never matches, and the guard silently never fires. Keep this in sync with the
// matchers when a coherence check is added or its keying changes.
var coherenceTopicKeys = []string{"ownership", "access model", "shape"}

// TestCoherenceTopicKeysArePinnedInPlanFrame fails if a substring a coherence
// check keys on is absent from plan/STAGE.md - i.e. the frame never tells the
// planner to produce a topic that substring would match. Backstops the app-shape
// route-sprawl bypass.
func TestCoherenceTopicKeysArePinnedInPlanFrame(t *testing.T) {
	frame := strings.ToLower(planFrameBody(t))
	for _, key := range coherenceTopicKeys {
		if !strings.Contains(frame, key) {
			t.Errorf("plan-coherence keys on topic substring %q, but plan/STAGE.md never mentions it - a planner following the frame would record a topic that never matches, so the check silently never fires. Pin a topic containing %q in the frame (or update the matcher).", key, key)
		}
	}
}

// decisionCheckFieldVocab is the full set of (field) tokens the conformance
// validator can read (wsNumericCols keys + reducer + trigger count). The schema
// must advertise ONLY the subset a guide '## Decision:' block actually provides a
// check for; the rest are latent validator capability, not planner-usable surface.
var decisionCheckFieldVocab = []string{
	"tick_hz", "authoritative", "reducer", "e2ee", "matchmaking", "room_gated", "ephemeral", "count",
}

var guideDecisionFieldRe = regexp.MustCompile(`field:\s*"([a-z_]+)"`)

// TestDecisionCheckFieldsAreGuideReachable ties the plan schema's advertised
// decision-check fields to what guide '## Decision:' blocks actually back. A
// field the schema advertises but no guide provides a check for is dead surface
// (a planner is told "do NOT invent a check", so it can never legitimately use
// it); a field a guide uses but the schema hides is unreachable. Backstops fix
// 1.5.
func TestDecisionCheckFieldsAreGuideReachable(t *testing.T) {
	desc := decisionFieldDescription(t)
	guideFields := decisionFieldsUsedByGuides(t)
	if len(guideFields) == 0 {
		t.Fatal("no field:\"...\" tokens found in any guide Decision block - did the guides move?")
	}

	for _, f := range decisionCheckFieldVocab {
		advertised := strings.Contains(desc, f)
		if advertised && !guideFields[f] {
			t.Errorf("plan schema advertises decision-check field %q but no guide '## Decision:' block provides a check for it - dead surface. Trim it from the field description, or add the guide Decision block that uses it.", f)
		}
		if guideFields[f] && !advertised {
			t.Errorf("a guide Decision block uses check field %q but the plan schema's field description doesn't advertise it - the planner can't legitimately emit it. Add it to the field description.", f)
		}
	}
}

// planFrameBody returns the raw body of the plan stage pack.
func planFrameBody(t *testing.T) string {
	t.Helper()
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	p := pack.BySlug(ps, "plan")
	if p == nil {
		t.Fatal("plan stage pack not found")
	}
	return p.Body
}

// decisionFieldDescription extracts the `field` property's description string
// from the plan response schema (the text the model reads for what's checkable).
func decisionFieldDescription(t *testing.T) string {
	t.Helper()
	i := strings.Index(planSchemaJSON, `"field":`)
	if i < 0 {
		t.Fatal("planSchemaJSON has no field property")
	}
	rest := planSchemaJSON[i:]
	const descAnchor = `"description": "`
	j := strings.Index(rest, descAnchor)
	if j < 0 {
		t.Fatal("field property has no description")
	}
	rest = rest[j+len(descAnchor):]
	e := strings.Index(rest, `"`)
	if e < 0 {
		t.Fatal("unterminated field description")
	}
	return rest[:e]
}

// decisionFieldsUsedByGuides scans every capability guide for the check `field`
// tokens its '## Decision:' blocks emit.
func decisionFieldsUsedByGuides(t *testing.T) map[string]bool {
	t.Helper()
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	out := map[string]bool{}
	for _, p := range ps {
		if p.Role != pack.RoleCapability {
			continue
		}
		for _, m := range guideDecisionFieldRe.FindAllStringSubmatch(p.Body, -1) {
			out[m[1]] = true
		}
	}
	return out
}
