package triage

// The cost lens. Triage's other half classifies calls the tools rejected; this
// half measures what a run spent getting to its result, whether or not anything
// failed. A build can rewrite its own history every few turns, re-buy the whole
// prefix at full price, and still report every tool call successful - so tool
// errors alone cannot rank builds by damage.

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"openfabrica/internal/buildstate"
	"openfabrica/internal/stringutil"
	"openfabrica/providers/llm"
)

const (
	// A prefix collapse is only an anomaly when it cost tokens: the history
	// shrinks harmlessly whenever compaction evicts bulk the model no longer
	// needs. What makes it a finding is the fresh input the next call re-buys.
	thrashMinFreshTokens = 5000
	// A call the cache served nothing for. Every segment opens with one, so the
	// floor keeps the legitimate cold start out of the count.
	coldMissMinTokens = 20000
	// Re-billed tokens convert to score at this rate.
	tokensPerCostPoint = 10000

	redundantSaveWeight    = 3
	rereadWeight           = 1
	validatorErrorWeight   = 4
	validatorWarningWeight = 1

	// Tasks landing together in one iteration mean the run made no incremental
	// progress, and per-task cost attribution degrades to an even split.
	batchTaskMinShared = 3
	// One re-read verifies a write; repeats are the model paging a file it
	// already owns back into context.
	rereadMinPerFile = 2
	// A full re-save of a body this small costs about what a patch would, so it is
	// not counted as a redundant rewrite. Matches the save tool's own threshold for
	// nudging a near-identical re-save toward patch.
	redundantSaveMinBytes = 1500

	// A shredded turn: it called a tool and produced almost nothing, having re-read
	// the whole prefix and paid the provider's fixed per-call latency to do it.
	// Output is the measure, not the call count: tool arguments ARE output, so a turn
	// declaring six tables costs ~700 tokens while one asking for a single endpoint
	// costs ~50. Turn count tracks billed input far better than the failure score
	// does, so without this the dominant cost would be invisible here.
	shredMaxOutputTokens = 300
	// Fewer avoidable tiny turns than this is noise, not a pattern worth a finding.
	shredMinTurns = 3
	// Shredded turns re-read a CACHED prefix, so their tokens are discounted to
	// full-price equivalents before scoring. Without this a single build's cache
	// reads would swamp every other signal in the ranking.
	shredCacheRateDivisor = 10

	// A checklist item that never reached done. The owner is told about it, so it
	// is not breakage, but it is the plan undelivered and outranks any churn.
	unbuiltTaskWeight = 4
)

// of_run_log kinds this lens reads, from the values the engine writes.
const (
	runLogIssueKind = buildstate.LogIssue
	runLogStageKind = buildstate.LogStage
	// runLogFinalKind is the engine's single authoritative record of the FINAL
	// validator sweep (engine.RunLogValidation). When present it is the verdict on
	// what shipped; the per-round issue entries are build history.
	runLogFinalKind = buildstate.LogValidation
)

// runCall is one engine LLM call in issue order - the row the prefix/cache
// diagnostics live on. SessionID is the engine session segment: prefix stability
// is only defined against the previous call of the SAME segment, and every
// segment's first call reports a zero prefix by construction.
type runCall struct {
	ID          int64 // of_llm_calls.id, joining the request-side facts (injected messages)
	Iteration   int
	Stage       string
	SessionID   string
	Input       int64
	Output      int64
	CacheRead   int64
	CacheCreate int64
	PrefixBytes int64
	// ToolCalls is how many tools the turn asked for. Counted in SQL so the
	// payload (a build's whole file bodies) never crosses into the sweep.
	ToolCalls int
	// Calls names each call (id, tool, action), extracted in SQL for the same reason.
	Calls []toolRef
	// At is when the call was made, zero when the row carries no usable time.
	At time.Time
}

// toolRef is one tool call of a turn, without its arguments.
type toolRef struct {
	ID, Name, Action string
}

// triageCost is what a run spent. Billed input is what the provider charges for
// (fresh + cache reads + cache writes); overhead is billed input per output
// token, the clearest single number for a run that thrashed.
type triageCost struct {
	Calls             int     `json:"calls"`
	BilledInputTokens int64   `json:"billed_input_tokens"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	CacheHitRatio     float64 `json:"cache_hit_ratio"`
	OverheadRatio     float64 `json:"overhead_ratio,omitempty"`
	RebilledTokens    int64   `json:"rebilled_tokens"`
	ColdMissTokens    int64   `json:"cold_miss_tokens"`
	// FlowTokens is the billed input from the first user-flow replay to the end of
	// the run, and FlowShare that over the run's total.
	FlowTokens int64   `json:"flow_tokens,omitempty"`
	FlowShare  float64 `json:"flow_share,omitempty"`
}

// costEvent is one call the cost lens flagged, kept for the text timeline.
type costEvent struct {
	Iteration  int    `json:"iteration"`
	Stage      string `json:"stage"`
	Kind       string `json:"kind"` // cache_thrash | cold_miss
	Input      int64  `json:"input_tokens"`
	CacheRead  int64  `json:"cache_read_tokens"`
	PrefixDrop int64  `json:"prefix_drop_bytes,omitempty"`
}

// loadRunCalls reads a run's engine calls in issue order. idx_of_llm_calls_run
// covers this exactly, so it stays cheap enough for the cross-project sweep.
func loadRunCalls(rdb *sql.DB, runID string) []runCall {
	// The calls column is "id|tool|action" per call, ";"-joined, built in SQL. arguments
	// is usually a JSON string, so it is re-parsed for its action, guarded by json_valid
	// so one malformed argument blob cannot fail the whole walk.
	rows, err := rdb.Query(
		`SELECT id, COALESCE(iteration, 0), COALESCE(stage, ''), COALESCE(session_id, ''),
		        COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
		        COALESCE(cache_read_input_tokens, 0), COALESCE(cache_creation_input_tokens, 0),
		        COALESCE(stable_prefix_bytes, 0), COALESCE(created_at, ''),
		        COALESCE(json_array_length(CASE WHEN response_tool_calls LIKE '[%'
		                 THEN response_tool_calls ELSE '[]' END), 0),
		        COALESCE((SELECT group_concat(
		                   COALESCE(json_extract(value, '$.id'), '') || '|' ||
		                   COALESCE(json_extract(value, '$.name'), '') || '|' ||
		                   COALESCE(CASE WHEN json_valid(json_extract(value, '$.arguments'))
		                            THEN json_extract(json_extract(value, '$.arguments'), '$.action') END, ''), ';')
		                  FROM json_each(CASE WHEN json_valid(response_tool_calls) AND response_tool_calls LIKE '[%'
		                                 THEN response_tool_calls ELSE '[]' END)), '')
		 FROM of_llm_calls
		 WHERE run_id = ? AND source = 'engine'
		 ORDER BY id ASC`, runID)
	if err != nil {
		return nil
	}
	return collectRows(rows, func(rows *sql.Rows) (runCall, error) {
		var c runCall
		var calls, at string
		err := rows.Scan(&c.ID, &c.Iteration, &c.Stage, &c.SessionID, &c.Input, &c.Output,
			&c.CacheRead, &c.CacheCreate, &c.PrefixBytes, &at, &c.ToolCalls, &calls)
		c.Calls = parseToolRefs(calls)
		c.At = parseCallTime(at)
		return c, err
	})
}

// parseToolRefs reads loadRunCalls' "id|tool|action;..." column.
func parseToolRefs(s string) []toolRef {
	if s == "" {
		return nil
	}
	var out []toolRef
	for _, part := range strings.Split(s, ";") {
		f := strings.SplitN(part, "|", 3)
		if len(f) != 3 {
			continue
		}
		out = append(out, toolRef{ID: f[0], Name: f[1], Action: f[2]})
	}
	return out
}

// detectCost totals a run's spend and flags the calls that re-bought context.
//
// A cache_thrash is a call whose stable prefix collapsed against the previous
// call of its segment AND that re-bought a large block of fresh input: the
// collapse says the history was rewritten, the fresh input says what it cost. A
// cold_miss is a call mid-segment that the cache served nothing for.
//
// Both are only meaningful within one segment. A segment's opening call reports
// a zero prefix and buys its context for the first time - comparing it to the
// last call of the previous segment invents a collapse that never happened.
func detectCost(calls []runCall) (triageCost, []costEvent) {
	cost := triageCost{Calls: len(calls)}
	var events []costEvent

	for i, c := range calls {
		cost.InputTokens += c.Input
		cost.OutputTokens += c.Output
		cost.CacheReadTokens += c.CacheRead
		cost.BilledInputTokens += c.Input + c.CacheRead + c.CacheCreate

		if i == 0 || calls[i-1].SessionID != c.SessionID {
			continue // a segment's cold start bills once and is not an anomaly
		}
		prev := calls[i-1]
		switch {
		case c.CacheRead == 0 && c.Input >= coldMissMinTokens:
			cost.ColdMissTokens += c.Input
			events = append(events, costEvent{
				Iteration: c.Iteration, Stage: c.Stage, Kind: "cold_miss",
				Input: c.Input, CacheRead: c.CacheRead,
			})
		case c.PrefixBytes < prev.PrefixBytes && c.Input >= thrashMinFreshTokens:
			cost.RebilledTokens += c.Input
			events = append(events, costEvent{
				Iteration: c.Iteration, Stage: c.Stage, Kind: "cache_thrash",
				Input: c.Input, CacheRead: c.CacheRead,
				PrefixDrop: prev.PrefixBytes - c.PrefixBytes,
			})
		}
	}

	if cost.BilledInputTokens > 0 {
		cost.CacheHitRatio = float64(cost.CacheReadTokens) / float64(cost.BilledInputTokens)
	}
	if cost.OutputTokens > 0 {
		cost.OverheadRatio = float64(cost.BilledInputTokens) / float64(cost.OutputTokens)
	}
	return cost, events
}

// shredding is how much of the BUILD stage went on tiny turns that could have ridden
// the turn before them.
type shredding struct {
	Turns      int   // avoidable tiny turns
	Needed     int   // tiny turns that had to wait for a result (not counted)
	StageTurns int   // build turns in total
	Tokens     int64 // billed input the avoidable turns re-read
}

// detectShredding counts the build's AVOIDABLE tiny turns. Every round-trip re-reads the
// whole prefix and pays a fixed per-call latency, and calls in one turn run in order, so a
// lone test, complete or small save sent right after a turn of successful saves could have
// ridden that turn for free. A tiny turn is NOT counted when it had to wait for a result:
// the session's first turn, a turn after a platform message (injected, keyed by call id),
// a turn after a failed call (failed, keyed by tool call id), or one acting on something
// it had just read or tested. Counting every tiny turn, as this once did, charged that
// necessary third of them as waste (14 builds: 115 of 196 tiny turns were avoidable).
//
// Build only: PLAN and EXPAND are single-shot by design. Tokens are what the counted
// turns actually billed.
func detectShredding(calls []runCall, failed func(toolCallID string) bool, injected map[int64]bool) shredding {
	var sh shredding
	var prev *runCall
	for i := range calls {
		c := &calls[i]
		if c.Stage != "build" {
			continue
		}
		sh.StageTurns++
		p := prev
		prev = c
		// A turn with no tool call is the closing narration, not shredded work.
		if c.ToolCalls < 1 || c.Output >= shredMaxOutputTokens {
			continue
		}
		if p == nil || p.SessionID != c.SessionID || injected[c.ID] || !ridesPrevious(*p, *c, failed) {
			sh.Needed++
			continue
		}
		sh.Turns++
		sh.Tokens += c.Input + c.CacheRead + c.CacheCreate
	}
	if sh.Turns < shredMinTurns {
		sh.Turns, sh.Tokens = 0, 0
	}
	return sh
}

// ridesPrevious reports whether turn c could have been sent inside turn p: p only saved
// or completed and every one of its calls succeeded, so nothing in its results was needed
// to argue c, and c reads nothing it would have to wait on.
func ridesPrevious(p, c runCall, failed func(string) bool) bool {
	if len(p.Calls) == 0 || len(c.Calls) == 0 {
		return false // unknown shape: never charge it
	}
	for _, r := range p.Calls {
		if failed != nil && failed(r.ID) {
			return false
		}
		if k := toolRefKind(r); k != "write" && k != "complete" {
			return false
		}
	}
	for _, r := range c.Calls {
		if toolRefKind(r) == "read" {
			return false
		}
	}
	return true
}

// toolRefKind sorts a call into what it does to the build: complete (a task tick),
// verify (a manage_testing probe or replay), read (gathers context the next call
// depends on), or write (everything else).
func toolRefKind(r toolRef) string {
	switch {
	case r.Name == "manage_tasks" && r.Action == "complete":
		return "complete"
	case r.Name == "manage_testing" && r.Action != "write_flows":
		return "verify"
	case r.Name == "find_tool" || r.Name == "manage_guides":
		return "read"
	}
	switch r.Action {
	case "get", "outline", "list", "read", "query", "describe", "list_tables", "check":
		return "read"
	}
	return "write"
}

// fileChurn is a run's per-artifact write/read pattern.
type fileChurn struct {
	saves   map[string]int // full-body writes per target
	rereads map[string]int // reads of a target this run already wrote
}

// detectFileChurn walks the tool dialogue for work the run redid: an artifact
// written more than once (the model rewriting rather than patching), and an
// artifact read back after it wrote it (paging its own output into context).
//
// Only calls that succeeded count. A rejected write leaves the artifact
// untouched, so the corrected re-save that follows is the run recovering, not
// redoing work - and the rejection is already the error lens's finding. Counting
// it here would bill one event to both lenses. Reads of a file the run never
// wrote are ordinary discovery and not counted either.
func detectFileChurn(dialogue []dialogueMsg) fileChurn {
	churn := fileChurn{saves: map[string]int{}, rereads: map[string]int{}}
	written := map[string]bool{}
	// A save right after a FAILED write on the same file is recovery, not a
	// redundant rewrite: after a rejected patch it is the tool's own instructed
	// fallback (the anchor-miss escalation says re-save after 2 misses), and after a
	// rejected save it is the corrected body the reject asked for.
	writeJustFailed := map[string]bool{}

	for _, m := range dialogue {
		if m.toolCalls == "" {
			continue
		}
		var calls []llm.ToolCall
		if json.Unmarshal([]byte(m.toolCalls), &calls) != nil {
			continue
		}
		for _, tc := range calls {
			action, file := parseToolArgs(tc.Arguments)
			if file == "" {
				continue
			}
			failed, _, _ := classifyResult(m.resultByID[tc.ID])
			if action == "patch" || action == "save" {
				if failed {
					writeJustFailed[file] = true
					continue
				}
				if action == "patch" {
					delete(writeJustFailed, file)
				}
			}
			if failed {
				continue
			}
			switch action {
			case "save":
				// The recovery exemption applies to a re-save of an existing file only:
				// when the rejected write was the file's creation, the corrected save is
				// still its first write and counts once.
				recovery := writeJustFailed[file] && written[file]
				delete(writeJustFailed, file)
				written[file] = true
				if recovery {
					continue
				}
				if n, ok := saveBodyLen(tc.Arguments); ok && n < redundantSaveMinBytes {
					continue
				}
				churn.saves[file]++
			case "get":
				// The read-dedup stub ("unchanged":true, body omitted) is a cheap
				// answer, not a paid re-read; counting it would let the dedup's own
				// success manufacture the finding.
				if written[file] && !isDedupStubResult(m.resultByID[tc.ID]) {
					churn.rereads[file]++
				}
			}
		}
	}
	return churn
}

// saveBodyLen returns the byte length of a save call's body argument. ok is false
// when the arguments do not parse or carry no string body, so an unknown size is
// never mistaken for a small one.
func saveBodyLen(arguments string) (n int, ok bool) {
	var a struct {
		Body *string `json:"body"`
	}
	if json.Unmarshal([]byte(arguments), &a) != nil || a.Body == nil {
		return 0, false
	}
	return len(*a.Body), true
}

// isDedupStubResult reports whether a get result is the read-dedup stub: the
// body was omitted because the model already holds it, so nothing was re-paid.
func isDedupStubResult(result string) bool {
	var r struct {
		Data struct {
			Unchanged bool `json:"unchanged"`
		} `json:"data"`
	}
	return json.Unmarshal([]byte(result), &r) == nil && r.Data.Unchanged
}

// selfCorrectionPhrases mark an assistant turn that found a defect in work it had
// already written. They are matched only on turns that also call a tool, so a
// plan-ahead mention does not count as a correction the run paid for.
var selfCorrectionPhrases = []string{
	"i notice", "the problem is", "the only problem", "need fixing",
	"needs fixing", "creates a cyclic", "i see the issues", "violates",
}

// detectSelfCorrections counts turns where the model caught its own mistake and
// acted on it. Prose-matched, so it is reported but never scored.
func detectSelfCorrections(dialogue []dialogueMsg) int {
	n := 0
	for _, m := range dialogue {
		if m.toolCalls == "" || m.content == "" {
			continue
		}
		low := strings.ToLower(m.content)
		for _, p := range selfCorrectionPhrases {
			if strings.Contains(low, p) {
				n++
				break
			}
		}
	}
	return n
}

// validatorIssue is one finding from a validator's of_run_log entry.
type validatorIssue struct {
	Severity string `json:"severity"`
	Category string `json:"category"`
	Page     string `json:"page"`
	File     string `json:"file"`
	Message  string `json:"message"`
}

// stageFailure is a stage the run did not carry to completion.
type stageFailure struct {
	Stage  string
	Status string
	Error  string
}

// runLog is a run's journal, partitioned by the kinds this lens reads. The three
// streams arrive in one query: the sweep pays a per-run cost for every project it
// opens, so each stream is a partition of one read, not a read of its own.
type runLog struct {
	// Issues are the validators' verdict on the shipped artifacts, keyed by
	// validator. A run whose every tool call succeeded can still end here with a
	// page that throws on load.
	Issues map[string][]validatorIssue
	// StageFailures are stages left failed, paused or blocked. of_runs carries
	// only the run-wide status, never which stage stopped or why.
	StageFailures []stageFailure
	// TasksByIteration counts task completions per iteration.
	TasksByIteration map[int]int
	// FlowsGreenAtShip reports a final sweep with no flow issue: the flow_tests
	// validator replayed of_test.js green on what shipped (a skip or a red would
	// have left a "flow" issue behind).
	FlowsGreenAtShip bool
}

// loadRunLog reads a run's journal. idx_of_run_log_run covers this exactly.
func loadRunLog(rdb *sql.DB, runID string) runLog {
	out := runLog{Issues: map[string][]validatorIssue{}, TasksByIteration: map[int]int{}}
	rows, err := rdb.Query(
		`SELECT kind, COALESCE(ref, ''), COALESCE(payload, '') FROM of_run_log
		 WHERE run_id = ? AND kind IN (?, ?, ?, ?) ORDER BY id ASC`,
		runID, runLogIssueKind, runLogStageKind, runLogTaskKind, runLogFinalKind)
	if err != nil {
		return out
	}
	type entry struct{ kind, ref, payload string }
	all := collectRows(rows, func(rows *sql.Rows) (entry, error) {
		var e entry
		err := rows.Scan(&e.kind, &e.ref, &e.payload)
		return e, err
	})
	var finalIssues []validatorIssue
	hasFinal := false
	for _, e := range all {
		switch e.kind {
		case runLogIssueKind:
			// Per-round validator findings. Accumulated only as the fallback for a run
			// with no final-validation entry (an older build); superseded below when one
			// exists, since a fix-pass re-runs the validators and a resolved issue simply
			// stops appearing.
			var p struct {
				Issues []validatorIssue `json:"issues"`
			}
			if json.Unmarshal([]byte(e.payload), &p) == nil {
				out.Issues[e.ref] = append(out.Issues[e.ref], p.Issues...)
			}
		case runLogFinalKind:
			// The authoritative final sweep: what the validators still reported on the
			// shipped artifacts. An empty payload means the build cleared everything.
			var p struct {
				Issues []validatorIssue `json:"issues"`
			}
			if json.Unmarshal([]byte(e.payload), &p) == nil {
				finalIssues = p.Issues
				hasFinal = true
			}
		case runLogStageKind:
			var p struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}
			if json.Unmarshal([]byte(e.payload), &p) != nil {
				continue
			}
			switch p.Status {
			case buildstate.StageFailed, buildstate.StagePaused, buildstate.StageBlocked:
				out.StageFailures = append(out.StageFailures,
					stageFailure{Stage: e.ref, Status: p.Status, Error: p.Error})
			}
		case runLogTaskKind:
			var p struct {
				Iteration int `json:"iteration"`
			}
			if json.Unmarshal([]byte(e.payload), &p) == nil {
				out.TasksByIteration[p.Iteration]++
			}
		}
	}

	// Reconcile against the final sweep: when the engine recorded one, it is the
	// verdict on the shipped artifacts, so discard the per-round history and key the
	// surviving issues by category. An empty final sweep clears the map, so an error a
	// fix-pass resolved stops reading as a shipped defect (and the build as broken).
	if hasFinal {
		out.FlowsGreenAtShip = true
		out.Issues = map[string][]validatorIssue{}
		for _, is := range finalIssues {
			key := is.Category
			if key == "flow" {
				out.FlowsGreenAtShip = false
			}
			if key == "" {
				key = "validator"
			}
			out.Issues[key] = append(out.Issues[key], is)
		}
	}
	return out
}

// batchedTasks reports the largest group of tasks completed on one iteration, and
// that iteration. Below batchTaskMinShared it reports 0.
func (rl runLog) batchedTasks() (n, iteration int) {
	for it, count := range rl.TasksByIteration {
		if count > n || (count == n && it < iteration) {
			n, iteration = count, it
		}
	}
	if n < batchTaskMinShared {
		return 0, 0
	}
	return n, iteration
}

// costFindings turns the cost lens' raw measurements into findings and the score
// they contribute. Tokens convert to points at tokensPerCostPoint so a run's
// token count cannot swamp the ranking; the action-shaped signals carry flat
// weights.
func costFindings(cost triageCost, events []costEvent, churn fileChurn, sh shredding,
	batchN, batchIter, selfCorrections int) ([]triageFinding, int) {

	var findings []triageFinding
	score := 0

	thrash, cold := 0, 0
	worstDrop, worstIter := int64(0), 0
	coldIter := 0
	for _, e := range events {
		switch e.Kind {
		case "cache_thrash":
			thrash++
			if e.PrefixDrop > worstDrop {
				worstDrop, worstIter = e.PrefixDrop, e.Iteration
			}
		case "cold_miss":
			cold++
			if coldIter == 0 {
				coldIter = e.Iteration
			}
		}
	}
	if thrash > 0 {
		findings = append(findings, triageFinding{
			Type: "cache_thrash", Count: thrash, Tokens: cost.RebilledTokens,
			Detail: "history rewritten mid-run, re-billing the prefix at full price; worst drop " +
				kb(worstDrop) + " at iteration " + strconv.Itoa(worstIter),
		})
		score += int(cost.RebilledTokens / tokensPerCostPoint)
	}
	if cold > 0 {
		findings = append(findings, triageFinding{
			Type: "cold_miss", Count: cold, Tokens: cost.ColdMissTokens,
			Detail: "call served nothing from cache, first at iteration " + strconv.Itoa(coldIter),
		})
		score += int(cost.ColdMissTokens / tokensPerCostPoint)
	}

	if files := repeatedTargets(churn.saves, 2); len(files) > 0 {
		findings = append(findings, triageFinding{
			Type: "redundant_save", Count: len(files), File: files[0],
			Detail: "artifact fully rewritten rather than patched: " + strings.Join(files, ", "),
		})
		score += len(files) * redundantSaveWeight
	}
	if files := repeatedTargets(churn.rereads, rereadMinPerFile); len(files) > 0 {
		findings = append(findings, triageFinding{
			Type: "reread", Count: len(files), File: files[0],
			Detail: "artifact read back repeatedly after this run wrote it: " + strings.Join(files, ", "),
		})
		score += len(files) * rereadWeight
	}
	if sh.Turns > 0 {
		detail := strconv.Itoa(sh.Turns) + " of " + strconv.Itoa(sh.StageTurns) +
			" build turns were a lone test, complete or small save right after successful saves; " +
			"calls in a turn run in order, so each could have ridden the turn before and re-read the prefix for nothing"
		switch {
		case sh.Needed == 1:
			detail += " (1 more tiny turn waited on a result and is not counted)"
		case sh.Needed > 1:
			detail += " (" + strconv.Itoa(sh.Needed) + " more tiny turns waited on a result and are not counted)"
		}
		findings = append(findings, triageFinding{
			Type: "turn_shredding", Count: sh.Turns, Tokens: sh.Tokens, Detail: detail,
		})
		score += int(sh.Tokens / (tokensPerCostPoint * shredCacheRateDivisor))
	}
	if batchN > 0 {
		// Surfaced, but NOT scored: completing several tasks in one call is a
		// measurement artifact (per-task cost becomes an even split), not build
		// friction, so it must not push an otherwise-clean build into "noisy".
		findings = append(findings, triageFinding{
			Type: "batch_tasks", Count: batchN, Tool: "manage_tasks",
			Detail: strconv.Itoa(batchN) + " tasks completed together at iteration " +
				strconv.Itoa(batchIter) + ", so per-task cost is an even split, not a measurement",
		})
	}
	if selfCorrections > 0 {
		findings = append(findings, triageFinding{
			Type: "self_correction", Count: selfCorrections,
			Detail: n2("model found a defect in work it had already written", selfCorrections),
		})
	}
	return findings, score
}

// unbuiltFindings reports checklist items the run never finished. Validators only
// inspect what was built, so an item that never ran leaves nothing for them to find -
// the checklist is the only record that the work was owed. A build can end here two
// ways: the model stopped early, or it built the thing and dropped the `complete`.
// Both ship a go-live report naming work as missing, so both belong in the ranking.
func unbuiltFindings(titles []string) ([]triageFinding, int) {
	if len(titles) == 0 {
		return nil, 0
	}
	return []triageFinding{{
		Type: "unbuilt_task", Count: len(titles),
		Detail: "checklist item(s) never reached done, so the owner was told this work is missing: " +
			strings.Join(titles, ", "),
	}}, len(titles) * unbuiltTaskWeight
}

// validatorFindings reports the gate's verdict on the run's artifacts and the
// score it contributes. An error here means the build shipped something broken,
// which outweighs any amount of churn on the way there.
func validatorFindings(byValidator map[string][]validatorIssue) ([]triageFinding, int) {
	var findings []triageFinding
	score := 0
	for _, ref := range sortedKeys(byValidator) {
		errs, warns := 0, 0
		detail, file := "", ""
		for _, is := range byValidator[ref] {
			if is.Severity == "error" {
				errs++
			} else {
				warns++
			}
			if detail == "" {
				detail, file = is.Message, is.File
				if file == "" {
					file = is.Page
				}
			}
		}
		count := errs + warns
		severity := "warning"
		if errs > 0 {
			severity = "error"
		}
		findings = append(findings, triageFinding{
			Type: "validator_issue", Tool: ref, Code: severity, File: file, Count: count,
			Detail: clip(detail, 160),
		})
		score += errs*validatorErrorWeight + warns*validatorWarningWeight
	}
	return findings, score
}

// countValidatorErrors totals the error-severity validator findings across a
// run's validators.
func countValidatorErrors(byValidator map[string][]validatorIssue) int {
	n := 0
	for _, issues := range byValidator {
		for _, is := range issues {
			if is.Severity == "error" {
				n++
			}
		}
	}
	return n
}

// verdictFor names a build in one word, so a caller need not read the score to
// know what it is looking at. A shipped defect outranks any spend.
func verdictFor(status string, score, validatorErrors int, cost triageCost) string {
	switch {
	case status == "failed" || validatorErrors > 0:
		return "broken"
	case cost.RebilledTokens+cost.ColdMissTokens >= tokensPerCostPoint*10,
		cost.FlowShare >= flowChaseExpensiveShare && cost.FlowTokens >= flowChaseExpensiveTokens:
		return "expensive"
	case score > 0:
		return "noisy"
	default:
		return "clean"
	}
}

// repeatedTargets lists the targets seen at least min times, worst first.
func repeatedTargets(counts map[string]int, min int) []string {
	var out []string
	for f, n := range counts {
		if n >= min {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func sortedKeys(m map[string][]validatorIssue) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func clip(s string, n int) string {
	return stringutil.Truncate(strings.TrimSpace(strings.ReplaceAll(s, "\n", " ")), n)
}

// kb renders a byte count for a finding detail.
func kb(b int64) string {
	if b < 1024 {
		return strconv.FormatInt(b, 10) + "B"
	}
	return strconv.FormatInt(b/1024, 10) + "KB"
}
