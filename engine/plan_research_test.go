package engine

import (
	"encoding/json"
	"testing"
)

func TestParsePrePlanResearchDecision(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`{"needs_research":true}`, true},
		{`{"needs_research":false}`, false},
		{"```json\n{\"needs_research\": true}\n```", true},
		{`invalid`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := parsePrePlanResearchDecision(tc.raw); got != tc.want {
			t.Errorf("parsePrePlanResearchDecision(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestPrePlanResearchGateSchemaValidJSON(t *testing.T) {
	s := prePlanResearchGateSchema()
	if !json.Valid(s.Schema) {
		t.Fatalf("schema is not valid JSON: %s", s.Schema)
	}
}
