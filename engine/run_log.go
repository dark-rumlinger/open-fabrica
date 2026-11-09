package engine

// of_run_log appender: the unified, append-only journal of everything a run
// did. One table records stage transitions and tool events alike; readers
// order by id and branch on kind.

import (
	"encoding/json"
	"log/slog"

	"openfabrica/db"
	"openfabrica/internal/buildstate"
	"openfabrica/internal/sqliteretry"
	"openfabrica/providers/llm"
)

// Run-log kinds (values defined in internal/buildstate, shared with the admin views).
const (
	RunLogStage   = buildstate.LogStage   // ref = stage name; payload {status, error?}
	RunLogTool    = buildstate.LogTool    // ref = tool name;  payload {action, ok, summary}
	RunLogMessage = buildstate.LogMessage // ref = "";         payload {text}
	RunLogIssue   = buildstate.LogIssue   // ref = validator;  payload {issues: [...]}
	RunLogSession = buildstate.LogSession // ref = session id; payload {segment, end_reason, iterations}
	RunLogSummary = buildstate.LogSummary // ref = "";         payload {calls, tokens, cache stats}
	RunLogTask    = buildstate.LogTask    // ref = task id;    payload {iteration, title} - one row per task completion
	// RunLogEditStats: one row per build session, payload {round, saves, patches, anchor_misses, noops} - the
	// patch-first guardrail's raw data, aggregated into the run summary.
	RunLogEditStats = buildstate.LogEditStats
	// RunLogValidation records the FINAL validator sweep once, at the end of the
	// build's fix loop: ref = "", payload {issues: [...]}. Per-round RunLogIssue
	// entries are build history; this one is the authoritative verdict on what
	// actually shipped.
	RunLogValidation = buildstate.LogValidation
)

// runLogger appends run events for one project.
type runLogger struct {
	pdb    *db.ProjectDB
	logger *slog.Logger
}

// entry appends one row. payload is marshalled to JSON; a nil payload stores
// an empty string. Append failures are logged and swallowed - the journal is
// observability, never control flow.
func (l *runLogger) entry(runID, kind, ref string, payload any, inTokens, outTokens int, durationMs int64) {
	raw := ""
	if payload != nil {
		if b, err := json.Marshal(payload); err == nil {
			raw = string(b)
		}
	}
	if _, err := sqliteretry.Exec(l.pdb.Writer(),
		`INSERT INTO of_run_log (run_id, kind, ref, payload, input_tokens, output_tokens, duration_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		runID, kind, ref, raw, inTokens, outTokens, durationMs,
	); err != nil {
		l.logger.Debug("run log append failed", "kind", kind, "error", err)
	}
}

// taskComplete records that a build task transitioned to done at the given
// per-segment iteration. The build runs as ONE continuous session, so
// of_llm_calls carries no task_id; the read side windows build-stage call cost
// onto these completion events by created_at. Best-effort, off the LLM hot path.
func (l *runLogger) taskComplete(runID, taskID, title string, iteration int) {
	l.entry(runID, RunLogTask, taskID, map[string]any{
		"iteration": iteration,
		"title":     title,
	}, 0, 0, 0)
}

// editStats records one build session's edit-shape split (saves vs patches vs
// anchor misses vs no-ops) for the patch-first guardrail. round is 0 for the build
// session. Skipped when the session made no file edits at all.
func (l *runLogger) editStats(runID string, round int, res SessionResult) {
	if res.Saves == 0 && res.Patches == 0 && res.AnchorMisses == 0 && res.NoOps == 0 {
		return
	}
	l.entry(runID, RunLogEditStats, "", map[string]any{
		"round":         round,
		"saves":         res.Saves,
		"patches":       res.Patches,
		"anchor_misses": res.AnchorMisses,
		"noops":         res.NoOps,
	}, 0, 0, 0)
}

// stage logs a stage transition or completion.
func (l *runLogger) stage(runID, stage, status, errMsg string, inTokens, outTokens int, durationMs int64) {
	payload := map[string]string{"status": status}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	l.entry(runID, RunLogStage, stage, payload, inTokens, outTokens, durationMs)
}

// finalValidation records the FINAL validator sweep - the authoritative verdict on
// what shipped - once at the end of the build. It is logged even when the
// sweep is clean (empty issues), so a reader can tell "clean" from "never checked".
func (l *runLogger) finalValidation(runID string, issues []Issue) {
	l.entry(runID, RunLogValidation, "", map[string]any{"issues": issues}, 0, 0, 0)
}

// message logs an owner-visible narration line.
func (l *runLogger) message(runID, text string) {
	l.entry(runID, RunLogMessage, "", map[string]string{"text": text}, 0, 0, 0)
}

// summary aggregates the run's of_llm_calls into one run_summary entry so
// cost regressions (build call count, cache effectiveness, effective input)
// are visible per run without re-querying the raw call log. Effective input
// is priced per model via llm.CacheCostFactorsFor (transport estimate only
// when no price is on record). Best-effort.
func (l *runLogger) summary(runID string) {
	reader := l.pdb.Reader()

	var (
		calls, replayHits                                int64
		input, output, cacheRead, cacheCreate, effective int64
	)
	rows, err := reader.Query(
		`SELECT COALESCE(provider_type, ''), COALESCE(provider_name, ''), COALESCE(model, ''),
		        COUNT(*), COALESCE(SUM(cache_hit), 0),
		        COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cache_read_input_tokens), 0), COALESCE(SUM(cache_creation_input_tokens), 0)
		 FROM of_llm_calls WHERE run_id = ? AND source = 'engine'
		 GROUP BY provider_type, provider_name, model`, runID)
	if err != nil {
		l.logger.Debug("run summary query failed", "error", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var providerType, providerName, model string
		var c, hits, inT, outT, cr, cc int64
		if rows.Scan(&providerType, &providerName, &model, &c, &hits, &inT, &outT, &cr, &cc) != nil {
			continue
		}
		readF, createF := llm.CacheCostFactorsFor(llm.ModelKey{Provider: providerName, Model: model}, providerType)
		calls += c
		replayHits += hits
		input += inT
		output += outT
		cacheRead += cr
		cacheCreate += cc
		effective += inT + int64(float64(cr)*readF) + int64(float64(cc)*createF)
	}
	if err := rows.Err(); err != nil {
		l.logger.Debug("run summary read failed", "error", err)
		return
	}

	var buildCalls int64
	_ = reader.QueryRow(
		`SELECT COUNT(*) FROM of_llm_calls
		 WHERE run_id = ? AND source = 'engine' AND stage = 'build'`,
		runID).Scan(&buildCalls)

	// Aggregate the per-session edit_stats rows: the patch-first guardrail.
	// Anchor-miss rate over ~5% of patches is the documented revert trigger.
	var saves, patches, anchorMisses, noops int64
	if erows, err := reader.Query(
		`SELECT payload FROM of_run_log WHERE run_id = ? AND kind = ?`,
		runID, RunLogEditStats); err == nil {
		defer erows.Close()
		for erows.Next() {
			var raw string
			if erows.Scan(&raw) != nil {
				continue
			}
			var p struct {
				Saves        int64 `json:"saves"`
				Patches      int64 `json:"patches"`
				AnchorMisses int64 `json:"anchor_misses"`
				NoOps        int64 `json:"noops"`
			}
			if json.Unmarshal([]byte(raw), &p) == nil {
				saves += p.Saves
				patches += p.Patches
				anchorMisses += p.AnchorMisses
				noops += p.NoOps
			}
		}
		_ = erows.Err() // best-effort edit-stats aggregation; a late iteration error just yields partial totals
	}

	payload := map[string]any{
		"calls":                  calls,
		"replay_hits":            replayHits,
		"input_tokens":           input,
		"output_tokens":          output,
		"cache_read_tokens":      cacheRead,
		"cache_write_tokens":     cacheCreate,
		"billed_input_tokens":    input + cacheRead + cacheCreate,
		"effective_input_tokens": effective,
		"build_calls":            buildCalls,
		"saves":                  saves,
		"patches":                patches,
		"patch_anchor_misses":    anchorMisses,
		"patch_noops":            noops,
	}
	l.entry(runID, RunLogSummary, "", payload, 0, 0, 0)
}
