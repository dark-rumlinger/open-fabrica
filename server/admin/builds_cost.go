package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"openfabrica/internal/buildstate"
	"openfabrica/providers/llm"
)

// Cost returns the token economics for one run.
// GET /admin/api/projects/{projectID}/builds/{buildID}/cost
func (h *BuildsHandler) Cost(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	runID := chi.URLParam(r, "buildID")
	if runID == "" {
		writeError(w, http.StatusBadRequest, "missing build ID")
		return
	}
	summary, err := buildTokenSummary(projectDB.Reader(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read build token summary")
		return
	}
	summary["build_id"] = runID
	writeJSON(w, http.StatusOK, summary)
}

// effectiveInputTokens prices cached prefix tokens relative to fresh input
// using the model's own published cache ratios (llm.CacheCostFactorsFor,
// falling back to the transport estimate when no price is on record).
func effectiveInputTokens(providerType, providerName, model string, input, cacheRead, cacheCreate int64) int64 {
	readF, createF := llm.CacheCostFactorsFor(llm.ModelKey{Provider: providerName, Model: model}, providerType)
	return input +
		int64(float64(cacheRead)*readF) +
		int64(float64(cacheCreate)*createF)
}

// buildTokenSummary aggregates of_llm_calls for one run into a per-stage
// token breakdown plus totals.
func buildTokenSummary(db *sql.DB, runID string) (map[string]any, error) {
	rows, err := db.Query(
		`SELECT stage, COALESCE(provider_type, '') AS provider_type,
		        COALESCE(provider_name, '') AS provider_name, COALESCE(model, '') AS model,
		        COUNT(*) AS calls,
		        COALESCE(SUM(input_tokens), 0)  AS input_tokens,
		        COALESCE(SUM(output_tokens), 0) AS output_tokens,
		        COALESCE(SUM(cache_read_input_tokens), 0)     AS cache_read,
		        COALESCE(SUM(cache_creation_input_tokens), 0) AS cache_creation,
		        COALESCE(SUM(stable_prefix_bytes), 0)         AS sum_prefix_bytes,
		        COALESCE(SUM(total_request_bytes), 0)         AS sum_total_bytes,
		        COALESCE(MIN(NULLIF(total_request_bytes, 0)), 0) AS min_total_bytes
		 FROM of_llm_calls
		 WHERE run_id = ? AND source = 'engine'
		 GROUP BY stage, provider_type, provider_name, model`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type stageAgg struct {
		stage       string
		calls       int64
		input       int64
		output      int64
		cacheRead   int64
		cacheCreate int64
		effective   int64
		sumPrefix   int64 // Σ stable_prefix_bytes (re-read message prefix)
		sumTotal    int64 // Σ total_request_bytes (whole request incl system+tools)
		minTotal    int64 // smallest non-zero total_request_bytes ≈ cold prefix
	}
	stages := map[string]*stageAgg{}
	stageOrder := []string{}
	var totCalls, totInput, totOutput, totCacheRead, totCacheCreate, totEffective int64

	for rows.Next() {
		var stage, providerType, providerName, model string
		var calls, inT, outT, cacheR, cacheC int64
		var sumPrefix, sumTotal, minTotal int64
		if scanErr := rows.Scan(&stage, &providerType, &providerName, &model, &calls, &inT, &outT, &cacheR, &cacheC, &sumPrefix, &sumTotal, &minTotal); scanErr != nil {
			continue
		}
		agg, ok := stages[stage]
		if !ok {
			agg = &stageAgg{stage: stage}
			stages[stage] = agg
			stageOrder = append(stageOrder, stage)
		}
		// Effective tokens are priced per model (cache discounts differ per
		// vendor), so they accumulate here per grouped row rather than being
		// derived from the stage sums afterwards.
		effective := effectiveInputTokens(providerType, providerName, model, inT, cacheR, cacheC)
		agg.calls += calls
		agg.input += inT
		agg.output += outT
		agg.cacheRead += cacheR
		agg.cacheCreate += cacheC
		agg.effective += effective
		agg.sumPrefix += sumPrefix
		agg.sumTotal += sumTotal
		if minTotal > 0 && (agg.minTotal == 0 || minTotal < agg.minTotal) {
			agg.minTotal = minTotal
		}

		totCalls += calls
		totInput += inT
		totOutput += outT
		totCacheRead += cacheR
		totCacheCreate += cacheC
		totEffective += effective
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	pipelineOrder := map[string]int{"plan": 0, "build": 1, "live": 2}
	sort.SliceStable(stageOrder, func(i, j int) bool {
		oi, iok := pipelineOrder[stageOrder[i]]
		oj, jok := pipelineOrder[stageOrder[j]]
		if iok && jok {
			return oi < oj
		}
		if iok != jok {
			return iok
		}
		return false
	})

	stageRows := []map[string]any{}
	for _, name := range stageOrder {
		agg := stages[name]
		billed := agg.input + agg.cacheRead + agg.cacheCreate
		row := map[string]any{
			"stage":                  agg.stage,
			"calls":                  agg.calls,
			"input_tokens":           agg.input,
			"output_tokens":          agg.output,
			"cache_read_tokens":      agg.cacheRead,
			"cache_creation_tokens":  agg.cacheCreate,
			"billed_input_tokens":    billed,
			"effective_input_tokens": agg.effective,
		}
		if billed > 0 {
			row["cache_hit_ratio"] = float64(agg.cacheRead) / float64(billed)
		} else {
			row["cache_hit_ratio"] = 0.0
		}
		if agg.output > 0 {
			row["overhead_ratio"] = float64(billed) / float64(agg.output)
		}
		if agg.calls > 0 {
			// avg_reread_prefix_bytes: how much of each call the caches re-serve.
			// avg_request_bytes: whole request size. cold_prefix_bytes: the fixed
			// system+tools+seed floor (smallest request in the run). new_bytes ≈
			// avg_request − cold_prefix is the genuinely-new content per call.
			row["avg_reread_prefix_bytes"] = agg.sumPrefix / agg.calls
			row["avg_request_bytes"] = agg.sumTotal / agg.calls
			row["cold_prefix_bytes"] = agg.minTotal
		}
		stageRows = append(stageRows, row)
	}

	totBilled := totInput + totCacheRead + totCacheCreate
	// The cost panel (web/admin builds.js renderCostPanel) reads the run-wide
	// figures from a nested `totals` object and bails when `totals.calls` is
	// missing - so the totals MUST be nested here, not flattened to the top
	// level, or the whole panel silently disappears. Field names match the
	// per-stage rows (calls/input_tokens/output_tokens/.../cache_hit_ratio/
	// overhead_ratio) so the frontend reads totals and stages identically.
	totals := map[string]any{
		"calls":                  totCalls,
		"input_tokens":           totInput,
		"output_tokens":          totOutput,
		"cache_read_tokens":      totCacheRead,
		"cache_creation_tokens":  totCacheCreate,
		"billed_input_tokens":    totBilled,
		"effective_input_tokens": totEffective,
		"cache_hit_ratio":        0.0,
	}
	if totBilled > 0 {
		totals["cache_hit_ratio"] = float64(totCacheRead) / float64(totBilled)
	}
	if totOutput > 0 {
		totals["overhead_ratio"] = float64(totBilled) / float64(totOutput)
	}
	result := map[string]any{
		"stages": stageRows,
		"totals": totals,
	}
	// Per-task cost is a best-effort overlay: it windows build-stage call cost
	// onto task-completion events. Older DBs / minimal schemas without
	// of_run_log (or created_at) simply omit `tasks` - it must never fail the
	// summary or the Cost endpoint.
	if taskRows, unattributed, taskErr := buildTaskAttribution(db, runID); taskErr == nil {
		result["tasks"] = taskRows
		result["tasks_unattributed"] = unattributed
	}
	return result, nil
}

// runLogTaskKind is the of_run_log kind emitted once per build-task completion.
const runLogTaskKind = buildstate.LogTask

// taskCompletion is one build-task done event (of_run_log kind='task'), in
// completion order; TS is the wall-clock second it landed.
type taskCompletion struct {
	TaskID    string
	Title     string
	TS        string
	Iteration int
}

// buildCall is one build-stage LLM call with its pre-priced effective input
// cost, in wall-clock order.
type buildCall struct {
	Effective int64
	Output    int64
	DurMs     int64
	TS        string
}

// taskCost is the cost attributed to one task (or the residual bucket).
type taskCost struct {
	TaskID    string
	Title     string
	Iteration int
	Calls     int
	Effective int64
	Output    int64
	DurMs     int64
	Estimated bool
}

// loadBuildCalls returns the run's build-stage LLM calls in wall-clock order,
// each pre-priced per model.
func loadBuildCalls(db *sql.DB, runID string) ([]buildCall, error) {
	rows, err := db.Query(
		`SELECT COALESCE(provider_type,''), COALESCE(provider_name,''), COALESCE(model,''),
		        input_tokens, output_tokens,
		        cache_read_input_tokens, cache_creation_input_tokens,
		        duration_ms, COALESCE(created_at,'')
		   FROM of_llm_calls
		  WHERE run_id = ? AND source = 'engine' AND stage = 'build'
		  ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var calls []buildCall
	for rows.Next() {
		var pt, pn, model, ts string
		var inT, outT, cr, cc, dur int64
		if scanErr := rows.Scan(&pt, &pn, &model, &inT, &outT, &cr, &cc, &dur, &ts); scanErr != nil {
			continue
		}
		calls = append(calls, buildCall{
			Effective: effectiveInputTokens(pt, pn, model, inT, cr, cc),
			Output:    outT,
			DurMs:     dur,
			TS:        ts,
		})
	}
	return calls, rows.Err()
}

// attributeBuildCost windows build-stage LLM cost onto per-task completion
// events. Each call is attributed to the first completion at or after the
// call's timestamp; when K tasks complete in the same second (a batched
// multi-complete turn) that window's cost is split evenly and those tasks are
// flagged estimated. Calls after the last completion (final page probe / wiring
// turns) fall into the residual. The split hands the integer remainder to the
// earliest tasks so per-task sums reconcile EXACTLY to the windowed total;
// adding the residual, the grand total equals the sum over calls. comps must be
// in completion (ascending-TS) order and calls in ascending-TS order.
//
// This is a deliberate estimate: the single continuous build session cannot
// attribute a call to a task directly (one turn advances several tasks), so
// attribution is by completion window, not by ground truth.
func attributeBuildCost(comps []taskCompletion, calls []buildCall) (rows []taskCost, residual taskCost) {
	// Group consecutive completions sharing a timestamp second into one turn.
	type group struct {
		ts    string
		tasks []taskCompletion
	}
	var groups []group
	for _, c := range comps {
		if n := len(groups); n > 0 && groups[n-1].ts == c.TS {
			groups[n-1].tasks = append(groups[n-1].tasks, c)
			continue
		}
		groups = append(groups, group{ts: c.TS, tasks: []taskCompletion{c}})
	}

	order := []string{}
	accs := map[string]*taskCost{}
	ci := 0
	for _, g := range groups {
		start := ci
		for ci < len(calls) && calls[ci].TS <= g.ts {
			ci++
		}
		window := calls[start:ci]
		k := int64(len(g.tasks))
		if k == 0 {
			continue
		}
		var wEff, wOut, wDur int64
		for _, cl := range window {
			wEff += cl.Effective
			wOut += cl.Output
			wDur += cl.DurMs
		}
		wCalls := int64(len(window))
		estimated := k > 1
		// share splits total evenly across the K tasks, giving the remainder to
		// the earliest tasks so Σ share == total exactly.
		share := func(total, i int64) int64 {
			q := total / k
			if i < total%k {
				q++
			}
			return q
		}
		for i, c := range g.tasks {
			a, ok := accs[c.TaskID]
			if !ok {
				a = &taskCost{TaskID: c.TaskID, Title: c.Title, Iteration: c.Iteration}
				accs[c.TaskID] = a
				order = append(order, c.TaskID)
			}
			ii := int64(i)
			a.Calls += int(share(wCalls, ii))
			a.Effective += share(wEff, ii)
			a.Output += share(wOut, ii)
			a.DurMs += share(wDur, ii)
			if estimated {
				a.Estimated = true
			}
		}
	}
	for ; ci < len(calls); ci++ {
		residual.Calls++
		residual.Effective += calls[ci].Effective
		residual.Output += calls[ci].Output
		residual.DurMs += calls[ci].DurMs
	}
	rows = make([]taskCost, 0, len(order))
	for _, id := range order {
		rows = append(rows, *accs[id])
	}
	return rows, residual
}

// buildTaskAttribution loads a run's task-completion events and build-stage
// calls, then windows the calls' effective cost onto the tasks (see
// attributeBuildCost). Returns JSON-ready rows plus the unattributed residual.
// A query error (e.g. no of_run_log, or an older of_llm_calls without
// created_at) propagates so the caller treats it as "no per-task data".
func buildTaskAttribution(db *sql.DB, runID string) ([]map[string]any, map[string]any, error) {
	compRows, err := db.Query(
		`SELECT ref, COALESCE(payload,''), COALESCE(created_at,'')
		   FROM of_run_log
		  WHERE run_id = ? AND kind = ? AND ref <> ''
		  ORDER BY id`, runID, runLogTaskKind)
	if err != nil {
		return nil, nil, err
	}
	var comps []taskCompletion
	for compRows.Next() {
		var ref, payload, ts string
		if scanErr := compRows.Scan(&ref, &payload, &ts); scanErr != nil {
			continue
		}
		c := taskCompletion{TaskID: ref, TS: ts, Title: ref}
		var p struct {
			Iteration int    `json:"iteration"`
			Title     string `json:"title"`
		}
		if json.Unmarshal([]byte(payload), &p) == nil {
			c.Iteration = p.Iteration
			if p.Title != "" {
				c.Title = p.Title
			}
		}
		comps = append(comps, c)
	}
	compRows.Close()
	if err := compRows.Err(); err != nil {
		return nil, nil, err
	}

	calls, err := loadBuildCalls(db, runID)
	if err != nil {
		return nil, nil, err
	}

	rows, residual := attributeBuildCost(comps, calls)
	taskRows := make([]map[string]any, 0, len(rows))
	for _, tc := range rows {
		taskRows = append(taskRows, map[string]any{
			"task_id":                tc.TaskID,
			"title":                  tc.Title,
			"iteration":              tc.Iteration,
			"calls":                  tc.Calls,
			"effective_input_tokens": tc.Effective,
			"output_tokens":          tc.Output,
			"wall_ms":                tc.DurMs,
			"estimated":              tc.Estimated,
		})
	}
	unattributed := map[string]any{
		"calls":                  residual.Calls,
		"effective_input_tokens": residual.Effective,
		"output_tokens":          residual.Output,
		"wall_ms":                residual.DurMs,
	}
	return taskRows, unattributed, nil
}
