package triage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/engine"
	"openfabrica/internal/buildstate"
	"openfabrica/providers/llm"
)

// collectRows runs scan over every row and returns the results, skipping a row
// whose scan errors. Local alias of db.CollectRows, kept terse for the many
// scan queries here (mirrors the admin helper of the same name).
func collectRows[T any](rows *sql.Rows, scan func(*sql.Rows) (T, error)) []T {
	return db.CollectRows(rows, scan)
}

// runLogTaskKind is the of_run_log kind emitted once per build-task completion
// (shared with the builds-cost analysis via internal/buildstate).
const runLogTaskKind = buildstate.LogTask

// triageFinding is one classified anomaly within a build. Findings are aggregated
// (by type+tool+code, or per file for a loop) so a noisy build yields a short
// list, not one entry per failed call. Tokens carries what a cost-lens finding
// re-bought; it is meaningless for the tool-error findings and omitted there.
type triageFinding struct {
	Type   string `json:"type"` // tool_reject | patch_loop | task_ordering | probe_failure | llm_error | build_failed | cache_thrash | cold_miss | redundant_save | reread | batch_tasks | self_correction | validator_issue | stage_failure | flow_chase | flow_fix_mix
	Detail string `json:"detail"`
	Tool   string `json:"tool,omitempty"`
	Code   string `json:"code,omitempty"`
	File   string `json:"file,omitempty"`
	Count  int    `json:"count,omitempty"`
	Tokens int64  `json:"tokens,omitempty"`
}

// triageBuild is the per-run report: headline stats, what the run spent, and its
// ranked findings. Verdict names the build in one word so the score needs no
// interpretation. ErrorRate is failed tool calls over tool calls - it says
// nothing about what the run cost, which is what Cost is for.
type triageBuild struct {
	Project     int             `json:"project"`
	ProjectName string          `json:"project_name"`
	Build       string          `json:"build"`
	Status      string          `json:"status"`
	Stage       string          `json:"stage,omitempty"`
	StartedAt   string          `json:"started_at"`
	DurationMS  int64           `json:"duration_ms,omitempty"`
	Model       string          `json:"model,omitempty"`
	Verdict     string          `json:"verdict"`
	ToolCalls   int             `json:"tool_calls"`
	ToolErrors  int             `json:"tool_errors"`
	ErrorRate   float64         `json:"error_rate"`
	Score       int             `json:"score"`
	Cost        triageCost      `json:"cost"`
	Findings    []triageFinding `json:"findings"`
	events      []costEvent     // timeline rows, rendered only by format=text
}

// Report is the whole response: cross-project rollups first, then builds ranked
// worst-first.
type Report struct {
	GeneratedAt     string         `json:"generated_at"`
	ProjectsScanned int            `json:"projects_scanned"`
	BuildsScanned   int            `json:"builds_scanned"`
	BuildsFlagged   int            `json:"builds_flagged"`
	CodeHistogram   map[string]int `json:"error_code_histogram"`
	TypeHistogram   map[string]int `json:"finding_type_histogram"`
	WorstFiles      []fileCount    `json:"worst_files"`
	Builds          []triageBuild  `json:"builds"`
}

type fileCount struct {
	File  string `json:"file"`
	Loops int    `json:"loops"`
}

// runRow is the of_runs projection the scan walks.
type runRow struct {
	ID          string
	Status      string
	Stage       string
	StartedAt   string
	CompletedAt sql.NullString
}

// listRuns returns a project's runs newest-first (capped at limit), or just the
// one matching buildFilter when set.
func listRuns(rdb *sql.DB, buildFilter string, limit int) []runRow {
	query := `SELECT id, status, COALESCE(stage, ''), COALESCE(started_at, ''), completed_at
		 FROM of_runs`
	args := []any{}
	if buildFilter != "" {
		query += ` WHERE id = ?`
		args = append(args, buildFilter)
	}
	query += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := rdb.Query(query, args...)
	if err != nil {
		return nil
	}
	return collectRows(rows, func(rows *sql.Rows) (runRow, error) {
		var rr runRow
		if err := rows.Scan(&rr.ID, &rr.Status, &rr.Stage, &rr.StartedAt, &rr.CompletedAt); err != nil {
			return rr, err
		}
		return rr, nil
	})
}

// scanRun computes the anomaly picture for one run: it replays the brain tool
// dialogue (assistant tool_calls paired with their role='tool' results) and rolls
// the failures up into a compact, weighted finding set.
func scanRun(rdb *sql.DB, projectID int, projectName string, run runRow) triageBuild {
	tb := triageBuild{
		Project:     projectID,
		ProjectName: projectName,
		Build:       run.ID,
		Status:      run.Status,
		Stage:       run.Stage,
		StartedAt:   run.StartedAt,
		DurationMS:  runDurationMS(run),
	}

	// of_chat_messages truncates tool results and drops the error `code`; recover
	// it from the full result the model was fed (of_llm_calls.request_messages).
	codeByID, injected := requestFacts(rdb, run.ID)

	// Aggregation buckets.
	rejectByKey := map[string]int{}        // "tool|code" -> count (generic rejects)
	rejectDetail := map[string][2]string{} // key -> {tool, code}
	editFailByFile := map[string]int{}     // manage_files failures per file\x00code
	taskOrdering := 0
	probeFailures := 0
	frictionRejects := 0             // rejects that actually cost the build; excludes healthy pre-ship catches
	probePending := map[string]int{} // probe target -> reds not yet turned green
	probeResolved := 0               // reds later cleared by a green probe of the same target
	var flowReds []string            // per red test_flows run: its failed flows not yet seen passing ("" once all have)

	dialogue := brainDialogue(rdb, run)
	for _, m := range dialogue {
		if m.role != "assistant" || m.toolCalls == "" {
			continue
		}
		var calls []llm.ToolCall
		if json.Unmarshal([]byte(m.toolCalls), &calls) != nil {
			continue
		}
		for _, tc := range calls {
			tb.ToolCalls++
			failed, code, errText := classifyResult(m.resultByID[tc.ID])
			// Probe resolution: a manage_testing target that fails and later
			// succeeds is the model's own gather->act->verify loop working - a red on
			// the way to green, not a defect. Track pending reds per target so only
			// the ones never turned green drive the score.
			action := flowToolAction(tc, m.resultByID[tc.ID])
			// write_flows replays every save at once, so its result is a replay too
			// whenever it names flows; a parse reject of the script is not one.
			replay := action == "test_flows" ||
				(action == "write_flows" && (flowsFailedRe.MatchString(errText) || len(flowPassed(m.resultByID[tc.ID])) > 0))
			if replay {
				// A user-flow replay is not a route probe: a red is a journey that broke
				// (an app bug or a stale script), and red-then-green is the flow tests
				// catching it before ship. Tracked on its own, never as probe_failure.
				if failed {
					flowReds = append(flowReds, flowFailedList(errText))
					tb.ToolErrors++
				} else if passed := flowPassed(m.resultByID[tc.ID]); len(passed) > 0 {
					// A red clears only when the flows it named pass: a green run of
					// one named flow, or of a suite rewritten to debug flows, says
					// nothing about the others. A budget refusal or a skip passes
					// nothing, so it clears nothing.
					whole := action == "write_flows" || !strings.Contains(tc.Arguments, `"flow"`)
					for i, rest := range flowReds {
						flowReds[i] = clearFlows(rest, passed, whole)
					}
				}
				continue
			}
			if tc.Name == "manage_testing" && action != "write_flows" {
				tgt := probeTarget(tc.Arguments)
				if failed {
					probePending[tgt]++
				} else {
					probeResolved += probePending[tgt]
					probePending[tgt] = 0
				}
			}
			if !failed {
				continue
			}
			if code == "" {
				code = codeByID[tc.ID]
			}
			tb.ToolErrors++
			_, file := parseToolArgs(tc.Arguments)
			switch {
			case tc.Name == "manage_tasks" && strings.Contains(errText, "depends on"):
				taskOrdering++
			case tc.Name == "manage_testing" && action != "write_flows":
				probeFailures++
			default:
				key := tc.Name + "|" + code
				rejectByKey[key]++
				rejectDetail[key] = [2]string{tc.Name, code}
				// A gate catching a genuine code defect (bad syntax, an undefined
				// reference, a cyclic import) before ship is the immune system
				// working, not friction: it is still surfaced as a finding, but it
				// does not count toward the score.
				if !isHealthyPreShipCode(code) {
					frictionRejects++
				}
			}
			if tc.Name == "manage_files" && file != "" {
				// Keyed on file AND code: a stuck loop is the SAME rejection
				// repeating (the message did not land). Two different gates
				// catching two different defects on one file, each fixed on the
				// next attempt, is sequential self-correction, not a loop.
				editFailByFile[file+"\x00"+code]++
			}
		}
	}

	// LLM-call level: errored provider calls in this run.
	llmErrors := 0
	_ = rdb.QueryRow(
		`SELECT COUNT(*) FROM of_llm_calls WHERE run_id = ? AND COALESCE(error_message, '') != ''`,
		run.ID).Scan(&llmErrors)
	_ = rdb.QueryRow(
		`SELECT model FROM of_llm_calls WHERE run_id = ? AND COALESCE(model, '') != ''
		 ORDER BY id DESC LIMIT 1`, run.ID).Scan(&tb.Model)

	// --- Emit findings, weighting the score by how much each costs a build. ---
	patchLoops := 0
	for key, n := range editFailByFile {
		if n < 2 {
			continue
		}
		file, code, _ := strings.Cut(key, "\x00")
		patchLoops++
		tb.Findings = append(tb.Findings, triageFinding{
			Type: "patch_loop", File: file, Code: code, Count: n,
			Detail: strconv.Itoa(n) + " failed edits to the same file with the same rejection - a stuck edit/repair loop",
		})
	}
	for key, n := range rejectByKey {
		d := rejectDetail[key]
		detail := n2("write/tool reject", n)
		if isHealthyPreShipCode(d[1]) {
			// Visible, but named for what it is: a defect the gate caught before ship,
			// which the model then fixed - not build friction (and not scored).
			detail = n2("gate caught a code defect pre-ship, self-corrected (not scored)", n)
		}
		tb.Findings = append(tb.Findings, triageFinding{
			Type: "tool_reject", Tool: d[0], Code: d[1], Count: n,
			Detail: detail,
		})
	}
	if taskOrdering > 0 {
		tb.Findings = append(tb.Findings, triageFinding{
			Type: "task_ordering", Tool: "manage_tasks", Count: taskOrdering,
			Detail: n2("complete rejected for an incomplete dependency", taskOrdering),
		})
	}
	unresolvedProbes := probeFailures - probeResolved
	if unresolvedProbes < 0 {
		unresolvedProbes = 0
	}
	if probeFailures > 0 {
		detail := n2("runtime probe failed (boot error / non-2xx)", probeFailures)
		if probeResolved > 0 {
			// Most probe reds are the model probing a route/endpoint before it is
			// built, then re-probing green. Only the unresolved ones score.
			detail += fmt.Sprintf(" - %d later cleared by a green probe of the same target (not scored); %d unresolved", probeResolved, unresolvedProbes)
		}
		tb.Findings = append(tb.Findings, triageFinding{
			Type: "probe_failure", Tool: "manage_testing", Count: probeFailures,
			Detail: detail,
		})
	}
	rl := loadRunLog(rdb, run.ID)
	flowPending := 0
	for _, rest := range flowReds {
		if rest != "" {
			flowPending++
		}
	}
	// Reds the build's own replays never cleared, but the final validator sweep
	// replayed of_test.js green: caught before ship by the post-build fix-pass.
	lateCatches := 0
	if flowPending > 0 && rl.FlowsGreenAtShip {
		lateCatches, flowPending = flowPending, 0
	}
	if caught := len(flowReds) - flowPending; caught > 0 {
		detail := n2("user-flow replay (of_test.js) went red and was green again by the end: an app bug or script drift caught before ship (not scored)", caught)
		if lateCatches > 0 {
			detail += fmt.Sprintf(" - %d cleared only by the post-build replay, never by the build's own", lateCatches)
		}
		tb.Findings = append(tb.Findings, triageFinding{
			Type: "flow_catch", Tool: "manage_testing", Count: caught,
			Detail: detail,
		})
	}
	if flowPending > 0 {
		tb.Findings = append(tb.Findings, triageFinding{
			Type: "flow_failure", Tool: "manage_testing", Count: flowPending,
			Detail: n2("user-flow replay (of_test.js) was still red when the model stopped testing: a journey ships broken or untested", flowPending) +
				"; the count is red replays, so one stuck flow replayed repeatedly counts each time",
		})
	}
	if llmErrors > 0 {
		tb.Findings = append(tb.Findings, triageFinding{
			Type: "llm_error", Count: llmErrors,
			Detail: n2("LLM provider call returned an error", llmErrors),
		})
	}
	if run.Status == "failed" {
		tb.Findings = append(tb.Findings, triageFinding{
			Type: "build_failed", Detail: "run ended in status=failed",
		})
	}
	for _, sf := range rl.StageFailures {
		tb.Findings = append(tb.Findings, triageFinding{
			Type: "stage_failure", Tool: sf.Stage, Code: sf.Status,
			Detail: clip("stage "+sf.Stage+" ended "+sf.Status+": "+sf.Error, 160),
		})
	}

	if tb.ToolCalls > 0 {
		tb.ErrorRate = float64(tb.ToolErrors) / float64(tb.ToolCalls)
	}
	// Score only what actually cost the build. Excluded on purpose: healthy pre-ship
	// catches (a defect the gate caught then the model self-corrected - the immune
	// system, not friction, so frictionRejects omits them) and resolved probe reds
	// (a route probed before it was built, then re-probed green - the model's own
	// verify loop). tb.ToolErrors still holds the raw total for the error-rate above.
	errorScore := frictionRejects + patchLoops*5 + unresolvedProbes*2 + flowPending*2 + taskOrdering + llmErrors*2
	if run.Status == "failed" {
		errorScore += 10
	}

	// The cost lens. It counts tokens and successful-but-wasteful actions; the
	// error lens above counts failed calls. The two sets are disjoint, so their
	// scores add without double-counting.
	runCalls := loadRunCalls(rdb, run.ID)
	cost, events := detectCost(runCalls)
	batchN, batchIter := rl.batchedTasks()
	failed := func(id string) bool {
		if len(dialogue) == 0 {
			return false
		}
		f, _, _ := classifyResult(dialogue[0].resultByID[id])
		return f
	}
	costF, costScore := costFindings(cost, events, detectFileChurn(dialogue),
		detectShredding(runCalls, failed, injected), batchN, batchIter, detectSelfCorrections(dialogue))
	tb.Findings = append(tb.Findings, costF...)

	// The flow lens: the replay loop's cost, and what its reds turned out to be.
	chase := detectFlowChase(dialogue, runCalls)
	flowF, flowScore := flowFindings(chase)
	tb.Findings = append(tb.Findings, flowF...)
	cost.FlowTokens, cost.FlowShare = chase.Tokens, chase.Share
	tb.Cost, tb.events = cost, events

	valF, valScore := validatorFindings(rl.Issues)
	tb.Findings = append(tb.Findings, valF...)

	// The checklist's own verdict on the run. Validators inspect what was built, so
	// work that never ran is invisible to them and has to be read off the plan.
	unbuiltF, unbuiltScore := unbuiltFindings(unbuiltTasks(rdb, run.ID))
	tb.Findings = append(tb.Findings, unbuiltF...)

	tb.Score = errorScore + costScore + flowScore + valScore + unbuiltScore
	tb.Verdict = verdictFor(run.Status, tb.Score, countValidatorErrors(rl.Issues), cost)
	return tb
}

// unbuiltTasks names the run's checklist items that never reached done, in plan order.
// of_tasks holds CURRENT status, so an item a later run finished reads as done here and
// this under-reports rather than crying wolf. Blocked counts: the owner is owed an answer
// about it either way. Scanned rather than indexed because a checklist is tens of rows.
func unbuiltTasks(rdb *sql.DB, runID string) []string {
	rows, err := rdb.Query(
		`SELECT COALESCE(title, '') FROM of_tasks
		 WHERE run_id = ? AND COALESCE(status, '') != 'done'
		 ORDER BY position ASC`, runID)
	if err != nil {
		return nil
	}
	return collectRows(rows, func(rows *sql.Rows) (string, error) {
		var title string
		err := rows.Scan(&title)
		return title, err
	})
}

// runDurationMS is a run's wall time, or 0 while it is still going. of_runs
// stores started_at space-separated and completed_at RFC3339, so both parse
// against both layouts.
func runDurationMS(run runRow) int64 {
	if !run.CompletedAt.Valid || run.CompletedAt.String == "" || run.StartedAt == "" {
		return 0
	}
	start, ok := parseRunTime(run.StartedAt)
	if !ok {
		return 0
	}
	end, ok := parseRunTime(run.CompletedAt.String)
	if !ok || end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

func parseRunTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// dialogueMsg is one reconstructed brain message with its paired tool result.
type dialogueMsg struct {
	role       string
	content    string
	toolCalls  string
	resultByID map[string]string // shared per-run: tool_call_id -> result content
}

// brainDialogue loads the brain chat for a run (time-windowed to the run, as the
// export is) and returns the assistant messages, each carrying a shared map of
// tool_call_id -> result content so a caller can pair a call with its outcome.
func brainDialogue(rdb *sql.DB, run runRow) []dialogueMsg {
	query := `SELECT role, COALESCE(content, ''), COALESCE(tool_calls, ''), COALESCE(tool_call_id, '')
		 FROM of_chat_messages WHERE session_id = 'brain'`
	args := []any{}
	// Normalize both sides with datetime(): run.StartedAt comes from a COALESCE'd
	// column (raw space-separated text) but run.CompletedAt from a bare DATETIME
	// column (RFC3339 ...T...Z), while created_at is space-separated. Without
	// datetime() the bare upper bound ('T' > ' ') over-includes a later run's early
	// brain messages into this run's window. See chat.LoadTimeline / exportPayload.
	if run.StartedAt != "" {
		query += ` AND datetime(created_at) >= datetime(?)`
		args = append(args, run.StartedAt)
	}
	if run.CompletedAt.Valid && run.CompletedAt.String != "" {
		query += ` AND datetime(created_at) <= datetime(?)`
		args = append(args, run.CompletedAt.String)
	}
	query += ` ORDER BY created_at ASC, id ASC`
	rows, err := rdb.Query(query, args...)
	if err != nil {
		return nil
	}
	type raw struct {
		role, content, toolCalls, toolCallID string
	}
	all := collectRows(rows, func(rows *sql.Rows) (raw, error) {
		var x raw
		err := rows.Scan(&x.role, &x.content, &x.toolCalls, &x.toolCallID)
		return x, err
	})
	resultByID := map[string]string{}
	for _, x := range all {
		if x.role == "tool" && x.toolCallID != "" {
			resultByID[x.toolCallID] = x.content
		}
	}
	var out []dialogueMsg
	for _, x := range all {
		if x.role == "assistant" {
			out = append(out, dialogueMsg{
				role: x.role, content: x.content, toolCalls: x.toolCalls, resultByID: resultByID,
			})
		}
	}
	return out
}

// codesByToolCallID recovers the error `code` for a run's failed tool calls.
// of_chat_messages stores a truncated result with no code; the full result the
// model was fed survives in of_llm_calls.request_messages. Returns tool_call_id ->
// code (first non-empty wins).
//
// Rows are scanned in ascending id order and only the messages each row newly
// appended are parsed (llm.AppendedRequestMessages), so every tool result is
// seen at its first appearance, before any compaction rewrite could summarize
// its body away. Residual gap: the run's final turn's tool results appear in
// no request, so their codes are unrecoverable here.
func codesByToolCallID(rdb *sql.DB, runID string) map[string]string {
	codes, _ := requestFacts(rdb, runID)
	return codes
}

// requestFacts is codesByToolCallID's single pass over request_messages, also returning,
// per of_llm_calls.id, whether the platform injected a message into that request (a
// fix-pass, a nudge) - a tiny turn answering one is not the model splitting its own work.
// Only what the row newly carries counts: the messages after its last assistant message,
// which covers both a delta row and a self-contained one.
func requestFacts(rdb *sql.DB, runID string) (map[string]string, map[int64]bool) {
	rows, err := rdb.Query(
		`SELECT id, COALESCE(request_messages, '') FROM of_llm_calls
		 WHERE run_id = ? AND source = 'engine' AND COALESCE(request_messages, '') != ''
		 ORDER BY id ASC`, runID)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()

	out := map[string]string{}
	injected := map[int64]bool{}
	for rows.Next() {
		var id int64
		var stored string
		if rows.Scan(&id, &stored) != nil {
			continue
		}
		appended := llm.AppendedRequestMessages(stored)
		type reqMsg struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		}
		msgs := make([]reqMsg, 0, len(appended))
		lastAssistant := -1
		for _, raw := range appended {
			var m reqMsg
			_ = json.Unmarshal(raw, &m) // a non-string content leaves Content empty
			if m.Role == "assistant" {
				lastAssistant = len(msgs)
			}
			msgs = append(msgs, m)
		}
		for _, m := range msgs[lastAssistant+1:] {
			if m.Role == "user" && !strings.HasPrefix(m.Content, "<!--__of_snapshot") {
				injected[id] = true
			}
		}
		for _, m := range msgs {
			if m.Role != "tool" || m.ToolCallID == "" || m.Content == "" {
				continue
			}
			if _, seen := out[m.ToolCallID]; seen {
				continue
			}
			var res struct {
				Code string `json:"code"`
			}
			if json.Unmarshal([]byte(m.Content), &res) == nil && res.Code != "" {
				out[m.ToolCallID] = res.Code
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil
	}
	return out, injected
}

// classifyResult inspects a tool result body and reports whether the call failed,
// its error code, and the error text. A result is a failure when it carries
// `"success": false`, or (for older shapes without success) a non-empty `error`.
//
// Only the summarized result reaches here: every summarizer routes a failure
// through tools.SummarizeError, which emits {success,error,code} alone. A failed
// call's `data` is already gone by this point, so nothing under it can be
// classified on - what a probe actually logged is recoverable from the validator
// issues in of_run_log, not from here.
func classifyResult(content string) (failed bool, code, errText string) {
	if content == "" {
		return false, "", ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(content), &m) != nil {
		return false, "", ""
	}
	if s, ok := m["success"].(bool); ok {
		failed = !s
	}
	if e, ok := m["error"].(string); ok && e != "" {
		errText = e
		if _, hasSuccess := m["success"]; !hasSuccess {
			failed = true
		}
	}
	if c, ok := m["code"].(string); ok {
		code = c
	}
	return failed, code, errText
}

// flowsFailedRe reads the flow names out of a red test_flows error. of_chat_messages
// keeps no `data` on a failed call, so the error text is the only record of them.
var flowsFailedRe = regexp.MustCompile(`^\d+ of \d+ flows failed: (.+?)(?: \(\d+ more blocked by them\))?$`)

// flowFailedList is the ", "-joined list of flows a red replay failed, as its error
// names them, or "*" when the error does not name them (cleared by any full green).
func flowFailedList(errText string) string {
	if m := flowsFailedRe.FindStringSubmatch(errText); m != nil {
		return m[1]
	}
	return "*"
}

// flowPassed is the data.passed list of a green test_flows result. A budget refusal
// (budget_exhausted) or a skip succeeds without one: nothing was replayed.
func flowPassed(content string) []string {
	var r struct {
		Data struct {
			Passed []string `json:"passed"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(content), &r) != nil {
		return nil
	}
	return r.Data.Passed
}

// clearFlows strikes each passed flow from a red's failed list and returns what is
// left ("" when all passed). Flow names may contain ", " themselves, so a name is
// matched whole at a list boundary rather than by splitting the list. whole reports a
// full-suite replay, the only kind that clears a red whose names are unknown ("*").
func clearFlows(rest string, passed []string, whole bool) string {
	if rest == "*" {
		if whole {
			return ""
		}
		return rest
	}
	for changed := true; changed && rest != ""; {
		changed = false
		for _, p := range passed {
			switch {
			case rest == p:
				rest = ""
			case strings.HasPrefix(rest, p+", "):
				rest = rest[len(p)+2:]
			case strings.HasSuffix(rest, ", "+p):
				rest = rest[:len(rest)-len(p)-2]
			case strings.Contains(rest, ", "+p+", "):
				rest = strings.Replace(rest, ", "+p+", ", ", ", 1)
			default:
				continue
			}
			changed = true
		}
	}
	return rest
}

// parseToolArgs pulls the action and the target artifact out of a tool call's
// JSON-string arguments (best-effort; empty on anything unparseable). The target
// comes from engine.CallTargetPath - the same resolution the guard screens on - so
// what this reports as a struggling artifact is exactly what the guard counts one
// on. A tool whose target this could not name would report no worst_files at all,
// hiding the very churn the endpoint exists to surface.
func parseToolArgs(arguments string) (action, file string) {
	if arguments == "" {
		return "", ""
	}
	var a map[string]any
	if json.Unmarshal([]byte(arguments), &a) != nil {
		return "", ""
	}
	action, _ = a["action"].(string)
	return action, engine.CallTargetPath(a)
}

// healthyPreShipCodes are the save-gate rejections that mean the model wrote a
// genuine code defect and the gate caught it BEFORE the artifact shipped - the
// build's immune system working, not build friction. They are still surfaced as
// findings, but they do not drive the noise score: a build whose only rejects are
// these self-corrected catches is clean, not noisy.
var healthyPreShipCodes = map[string]bool{
	"syntax_error":              true,
	"static_error":              true, // bundler-level sibling of syntax_error
	"undefined_reference":       true,
	"unresolved_import":         true,
	"dynamic_import_unresolved": true,
	"export_removed_in_use":     true,
	"entry_exports":             true,
	"cyclic_import":             true,
	"window_dead_global":        true,
	"window_global_bridge":      true,
	"reserved_module_name":      true,
	"colliding_module_name":     true,
	// Size gates: the model over-stuffed one file, the gate steered it to split -
	// the same catch-and-correct shape as the code gates above.
	"module_too_large_client": true,
	"oversized_body":          true,
	"page_not_fragment":       true, // page saved as a full HTML document (the doc-shell scramble)
	"page_has_layout_token":   true, // layout body ({{content}}/{{assets}}) misfiled as a page
	// Layout-body structural slips (the page/layout scramble family): the model
	// built a malformed shell, the gate caught it pre-ship, the model self-corrects.
	"layout_missing_content":        true, // SPA/full shell built without {{content}}
	"layout_duplicate_content":      true,
	"layout_duplicate_assets":       true,
	"layout_unknown_placeholder":    true,
	"layout_placeholder_in_rawtext": true, // {{content}}/{{assets}} inside <script>/<style>
	"layout_unclosed_rawtext":       true,
	"hand_written_asset_tag":        true, // <script src>/<link rel=stylesheet> typed into a page/layout body instead of attach/{{assets}}
}

func isHealthyPreShipCode(code string) bool { return healthyPreShipCodes[code] }

// probeTarget names the thing a manage_testing call probed, so a failed probe can
// be matched against a LATER successful probe of the same target: a red turned
// green is the model's verify loop working, not a defect. page_path and url_path
// address the SAME public route (test_page loads it in a browser, test_url checks
// its HTTP status), so both normalize to a shared "route:" key - a green via either
// clears a red on that route (e.g. a 503 on `/` during the build, then a green
// test_page once it is live). Endpoints are a separate namespace; untargeted probes
// fall back to the action so they still pair consistently.
func probeTarget(argsJSON string) string {
	var a struct {
		Action       string `json:"action"`
		PagePath     string `json:"page_path"`
		URLPath      string `json:"url_path"`
		EndpointPath string `json:"endpoint_path"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &a)
	// Query strings are call detail, not identity: a red on a bare endpoint cleared
	// by a green on the same endpoint with query params is one target re-probed
	// with the missing param supplied.
	stripQuery := func(s string) string {
		if i := strings.IndexAny(s, "?#"); i >= 0 {
			return s[:i]
		}
		return s
	}
	switch {
	case a.PagePath != "":
		return "route:" + stripQuery(a.PagePath)
	case a.URLPath != "":
		return "route:" + stripQuery(a.URLPath)
	case a.EndpointPath != "":
		return "endpoint:" + stripQuery(a.EndpointPath)
	default:
		return "action:" + a.Action
	}
}

// testingAction returns the action of a manage_testing call ("" for any other tool).
func testingAction(tc llm.ToolCall) string {
	if tc.Name != "manage_testing" {
		return ""
	}
	var a struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal([]byte(tc.Arguments), &a)
	return a.Action
}

// n2 renders "<label>" or "<label> (xN)" for a compact finding detail.
func n2(label string, n int) string {
	if n <= 1 {
		return label
	}
	return label + " (x" + strconv.Itoa(n) + ")"
}

// Options controls a triage scan.
type Options struct {
	BuildFilter  string // restrict to one run id ("" = all)
	Limit        int    // builds scanned per project, newest first
	IncludeClean bool   // include builds that produced no findings
}

// BuildReport scans the given projects and returns a ranked anomaly report. open
// returns a project's read DB by id; a project that fails to open is skipped
// (logged at debug when logger is non-nil). This is the analysis entry point the
// admin handler drives.
func BuildReport(projects []models.Project, open func(projectID int) (*sql.DB, error), logger *slog.Logger, opts Options) Report {
	report := Report{
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		CodeHistogram: map[string]int{},
		TypeHistogram: map[string]int{},
		Builds:        []triageBuild{},
	}
	fileLoops := map[string]int{}

	for _, p := range projects {
		rdb, err := open(p.ID)
		if err != nil {
			if logger != nil {
				logger.Debug("triage: open project db failed", "project", p.ID, "error", err)
			}
			continue
		}
		report.ProjectsScanned++
		for _, run := range listRuns(rdb, opts.BuildFilter, opts.Limit) {
			report.BuildsScanned++
			tb := scanRun(rdb, p.ID, p.Name, run)
			for _, f := range tb.Findings {
				report.TypeHistogram[f.Type]++
				if f.Code != "" {
					report.CodeHistogram[f.Code]++
				}
				if f.Type == "patch_loop" && f.File != "" {
					fileLoops[f.File]++
				}
			}
			// BuildsFlagged counts builds with findings, not builds returned:
			// IncludeClean adds clean builds to the response without flagging them.
			if len(tb.Findings) > 0 {
				report.BuildsFlagged++
			} else if !opts.IncludeClean {
				continue
			}
			report.Builds = append(report.Builds, tb)
		}
	}

	sort.SliceStable(report.Builds, func(i, j int) bool {
		return report.Builds[i].Score > report.Builds[j].Score
	})
	for f, n := range fileLoops {
		report.WorstFiles = append(report.WorstFiles, fileCount{File: f, Loops: n})
	}
	sort.SliceStable(report.WorstFiles, func(i, j int) bool {
		return report.WorstFiles[i].Loops > report.WorstFiles[j].Loops
	})
	return report
}
