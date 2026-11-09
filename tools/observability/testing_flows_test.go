package observability

import (
	"database/sql"
	"strings"
	"testing"

	"openfabrica/internal/replaybudget"
	"openfabrica/probe"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// With a run id set (build and repair), the replay budget is the durable per-run
// one in the project DB. This proves the migration created of_flow_budget (a
// missing table fails open, so the past-Max refusal would never fire) and that a
// ToolContext.RunID reaches the durable budget and bounds the whole run.
func TestReplayBudgetDurablePerRun(t *testing.T) {
	db := tooltest.ProjectDB(t)
	run := replaybudget.Scope{DB: db, ProjectID: 7, RunID: "run_durable"}
	for i := 0; i < replaybudget.Max; i++ {
		if !replaybudget.Take(run) {
			t.Fatalf("replay %d must be within budget", i+1)
		}
	}
	if replaybudget.Take(run) {
		t.Fatal("a replay past Max must be refused by the durable budget")
	}
	if !replaybudget.Take(replaybudget.Scope{DB: db, ProjectID: 7, RunID: "run_durable2"}) {
		t.Fatal("a different run on the same project DB starts with a fresh budget")
	}
}

// A run the browser could not do is reported as a success with a skip note, never
// as a red the model would try to "fix"; a real failure stays red.
func TestFlowsResult_SkipIsNotARed(t *testing.T) {
	skipped := flowsResult(probe.FlowResult{Skipped: "headless browser unavailable: no chrome"})
	if !skipped.Success {
		t.Fatalf("a skipped run must not be a failure: %+v", skipped)
	}
	data, _ := skipped.Data.(map[string]any)
	if data["skipped"] == nil || data["note"] == nil {
		t.Fatalf("a skip must say so and tell the model not to change code: %+v", data)
	}
	if data["skip_kind"] != probe.SkipNoBrowser {
		t.Fatalf("a skip must carry its kind for the admin card, got %v", data["skip_kind"])
	}

	red := flowsResult(probe.FlowResult{Failed: []string{"a"}, Issues: []probe.ValidationIssue{{Message: "boom"}}})
	if red.Success {
		t.Fatal("a failing flow must stay red")
	}
	green := flowsResult(probe.FlowResult{Passed: []string{"a"}})
	if !green.Success {
		t.Fatal("a passing run must be green")
	}
}

// A flow save is replayed in the same call rather than costing a separate test_flows
// turn. When the project cannot be reached the save still succeeds: the replay is
// reported as skipped, never as a failed write.
func TestWriteFlows_SavesAndReplaysInOneCall(t *testing.T) {
	ctx := &tools.ToolContext{DB: tooltest.ProjectDB(t), ProjectID: 1}
	res, err := (&DiagWriteFlows{Source: `export default { flows: [{ name: "home", steps: [{ goto: "/" }] }] }`}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("a valid script must save even when the replay cannot run: %+v (err %v)", res, err)
	}
	data, _ := res.Data.(map[string]any)
	if data["saved"] != probe.FlowPath || data["replay_skipped"] == nil || data["replayed"] != nil {
		t.Fatalf("want saved + replay_skipped and no replay claim, got %+v", data)
	}
	if probe.LoadFlowSource(ctx.DB) == "" {
		t.Fatal("the script must be stored")
	}
}

func suiteOf(t *testing.T, names ...string) *probe.FlowSuite {
	t.Helper()
	src := "export default { flows: ["
	for i, n := range names {
		if i > 0 {
			src += ","
		}
		src += `{ name: "` + n + `", steps: [{ goto: "/" }] }`
	}
	suite, err := probe.ParseFlowSource(src + "] }")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return suite
}

// A suite that keeps under half of the saved flows and is at most half its size is a
// rewrite into diagnostics; pruning, small suites and fixing in place are not.
func TestRewritesToDiagnostics(t *testing.T) {
	src := func(names ...string) string {
		s := "export default { flows: ["
		for i, n := range names {
			if i > 0 {
				s += ","
			}
			s += `{ name: "` + n + `", steps: [{ goto: "/" }] }`
		}
		return s + "] }"
	}
	cases := []struct {
		name  string
		saved string
		next  *probe.FlowSuite
		want  bool
	}{
		{"one probe flow replaces four", src("a", "b", "c", "d"), suiteOf(t, "probe"), true},
		{"two renamed probes replace four", src("a", "b", "c", "d"), suiteOf(t, "p1", "p2"), true},
		{"keeping half is a prune", src("a", "b", "c", "d"), suiteOf(t, "a", "b"), false},
		{"fixing flows in place", src("a", "b", "c", "d"), suiteOf(t, "a", "b", "c", "d"), false},
		{"a single saved flow", src("a"), suiteOf(t, "probe"), false},
		{"nothing saved yet", "", suiteOf(t, "probe"), false},
	}
	for _, c := range cases {
		if got := rewritesToDiagnostics(c.saved, c.next); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Each replay reports the budget left; reds that do not improve add a stall count and
// the cap adds a stop note.
func TestAddReplayProgressStopsAtTheCap(t *testing.T) {
	const project = 9101
	replaybudget.ResetStalls(replaybudget.Scope{ProjectID: project})
	t.Cleanup(func() { replaybudget.ResetStalls(replaybudget.Scope{ProjectID: project}) })
	var data map[string]any
	for i := 0; i <= replaybudget.MaxStalls; i++ {
		out := tools.Ok(map[string]any{})
		addReplayProgress(out, replaybudget.Scope{ProjectID: project}, 2, 0, true)
		data, _ = out.Data.(map[string]any)
		if i == 0 && data["replays_without_progress"] != nil {
			t.Fatalf("the first red only sets the baseline: %+v", data)
		}
	}
	if data["replays_left"] == nil || data["stop"] == nil {
		t.Fatalf("the replay that reaches the cap must say stop: %+v", data)
	}
	if !replaybudget.Stalled(replaybudget.Scope{ProjectID: project}) {
		t.Fatal("the project must be stalled")
	}
}

// A refused save changes nothing, and applies as soon as the latest replay is red.
func TestWriteFlows_RefusesDiagnosticRewriteWhileRed(t *testing.T) {
	const project = 9102
	replaybudget.ResetStalls(replaybudget.Scope{ProjectID: project})
	t.Cleanup(func() { replaybudget.ResetStalls(replaybudget.Scope{ProjectID: project}) })
	ctx := &tools.ToolContext{DB: tooltest.ProjectDB(t), ProjectID: project}
	full := `export default { flows: [` +
		`{ name: "a", steps: [{ goto: "/" }] },{ name: "b", steps: [{ goto: "/" }] },` +
		`{ name: "c", steps: [{ goto: "/" }] },{ name: "d", steps: [{ goto: "/" }] }] }`
	probeSrc := `export default { flows: [{ name: "probe", steps: [{ goto: "/" }] }] }`
	if res, _ := (&DiagWriteFlows{Source: full}).Run(ctx); res == nil || !res.Success {
		t.Fatalf("the first save must land: %+v", res)
	}
	// A red replay on record: keeping the saved flows is fine, replacing them is not.
	replaybudget.Observe(replaybudget.Scope{ProjectID: project}, 4, 0, true)
	if res, _ := (&DiagWriteFlows{Source: full}).Run(ctx); res == nil || !res.Success {
		t.Fatalf("re-saving the same suite must be allowed: %+v", res)
	}
	replaybudget.Observe(replaybudget.Scope{ProjectID: project}, 4, 0, true)
	res, _ := (&DiagWriteFlows{Source: probeSrc}).Run(ctx)
	if res == nil || res.Success || res.Code != "diagnostic_suite" {
		t.Fatalf("a rewrite into diagnostics while red must be refused: %+v", res)
	}
	if probe.LoadFlowSource(ctx.DB) != full {
		t.Fatal("a refused save must leave the stored script alone")
	}
	replaybudget.ResetStalls(replaybudget.Scope{ProjectID: project})
	if res, _ := (&DiagWriteFlows{Source: probeSrc, Drop: []string{"a", "b", "c", "d"}}).Run(ctx); res == nil || !res.Success {
		t.Fatalf("with no red replay on record a deliberate replacement is allowed: %+v", res)
	}
}

// One failure's text is cut from the middle, keeping which step failed and the verdict.
func TestClipMiddleKeepsStartAndVerdict(t *testing.T) {
	long := "step 3 failed: " + strings.Repeat("x", 4000) + "\nVerdict: fix the app"
	got := clipMiddle(long, maxFailureBytes)
	if len([]rune(got)) > maxFailureBytes+10 {
		t.Fatalf("not clipped: %d runes", len([]rune(got)))
	}
	if got[:15] != "step 3 failed: " || got[len(got)-len("Verdict: fix the app"):] != "Verdict: fix the app" {
		t.Fatalf("start and verdict must survive: %q", got)
	}
	if clipMiddle("short", maxFailureBytes) != "short" {
		t.Fatal("a short message is untouched")
	}
}

// With the owner's setting off, write_flows and test_flows stop before saving or
// replaying anything and say why; with it on (or unset) they run as usual.
func TestFlowTools_RefuseWhenUserFlowsAreOff(t *testing.T) {
	global, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })
	if _, err := global.Exec(`CREATE TABLE projects (
		id INTEGER PRIMARY KEY, name TEXT, domain TEXT, description TEXT, llm_model_id INTEGER,
		published INTEGER, mode TEXT, config TEXT NOT NULL DEFAULT '', source_project_id INTEGER,
		ambition_level TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := global.Exec(`INSERT INTO projects (id, name, published, mode, llm_model_id, ambition_level, config) VALUES (7, 'p', 0, 'building', 1, 'high', '{"user_flows":false}')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ctx := &tools.ToolContext{DB: tooltest.ProjectDB(t), GlobalDB: global, ProjectID: 7}

	src := `export default { flows: [{ name: "home", steps: [{ goto: "/" }] }] }`
	res, _ := (&DiagWriteFlows{Source: src}).Run(ctx)
	if res == nil || res.Success || res.Code != "user_flows_off" {
		t.Fatalf("write_flows must refuse, got %+v", res)
	}
	if probe.LoadFlowSource(ctx.DB) != "" {
		t.Fatal("a refused save must store nothing")
	}
	res, _ = (&DiagTestFlows{}).Run(ctx)
	if res == nil || res.Success || res.Code != "user_flows_off" {
		t.Fatalf("test_flows must refuse, got %+v", res)
	}

	if _, err := global.Exec(`UPDATE projects SET config = '' WHERE id = 7`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if res, _ := (&DiagWriteFlows{Source: src}).Run(ctx); res == nil || res.Success || res.Code != "user_flows_off" {
		t.Fatalf("an unset setting is the default (off) and must refuse, got %+v", res)
	}
	if _, err := global.Exec(`UPDATE projects SET config = '{"user_flows":true}' WHERE id = 7`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if res, _ := (&DiagWriteFlows{Source: src}).Run(ctx); res == nil || !res.Success {
		t.Fatalf("with the setting on the save must land, got %+v", res)
	}
}

// A save that leaves out saved flows is refused with the saved script, so a follow-up
// change that writes only the new feature's flows cannot silently delete old coverage and
// the model can paste the old flows back exactly. Naming a flow in drop deletes it on purpose.
func TestWriteFlows_RefusesToDropSavedFlows(t *testing.T) {
	const project = 9103
	replaybudget.ResetStalls(replaybudget.Scope{ProjectID: project})
	t.Cleanup(func() { replaybudget.ResetStalls(replaybudget.Scope{ProjectID: project}) })
	ctx := &tools.ToolContext{DB: tooltest.ProjectDB(t), ProjectID: project}
	first := `export default { flows: [{ name: "a", steps: [{ goto: "/" }] },{ name: "b", steps: [{ goto: "/" }] }] }`
	second := `export default { flows: [{ name: "a", steps: [{ goto: "/" }] },{ name: "c", steps: [{ goto: "/" }] }] }`
	res, _ := (&DiagWriteFlows{Source: first}).Run(ctx)
	if res == nil || !res.Success {
		t.Fatalf("the first save must land: %+v", res)
	}
	res, _ = (&DiagWriteFlows{Source: second}).Run(ctx)
	if res == nil || res.Success || res.Code != "flows_dropped" {
		t.Fatalf("leaving out flow b must be refused: %+v", res)
	}
	if !strings.Contains(res.Error, `name: "b"`) {
		t.Fatalf("the refusal must carry the saved script, got %q", res.Error)
	}
	if probe.LoadFlowSource(ctx.DB) != first {
		t.Fatal("a refused save must leave the stored script alone")
	}
	res, _ = (&DiagWriteFlows{Source: second, Drop: []string{"b"}}).Run(ctx)
	if res == nil || !res.Success {
		t.Fatalf("a flow named in drop may be removed: %+v", res)
	}
	merged := `export default { flows: [{ name: "a", steps: [{ goto: "/" }] },{ name: "c", steps: [{ goto: "/" }] },{ name: "d", steps: [{ goto: "/" }] }] }`
	if res, _ := (&DiagWriteFlows{Source: merged}).Run(ctx); res == nil || !res.Success {
		t.Fatalf("keeping every saved flow and adding one must be allowed: %+v", res)
	}
}

// Failing further into the steps is measured from the failure text.
func TestFailureDepthSumsFailingSteps(t *testing.T) {
	issues := []probe.ValidationIssue{
		{Message: `user flow "a" (of_test.js) failed at step 3 of 12 {"click":"#x"}: nothing`},
		{Message: `user flow "b" (of_test.js) failed at step 10 of 12 {"click":"#y"}: nothing`},
		{Message: "some other finding"},
	}
	if got := failureDepth(issues); got != 13 {
		t.Fatalf("depth must be 3+10, got %d", got)
	}
}

// Text rendered from the project's own seeded tables counts as present, so a flow that
// asserts a seeded title is not warned about; platform tables stay out of the corpus.
func TestFlowCorpusIncludesSeededTableText(t *testing.T) {
	db := tooltest.ProjectDB(t)
	if _, err := db.Exec(`CREATE TABLE projects (id INTEGER PRIMARY KEY, title TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (title) VALUES ('Northwind Coffee')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	corpus := flowCorpus(db)
	if !strings.Contains(corpus, "Northwind Coffee") {
		t.Fatalf("seeded row text must be in the corpus, got %d bytes", len(corpus))
	}
	suite, err := probe.ParseFlowSource(`export default { flows: [{ name: "f", steps: [{ goto: "/" }, { expectText: "Northwind Coffee" }] }] }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := suite.UnmatchedReferences(corpus + "\n<main>home</main>"); len(got) != 0 {
		t.Fatalf("a seeded title must not be reported as missing, got %v", got)
	}
}
