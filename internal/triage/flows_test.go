package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// flowCall is one scripted tool call and its result for the flow lens tests.
type flowCall struct {
	id, name, args, result string
}

func flowDialogue(steps []flowCall) []dialogueMsg {
	var calls []string
	results := map[string]string{}
	for _, s := range steps {
		c, _ := json.Marshal(map[string]string{"id": s.id, "name": s.name, "arguments": s.args})
		calls = append(calls, string(c))
		results[s.id] = s.result
	}
	return []dialogueMsg{{role: "assistant", toolCalls: "[" + strings.Join(calls, ",") + "]", resultByID: results}}
}

func writeFlowsArgs(names ...string) string {
	var parts []string
	for _, n := range names {
		parts = append(parts, `{ name: "`+n+`", steps: [] }`)
	}
	args, _ := json.Marshal(map[string]string{
		"action": "write_flows", "source": "export default { flows: [" + strings.Join(parts, ",") + "] }",
	})
	return string(args)
}

const (
	flowRed     = `{"success":false,"error":"1 of 2 flows failed: a"}`
	flowGreen   = `{"success":true,"data":{"failed":null,"passed":["a","b"]}}`
	flowRefused = `{"_compacted":"x","data":{"budget_exhausted":true},"success":true}`
	appSave     = `{"action":"save","file_path":"/files/app.js","body":"x"}`
)

// A red cleared after the model edited the app is a real bug caught; a red cleared by
// re-saving only the script is not. The mix is reported, never scored.
func TestFlowChase_FixMixSeparatesAppFromScript(t *testing.T) {
	fc := detectFlowChase(flowDialogue([]flowCall{
		{"1", "manage_testing", `{"action":"test_flows"}`, flowRed},
		{"2", "manage_files", appSave, `{"success":true}`},
		{"3", "manage_testing", `{"action":"test_flows"}`, flowGreen},
		{"4", "manage_testing", `{"action":"test_flows"}`, flowRed},
		{"5", "manage_testing", writeFlowsArgs("a", "b"), flowGreen},
	}), nil)
	if fc.AppFixes != 1 || fc.ScriptFixes != 1 {
		t.Errorf("want 1 app fix and 1 script fix, got %+v", fc)
	}
	findings, score := flowFindings(fc)
	if score != 0 {
		t.Errorf("a short, cleared loop must not score, got %d", score)
	}
	if len(findings) != 1 || findings[0].Type != "flow_fix_mix" {
		t.Errorf("want only the informational fix mix, got %+v", findings)
	}
}

// Budget refusals, a script rewritten to diagnostics, and app edits made while no replay
// is left are the chase signature.
func TestFlowChase_DetectsDiagnosticsAndBlindEdits(t *testing.T) {
	fc := detectFlowChase(flowDialogue([]flowCall{
		{"1", "manage_testing", writeFlowsArgs("a", "b", "c"), flowRed},
		{"2", "manage_testing", `{"action":"test_flows"}`, flowRed},
		{"3", "manage_testing", writeFlowsArgs("probe one"), flowRed},
		{"4", "manage_testing", writeFlowsArgs("probe two"), flowRefused},
		{"5", "manage_files", appSave, `{"success":true}`},
		{"6", "manage_files", appSave, `{"success":true}`},
		{"7", "manage_tasks", `{"action":"complete","task_id":"t"}`, `{"success":true}`},
	}), nil)
	if fc.Replays != 3 || fc.Exhausted != 1 || fc.Diagnostic != 1 || fc.BlindEdits != 2 {
		t.Errorf("want 3 replays, 1 refusal, 1 diagnostic rewrite, 2 blind edits, got %+v", fc)
	}
	findings, score := flowFindings(fc)
	if findings[0].Type != "flow_chase" || score == 0 {
		t.Errorf("a chase must be reported and score, got %+v score=%d", findings, score)
	}
}

// A parse reject is not a replay, and a green clears the blind-edit window.
func TestFlowChase_ParseRejectIsNotAReplayAndGreenResetsBlindEdits(t *testing.T) {
	fc := detectFlowChase(flowDialogue([]flowCall{
		{"1", "manage_testing", writeFlowsArgs("a"), `{"success":false,"error":"flow \"a\" step 1: has more than one verb"}`},
		{"2", "manage_testing", `{"action":"test_flows"}`, flowRefused},
		{"3", "manage_files", appSave, `{"success":true}`},
		{"4", "manage_testing", `{"action":"test_flows"}`, flowGreen},
		{"5", "manage_files", appSave, `{"success":true}`},
	}), nil)
	if fc.Replays != 1 || fc.BlindEdits != 1 {
		t.Errorf("want 1 replay and 1 blind edit, got %+v", fc)
	}
}

// The window starts at the first replay: its billed input over the run's total, and its
// wall time. A run that spent most of its input there reads as expensive.
func TestFlowChase_WindowAndVerdict(t *testing.T) {
	at := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	replay := []toolRef{{ID: "1", Name: "manage_testing", Action: "test_flows"}}
	calls := []runCall{
		{Input: 100_000, At: at},
		{Input: 100_000, At: at.Add(time.Minute), Calls: replay},
		{Input: 1_800_000, At: at.Add(11 * time.Minute)},
	}
	tokens, share, millis := flowWindow(calls)
	if tokens != 1_900_000 || share < 0.94 || share > 0.96 || millis != 10*60*1000 {
		t.Errorf("window = %d tokens, %.2f share, %dms", tokens, share, millis)
	}
	cost := triageCost{FlowTokens: tokens, FlowShare: share}
	if got := verdictFor("completed", 5, 0, cost); got != "expensive" {
		t.Errorf("verdict = %q, want expensive", got)
	}
	if got := verdictFor("completed", 5, 0, triageCost{FlowTokens: 200_000, FlowShare: 0.9}); got != "noisy" {
		t.Errorf("a small flow phase must not read as expensive, got %q", got)
	}
	if tk, sh, ms := flowWindow([]runCall{{Input: 5}}); tk != 0 || sh != 0 || ms != 0 {
		t.Errorf("a run with no replay has no flow window, got %d %v %d", tk, sh, ms)
	}
}

// Restructuring the suite after the first red is not a chase: it takes a replay that
// got no closer to green first.
func TestFlowChase_RestructureAfterFirstRedIsNotDiagnostics(t *testing.T) {
	fc := detectFlowChase(flowDialogue([]flowCall{
		{"1", "manage_testing", writeFlowsArgs("a", "b", "c", "d"), `{"success":false,"error":"4 of 4 flows failed: a, b, c, d"}`},
		{"2", "manage_testing", writeFlowsArgs("one", "two"), `{"success":false,"error":"2 of 2 flows failed: one, two"}`},
		{"3", "manage_testing", writeFlowsArgs("three"), `{"success":false,"error":"2 of 2 flows failed: one, two"}`},
	}), nil)
	if fc.Diagnostic != 0 {
		t.Errorf("a first restructure is not diagnostics: %+v", fc)
	}
}
