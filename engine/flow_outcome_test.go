package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/probe"
	"openfabrica/store"
)

func flowIssue(msg string) Issue {
	return Issue{Severity: store.SeverityError, Category: "flow", Message: msg, Deterministic: true}
}

// A flow finding alone is reported as a check that did not pass; anything else mixed
// in keeps the build's known-issues label.
func TestOnlyFlowIssues(t *testing.T) {
	runtime := Issue{Severity: store.SeverityError, Category: "runtime", Deterministic: true}
	if !onlyFlowIssues([]Issue{flowIssue("a"), flowIssue("b")}) {
		t.Error("flow findings alone must count as flow-only")
	}
	if onlyFlowIssues([]Issue{flowIssue("a"), runtime}) {
		t.Error("a mixed set is not flow-only")
	}
	if onlyFlowIssues(nil) {
		t.Error("an empty set is not flow-only")
	}
}

// The go-live note names the journeys the final sweep still reports, once each, and
// falls back to a generic name when the finding names none.
func TestUnverifiedFlowsAndSection(t *testing.T) {
	latest := []Issue{
		flowIssue(`user flow "revoke link" (of_test.js) failed at step 8 of 11 {}: x`),
		flowIssue(`user flow "revoke link" (of_test.js) failed at step 9 of 11 {}: x`),
		flowIssue("this project has pages but no of_test.js"),
		{Severity: store.SeverityWarning, Category: "flow", Message: `user flow "warned" failed`},
		{Severity: store.SeverityError, Category: "runtime", Message: "boom"},
	}
	names := unverifiedFlows(latest)
	if len(names) != 2 || names[0] != "revoke link" || names[1] != "the user-flow script" {
		t.Fatalf("names = %v", names)
	}
	section := flowUnverifiedSection(names)
	if !strings.Contains(section, "Flow tests that did not pass (2)") || !strings.Contains(section, "revoke link") {
		t.Errorf("section = %q", section)
	}
	if flowUnverifiedSection(nil) != "" {
		t.Error("no failed flows must render nothing")
	}
}

// The payload carries the failed flows for the card, and omits the field when none.
func TestGoLivePayloadCarriesFlowUnverified(t *testing.T) {
	p := buildGoLivePayload(nil, nil, "http://localhost:5733", "/projects/7/settings-sharing", false, []string{"revoke link"}, nil)
	if len(p.FlowUnverified) != 1 || p.KnownIssues {
		t.Errorf("payload = %+v", p)
	}
}

const threeFlowScript = `export default { flows: [` +
	`{ name: "a", steps: [{ goto: "/" }] },{ name: "b", steps: [{ goto: "/" }] },{ name: "c", steps: [{ goto: "/" }] }] }`

// The go-live flow result counts the saved flows, names the failed ones, and says so
// when the replay did not run instead of reading as a pass.
func TestBuildFlowCheck(t *testing.T) {
	red := buildFlowCheck([]Issue{flowIssue(`user flow "b" (of_test.js) failed at step 2 of 3 {}: x`)}, threeFlowScript)
	if red == nil || red.Total != 3 || len(red.Failed) != 1 || red.Failed[0] != "b" || red.Skipped {
		t.Fatalf("red = %+v", red)
	}

	green := buildFlowCheck(nil, threeFlowScript)
	if green == nil || green.Total != 3 || len(green.Failed) != 0 || green.Skipped {
		t.Fatalf("green = %+v", green)
	}

	skip := Issue{
		Severity:  store.SeverityWarning,
		Category:  flowCategory,
		Signature: probe.FlowSkippedSignaturePrefix + probe.SkipNoBrowser,
		Message:   "user-flow tests in of_test.js were skipped: headless browser unavailable",
	}
	skipped := buildFlowCheck([]Issue{skip}, threeFlowScript)
	if skipped == nil || !skipped.Skipped || skipped.SkipKind != probe.SkipNoBrowser || len(skipped.Failed) != 0 {
		t.Fatalf("skipped = %+v", skipped)
	}
	if !strings.Contains(flowSkippedSection(skipped), "not replayed") || flowSkippedSection(green) != "" || flowSkippedSection(nil) != "" {
		t.Error("only a skipped replay gets the not-replayed note")
	}

	if buildFlowCheck(nil, "") != nil {
		t.Error("a project with no script and no finding has nothing to report")
	}
}

// The payload carries flow_check, and omits it when there is none.
func TestGoLivePayloadCarriesFlowCheck(t *testing.T) {
	fc := &flowCheck{Total: 3, Failed: []string{"b"}}
	p := buildGoLivePayload(nil, nil, "http://localhost:5733", "/projects/7/settings-sharing", false, nil, fc)
	raw, err := json.Marshal(p)
	if err != nil || !strings.Contains(string(raw), `"flow_check":{"total":3,"failed":["b"]}`) {
		t.Fatalf("payload json = %s (err %v)", raw, err)
	}
	none, _ := json.Marshal(buildGoLivePayload(nil, nil, "u", "s", false, nil, nil))
	if strings.Contains(string(none), "flow_check") {
		t.Fatalf("an absent result must be omitted: %s", none)
	}
}

type namedValidator string

func (n namedValidator) Name() string { return string(n) }
func (namedValidator) Validate(context.Context, *sql.DB, string, string) []Issue {
	return nil
}

// The sweep leaves out only the user-flow validator when the owner turned the tests
// off: the simple page/module checks (runtime_probe, explore, the static validators)
// still run, so a flows-off project is still tested.
func TestWithoutFlowValidator(t *testing.T) {
	all := []Validator{
		namedValidator("runtime_probe"), namedValidator("explore"), namedValidator("static_code"),
		namedValidator(flowTestsValidatorName), namedValidator("websocket"),
	}
	got := withoutFlowValidator(all)
	if len(got) != len(all)-1 {
		t.Fatalf("exactly one validator should be removed, got %d of %d", len(got), len(all))
	}
	for _, v := range got {
		if v.Name() == flowTestsValidatorName {
			t.Fatal("the user-flow validator must be removed when tests are off")
		}
	}
	for _, name := range []string{"runtime_probe", "explore", "static_code", "websocket"} {
		found := false
		for _, v := range got {
			if v.Name() == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q must still run when user-flow tests are off", name)
		}
	}
	if len(all) != 5 {
		t.Fatal("the registered set must not be modified")
	}
}

// The user-flow checklist item is added only when the owner left the tests on, and the
// worker reads that choice from the project row.
func TestUserFlowsSettingGatesThePlanItem(t *testing.T) {
	mem, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	if _, err := mem.Exec(`CREATE TABLE projects (
		id INTEGER PRIMARY KEY, name TEXT, domain TEXT, description TEXT, llm_model_id INTEGER,
		published INTEGER, mode TEXT, config TEXT NOT NULL DEFAULT '', source_project_id INTEGER,
		ambition_level TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for id, cfg := range map[int]string{1: `{"user_flows":true}`, 2: ""} {
		if _, err := mem.Exec(`INSERT INTO projects (id, name, published, mode, llm_model_id, ambition_level, config) VALUES (?, 'p', 0, 'building', 1, 'high', ?)`, id, cfg); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	worker := func(id int) *Worker {
		return &Worker{deps: &Deps{DB: &db.DB{DB: mem}}, projectID: id}
	}
	plan := func() *Plan {
		return &Plan{Tasks: []PlanTaskSpec{{ID: "home", Title: "Home", Intent: "x", DoneWhen: "y", Paths: []string{"/"}}}}
	}

	on, off := plan(), plan()
	if w := worker(1); !w.userFlowsEnabled() || !ensureUserFlowsTask(on) {
		t.Fatal("with the default the item must be added")
	}
	if w := worker(2); w.userFlowsEnabled() {
		t.Fatal("a project with the tests off must read as off")
	}
	if len(off.Tasks) != 1 {
		t.Fatal("the plan for a project with the tests off must be left alone")
	}
	if len(on.Tasks) != 2 {
		t.Fatalf("the default plan must gain the user-flows item, got %d tasks", len(on.Tasks))
	}
}
