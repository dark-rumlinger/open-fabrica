package observability

// write_flows / test_flows: the model's handle on of_test.js, the per-project
// user-journey script (sign up, add an item, play the game, draw, chat as two
// users, ...) that the flow_tests validator replays in headless Chrome after every
// build, and whose absence it flags on any project with pages. write_flows validates the
// script before storing it so a typo never reaches the validator; test_flows
// replays it now so a broken journey is caught while the code is still warm. The
// write_flows Doc is the model's only reference for the format.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"openfabrica/db/models"
	"openfabrica/internal/dbh"
	"openfabrica/internal/flowhooks"
	"openfabrica/internal/flowsnap"
	"openfabrica/internal/replaybudget"
	"openfabrica/probe"
	"openfabrica/store"
	"openfabrica/tools"
)

// DiagWriteFlows validates and stores of_test.js.
type DiagWriteFlows struct {
	Source string   `arg:"source,required" desc:"The full of_test.js module; replaces the existing file."`
	Drop   []string `arg:"drop" desc:"Names of saved flows you are deliberately removing (their feature is gone, or you renamed them). Every other saved flow must still be in source."`
}

func (a *DiagWriteFlows) ActionName() string { return "write_flows" }
func (a *DiagWriteFlows) Doc() string {
	return "Save of_test.js and replay it at once. Shape: export default { flows: [{ name, viewport?, steps: [...] }] }, plain data, one verb per step (verbs: the user_flows guide, section=writing-flows). The save REPLACES the file: a script missing a saved flow is refused unless that flow is listed in drop. Every save replays; replays that stop getting closer to green are refused."
}
func (a *DiagWriteFlows) Example() any {
	return &DiagWriteFlows{Source: `export default { flows: [
  { name: "sign up", steps: [{ goto: "/signup" }, { fill: "#email", value: "{{email}}" }, { fill: "#password", value: "{{password}}" }, { click: "text=Create account" }, { expectUrl: "/app" }] },
  { name: "chat", steps: [{ goto: "/chat" }, { goto: "/chat", as: "bob" }, { fill: "#msg", value: "hi {{run}}" }, { press: "Enter" }, { expectText: "hi {{run}}", as: "bob" }] },
  { name: "phone menu", viewport: "mobile", steps: [{ goto: "/" }, { click: "#menu-toggle" }, { click: "text=About" }, { expectUrl: "/about" }] }
] }`}
}
func (a *DiagWriteFlows) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if rej := userFlowsOffRejection(ctx); rej != nil {
		return rej, nil
	}
	suite, err := probe.ParseFlowSource(a.Source)
	if err != nil {
		return tools.FailErr(err), nil
	}
	saved := probe.LoadFlowSource(ctx.DB)
	if replaybudget.LastRed(budgetScope(ctx)) && rewritesToDiagnostics(saved, suite) {
		return tools.FailCode("diagnostic_suite",
			"not saved: this script drops most of the saved flows right after a red replay, which is a rewrite into diagnostics. "+
				"Keep the saved flows by name and change only the ones that need it. Find the cause in the code (and test_page), not by replacing the suite; "+
				"a flow that is wrong is fixed in place."), nil
	}
	if left := leftOut(droppedFlows(saved, suite), a.Drop); len(left) > 0 {
		return tools.FailCode("flows_dropped", "%s", fmt.Sprintf(
			"not saved: this script leaves out %d saved flow(s): %s. write_flows replaces the whole file, so they would be deleted. "+
				"Send the script again with them kept (the saved script is below; add your new flows to it). "+
				"If you really mean to delete one because its feature is gone or you renamed it, list its name in `drop`.\n\nSaved script:\n%s",
			len(left), strings.Join(left, ", "), clipMiddle(saved, maxSavedScriptEcho))), nil
	}
	if _, _, err := store.ArtifactSave(ctx.DB, &store.Artifact{
		Kind:        probe.FlowKind,
		Path:        probe.FlowPath,
		Storage:     store.StorageInline,
		Body:        a.Source,
		ContentType: "text/javascript",
		// Not "global": nothing may auto-inject or resolve this as a page asset.
		Scope: probe.FlowKind,
	}, ctx.ChangedBy()); err != nil {
		return tools.Fail("saving of_test.js failed: %v", err), nil
	}
	names := make([]string, 0, len(suite.Flows))
	steps := 0
	for _, f := range suite.Flows {
		names = append(names, f.Name)
		steps += len(f.Steps)
	}
	// A save is replayed at once: across 14 builds, 41 of 54 flow saves were followed by
	// a test_flows turn of its own and none by a test_flows in the same turn, so every
	// save cost a whole extra round-trip that re-read the conversation to run one call.
	// The result is exactly a test_flows result (red stays red), with the save's facts
	// added, so the model and triage read it the same way.
	out, envErr := replayFlows(ctx, suite, "", true)
	if envErr != nil {
		// The save landed; only the replay could not run here. Never report a
		// saved script as a failed write.
		out = tools.Ok(map[string]any{"replay_skipped": envErr.Error()})
	}
	data, _ := out.Data.(map[string]any)
	if data == nil {
		data = map[string]any{}
		out.Data = data
	}
	data["saved"], data["flows"], data["steps"] = probe.FlowPath, names, steps
	if envErr == nil && data["budget_exhausted"] == nil && data["stalled"] == nil {
		data["replayed"] = "of_test.js was saved and replayed at once; call test_flows only to re-run it after an app fix"
	}
	warnings := suite.Lint()
	warnings = append(warnings, suite.UnmatchedReferences(flowCorpus(ctx.DB))...)
	if len(warnings) > 0 {
		data["warnings"] = warnings
	}
	if len(suite.Notes) > 0 {
		data["normalized"] = suite.Notes
		data["normalized_note"] = "these steps were read in the canonical form shown; write them that way next time"
	}
	return out, nil
}

// userFlowsOffRejection refuses a flow call when the owner turned user-flow tests off.
// It returns nil when they are on or the setting cannot be read.
func userFlowsOffRejection(ctx *tools.ToolContext) *tools.Result {
	if ctx.GlobalDB == nil {
		return nil
	}
	p, err := models.GetProjectByID(ctx.GlobalDB, ctx.ProjectID)
	if err != nil || models.ProjectUserFlowsEnabled(p) {
		return nil
	}
	return tools.FailCode("user_flows_off",
		"user-flow tests are turned off for this project by its owner: do not write or replay of_test.js. "+
			"Nothing else is needed for this item; carry on with the rest of the work.")
}

// DiagTestFlows replays of_test.js (or one flow of it) against the running project.
type DiagTestFlows struct {
	Flow string `arg:"flow" desc:"Run only the flow with this name; omit to run all of them in order."`
}

func (a *DiagTestFlows) ActionName() string { return "test_flows" }
func (a *DiagTestFlows) Doc() string {
	return "Re-run of_test.js after an app fix (write_flows replays on its own save). Per failure: the step, the reason, JS errors, failed requests, the page's visible controls, and whether the app or the step is at fault."
}
func (a *DiagTestFlows) ReadOnly() bool { return true }
func (a *DiagTestFlows) Example() any   { return &DiagTestFlows{} }
func (a *DiagTestFlows) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if rej := userFlowsOffRejection(ctx); rej != nil {
		return rej, nil
	}
	src := probe.LoadFlowSource(ctx.DB)
	if src == "" {
		return tools.Fail("no of_test.js saved yet; write one with manage_testing action=write_flows"), nil
	}
	suite, err := probe.ParseFlowSource(src)
	if err != nil {
		return tools.FailErr(err), nil
	}
	if a.Flow != "" {
		found := false
		for _, f := range suite.Flows {
			found = found || f.Name == a.Flow
		}
		if !found {
			return tools.Fail("no flow named %q in of_test.js", a.Flow), nil
		}
	}
	out, envErr := replayFlows(ctx, suite, a.Flow, false)
	if envErr != nil {
		return tools.FailErr(envErr), nil
	}
	return out, nil
}

// budgetScope maps a tool call to its replay budget: the durable per-run budget
// when the engine set a run id (build and repair), else the in-memory per-project
// budget (LIVE owner chat, scheduled tasks, tests).
func budgetScope(ctx *tools.ToolContext) replaybudget.Scope {
	return replaybudget.Scope{DB: ctx.DB, ProjectID: ctx.ProjectID, RunID: ctx.RunID}
}

// replayFlows replays suite (or its flow named only) against the running project,
// within the replay budget, and shapes the outcome for the model. Shared by test_flows
// and write_flows, which replays every save at once (saved). envErr reports that the project
// could not be reached at all, which says nothing about the flows.
func replayFlows(ctx *tools.ToolContext, suite *probe.FlowSuite, only string, saved bool) (out *tools.Result, envErr error) {
	domain, port, err := testResolveDomain(ctx)
	if err != nil {
		return nil, err
	}
	scope := budgetScope(ctx)
	// A stall refuses replays of a script that has not changed. A save is a changed script,
	// and refusing to replay it would ship the rewrite unverified; the budget still caps it.
	if !saved && replaybudget.Stalled(scope) {
		return tools.Ok(map[string]any{
			"stalled": true,
			"note":    fmt.Sprintf("replays are refused for now: the last %d did not get closer to green. %s", replaybudget.MaxStalls, stallStopNote),
		}), nil
	}
	if !replaybudget.Take(scope) {
		return tools.Ok(map[string]any{
			"budget_exhausted": true,
			"note": fmt.Sprintf("replay budget reached (%d replays for this run): stop testing now, and do not hunt for the cause another way (no rewrites of of_test.js into diagnostic flows, no speculative app changes to make a red flow pass). "+
				"Complete the user-flows checklist item with what you have. The post-build validation replays of_test.js once more, and any flow still red is reported to the owner as a check that did not pass.",
				replaybudget.Max),
		}), nil
	}

	// Replay between a snapshot of the app's data and its restore, so the flows'
	// orders, uploads and status changes are rolled back afterwards.
	// LIVE caveat: a scheduled task (or any other write) that runs on the deployed
	// project DURING a LIVE replay lands inside this snapshot window and is rolled back
	// by the restore. Build and repair are single-threaded (tool calls run in order), so
	// this cannot happen there; in LIVE it is a rare edge - replays are owner-driven and
	// brief - and is left as a known limitation rather than guarded.
	var res probe.FlowResult
	// Flows also reach past the page: the app's outgoing mail (kept, not sent),
	// seeded accounts, scheduled tasks and webhooks.
	replayCtx := probe.WithFlowHooks(ctx.Context(), flowhooks.New(ctx.ProjectID, ctx.DB, ctx.JWTManager, ctx.Encryptor))
	skip, restoreErr := flowsnap.Around(replayCtx, func() {
		res = probe.RunFlowTests(replayCtx, suite, fmt.Sprintf("http://127.0.0.1:%d", port), domain, only)
	})
	if skip != "" {
		res = probe.FlowResult{Skipped: skip}
	}
	out = flowsResult(res)
	if res.Skipped == "" {
		addReplayProgress(out, scope, len(res.Failed), failureDepth(res.Issues), only == "")
	}
	if restoreErr != nil {
		if data, ok := out.Data.(map[string]any); ok {
			data["rollback_error"] = "the replay's data changes could not be fully rolled back: " + restoreErr.Error()
		}
	}
	return out, nil
}

// stallStopNote ends a replay loop that is not converging.
const stallStopNote = "Do not rewrite of_test.js into diagnostic flows and do not make speculative app changes to turn a flow green. " +
	"If you know the cause, make that one fix; otherwise complete the user-flows item with what you have. " +
	"The post-build validation replays of_test.js once more and reports any flow still red to the owner."

// addReplayProgress records the replay against the stall tracker and tells the model
// the replays left, the replays without progress, and a stop at the cap.
func addReplayProgress(out *tools.Result, scope replaybudget.Scope, failed, depth int, full bool) {
	data, ok := out.Data.(map[string]any)
	if !ok {
		return
	}
	stalls := replaybudget.Observe(scope, failed, depth, full)
	data["replays_left"] = replaybudget.Left(scope)
	if stalls == 0 {
		return
	}
	data["replays_without_progress"] = stalls
	if stalls >= replaybudget.MaxStalls {
		data["stop"] = fmt.Sprintf("STOP: %d replays in a row failed no fewer flows than before, and further replays are refused. %s", stalls, stallStopNote)
	}
}

// failStepRe reads the step number out of a flow failure ("failed at step 12 of 20").
var failStepRe = regexp.MustCompile(`failed at step (\d+) of`)

// failureDepth is how far the failing flows got: the sum of the step each one failed at.
// With the same flows failing, a larger depth means the script or app got further, which
// the stall tracker counts as progress.
func failureDepth(issues []probe.ValidationIssue) int {
	depth := 0
	for _, i := range issues {
		if m := failStepRe.FindStringSubmatch(i.Message); m != nil {
			n, _ := strconv.Atoi(m[1])
			depth += n
		}
	}
	return depth
}

// maxSavedScriptEcho bounds the saved script quoted back in a refusal.
const maxSavedScriptEcho = 20000

// leftOut removes the names the caller deliberately listed in drop from dropped.
func leftOut(dropped, drop []string) []string {
	allowed := map[string]bool{}
	for _, n := range drop {
		allowed[n] = true
	}
	var left []string
	for _, n := range dropped {
		if !allowed[n] {
			left = append(left, n)
		}
	}
	return left
}

// droppedFlows names the saved flows that next no longer has. Empty when there is no
// saved script, it does not parse, or next keeps them all.
func droppedFlows(saved string, next *probe.FlowSuite) []string {
	if saved == "" {
		return nil
	}
	prev, err := probe.ParseFlowSource(saved)
	if err != nil {
		return nil
	}
	kept := map[string]bool{}
	for _, f := range next.Flows {
		kept[f.Name] = true
	}
	var dropped []string
	for _, f := range prev.Flows {
		if !kept[f.Name] {
			dropped = append(dropped, f.Name)
		}
	}
	return dropped
}

// Row text from the project's own tables joins the corpus, because pages routinely render
// it (a seeded project title, a menu item). Bounded so a large table cannot slow a save.
const (
	corpusRowsPerTable = 300
	corpusDataBudget   = 1 << 20
)

// flowCorpus concatenates the bodies of the project's pages, layouts, scripts and
// stylesheets, plus the text in its seeded tables: what a flow's selectors and labels are
// checked against. Empty when the files cannot be read, which switches the check off.
func flowCorpus(db dbh.Handle) string {
	rows, err := db.Query(`SELECT COALESCE(body,'') FROM of_files WHERE is_deleted = 0 AND kind <> ?`, probe.FlowKind)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var body string
		if rows.Scan(&body) == nil {
			b.WriteString(body)
			b.WriteByte('\n')
		}
	}
	if rows.Err() != nil {
		return ""
	}
	appendTableText(&b, db)
	return b.String()
}

// appendTableText adds the text cells of the project's own tables (not the platform's
// of_* tables) to b, up to corpusDataBudget bytes. Any read error just ends the scan: the
// corpus then holds less, which only makes the check quieter.
func appendTableText(b *strings.Builder, db dbh.Handle) {
	names, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT LIKE 'of\_%' ESCAPE '\' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		return
	}
	var tables []string
	for names.Next() {
		var n string
		if names.Scan(&n) == nil {
			tables = append(tables, n)
		}
	}
	if names.Err() != nil {
		names.Close()
		return
	}
	names.Close()
	start := b.Len()
	for _, t := range tables {
		rows, err := db.Query(fmt.Sprintf(`SELECT * FROM "%s" LIMIT %d`, strings.ReplaceAll(t, `"`, `""`), corpusRowsPerTable))
		if err != nil {
			continue
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if rows.Scan(ptrs...) != nil {
				break
			}
			for _, v := range vals {
				if s, ok := v.(string); ok {
					b.WriteString(s)
					b.WriteByte('\n')
				}
			}
			if b.Len()-start > corpusDataBudget {
				rows.Close()
				return
			}
		}
		if rows.Err() != nil {
			rows.Close()
			continue
		}
		rows.Close()
	}
}

// rewritesToDiagnostics reports whether next replaces most of the saved suite: it keeps
// under half of the saved flows by name and is at most half its size.
func rewritesToDiagnostics(saved string, next *probe.FlowSuite) bool {
	if saved == "" {
		return false
	}
	prev, err := probe.ParseFlowSource(saved)
	if err != nil || len(prev.Flows) < 2 {
		return false
	}
	kept := 0
	for _, p := range prev.Flows {
		for _, n := range next.Flows {
			if p.Name == n.Name {
				kept++
				break
			}
		}
	}
	return kept*2 < len(prev.Flows) && len(next.Flows)*2 <= len(prev.Flows)
}

// maxFailureBytes bounds one failure's text so several fit in the tool result.
const maxFailureBytes = 2200

// clipMiddle cuts s to about max characters from the middle, keeping the start and the end.
func clipMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	tail := max * 2 / 5
	head := max - tail - 5
	return string(r[:head]) + "\n...\n" + string(r[len(r)-tail:])
}

// flowsResult shapes a flow run for the model. A run the browser could not do
// (no Chrome on this host, Chrome died, the run was cancelled) is NOT a red: like
// test_page, it succeeds with a skip note, so the model neither "fixes" a working
// app for an environment problem nor loops trying to turn it green.
func flowsResult(res probe.FlowResult) *tools.Result {
	if res.Skipped != "" {
		return tools.Ok(map[string]any{
			"skipped":   res.Skipped,
			"skip_kind": probe.SkipKind(res.Skipped),
			"note":      "the flows were not replayed on this host, which says nothing about the app: do not change code for this; of_test.js stays saved and is replayed wherever a browser is available",
		})
	}
	data := map[string]any{"passed": res.Passed, "failed": res.Failed}
	if len(res.WeakChecks) > 0 {
		data["weak_checks"] = res.WeakChecks
		data["weak_checks_note"] = "these steps passed but cannot show what they appear to; strengthen them when you next save the flows"
	}
	if len(res.Blocked) > 0 {
		data["blocked"] = res.Blocked
		data["blocked_note"] = "blocked flows did not run: an earlier flow they take a remembered value from failed first. Fix that flow; these are not separate failures."
	}
	if len(res.Issues) == 0 {
		return tools.Ok(data)
	}
	failures := make([]string, 0, len(res.Issues))
	for _, i := range res.Issues {
		failures = append(failures, clipMiddle(i.Message, maxFailureBytes))
	}
	data["failures"] = failures
	msg := fmt.Sprintf("%d of %d flows failed: %s", len(res.Failed), len(res.Failed)+len(res.Passed), strings.Join(res.Failed, ", "))
	if len(res.Blocked) > 0 {
		msg += fmt.Sprintf(" (%d more blocked by them)", len(res.Blocked))
	}
	return &tools.Result{Success: false, Error: msg, Data: data}
}
