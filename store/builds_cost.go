package store

import "database/sql"

// RunStageTokenAgg holds aggregated token/byte metrics for one (stage, provider, model) group in a run.
type RunStageTokenAgg struct {
	Stage        string
	ProviderType string
	ProviderName string
	Model        string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheCreate  int64
	SumPrefix    int64
	SumTotal     int64
	MinTotal     int64
}

// LoadRunStageTokenAggs aggregates of_llm_calls for one engine run by stage and model.
func LoadRunStageTokenAggs(reader *sql.DB, runID string) ([]RunStageTokenAgg, error) {
	rows, err := reader.Query(
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

	var out []RunStageTokenAgg
	for rows.Next() {
		var r RunStageTokenAgg
		if err := rows.Scan(
			&r.Stage, &r.ProviderType, &r.ProviderName, &r.Model,
			&r.Calls, &r.InputTokens, &r.OutputTokens, &r.CacheRead, &r.CacheCreate,
			&r.SumPrefix, &r.SumTotal, &r.MinTotal,
		); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunBuildCallRow is one build-stage LLM call row in wall-clock order.
type RunBuildCallRow struct {
	ProviderType string
	ProviderName string
	Model        string
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheCreate  int64
	DurationMs   int64
	CreatedAt    string
}

// LoadRunBuildCalls returns the run's build-stage LLM calls in wall-clock order.
func LoadRunBuildCalls(reader *sql.DB, runID string) ([]RunBuildCallRow, error) {
	rows, err := reader.Query(
		`SELECT COALESCE(provider_type,''), COALESCE(provider_name,''), COALESCE(model,''),
		        input_tokens, output_tokens,
		        cache_read_input_tokens, cache_creation_input_tokens,
		        duration_ms, COALESCE(created_at,'')
		   FROM of_llm_calls
		  WHERE run_id = ? AND source = 'engine' AND stage = 'build'
		  ORDER BY created_at, id`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RunBuildCallRow
	for rows.Next() {
		var r RunBuildCallRow
		if err := rows.Scan(
			&r.ProviderType, &r.ProviderName, &r.Model,
			&r.InputTokens, &r.OutputTokens, &r.CacheRead, &r.CacheCreate,
			&r.DurationMs, &r.CreatedAt,
		); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunTaskCompletionRow is one task completion event from of_run_log.
type RunTaskCompletionRow struct {
	Ref       string
	Payload   string
	CreatedAt string
}

// LoadRunTaskCompletions returns task-completion events for runID in chronological order.
func LoadRunTaskCompletions(reader *sql.DB, runID, kind string) ([]RunTaskCompletionRow, error) {
	rows, err := reader.Query(
		`SELECT ref, COALESCE(payload,''), COALESCE(created_at,'')
		   FROM of_run_log
		  WHERE run_id = ? AND kind = ? AND ref <> ''
		  ORDER BY id`,
		runID, kind,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RunTaskCompletionRow
	for rows.Next() {
		var r RunTaskCompletionRow
		if err := rows.Scan(&r.Ref, &r.Payload, &r.CreatedAt); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
