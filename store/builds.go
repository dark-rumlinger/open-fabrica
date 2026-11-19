package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"openfabrica/db"
)

// ErrBuildNotFound is returned when a run ID does not exist in of_runs.
var ErrBuildNotFound = errors.New("build not found")

// BuildSummary holds summary metrics for a single engine run.
type BuildSummary struct {
	ID                string  `json:"id"`
	StartedAt         string  `json:"started_at"`
	CompletedAt       *string `json:"completed_at,omitempty"`
	Status            string  `json:"status"`
	FinalStage        *string `json:"final_stage,omitempty"`
	TotalInputTokens  int     `json:"total_input_tokens"`
	TotalOutputTokens int     `json:"total_output_tokens"`
	EngineVersion     string  `json:"engine_version,omitempty"`
	EngineBuiltAt     string  `json:"engine_built_at,omitempty"`
}

// BuildTimelineRow represents one chronological entry in a build timeline.
type BuildTimelineRow struct {
	Kind      string         `json:"kind"`
	TS        string         `json:"ts"`
	Stage     string         `json:"stage,omitempty"`
	Iteration *int           `json:"iteration,omitempty"`
	Data      map[string]any `json:"data"`
}

func iterPtr(n int) *int { return &n }

// LoadBuildSummary loads a single run summary with live token totals from of_llm_calls.
func LoadBuildSummary(reader *sql.DB, runID string) (*BuildSummary, error) {
	b := &BuildSummary{}
	var completedAt sql.NullString
	var stage string
	var engineVersion, engineBuiltAt sql.NullString
	err := reader.QueryRow(
		`SELECT id, started_at, completed_at, status, stage,
		 engine_version, engine_built_at
		 FROM of_runs WHERE id = ?`, runID,
	).Scan(&b.ID, &b.StartedAt, &completedAt, &b.Status, &stage,
		&engineVersion, &engineBuiltAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBuildNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := reader.QueryRow(
		`SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0)
		 FROM of_llm_calls WHERE run_id = ? AND source = 'engine'`, runID,
	).Scan(&b.TotalInputTokens, &b.TotalOutputTokens); err != nil {
		return nil, err
	}
	if completedAt.Valid {
		s := completedAt.String
		b.CompletedAt = &s
	}
	if stage != "" {
		b.FinalStage = &stage
	}
	b.EngineVersion = engineVersion.String
	b.EngineBuiltAt = engineBuiltAt.String
	return b, nil
}

// ListBuilds returns recent runs (up to limit) and the total run count.
func ListBuilds(pdb *db.ProjectDB, limit int) (builds []BuildSummary, total int, err error) {
	reader := pdb.Reader()
	_ = reader.QueryRow("SELECT COUNT(*) FROM of_runs").Scan(&total)

	rows, err := reader.Query(
		`SELECT r.id, r.started_at, r.completed_at, r.status, r.stage,
		 COALESCE(c.in_tokens, 0), COALESCE(c.out_tokens, 0)
		 FROM of_runs r
		 LEFT JOIN (
		   SELECT run_id,
		          SUM(input_tokens)  AS in_tokens,
		          SUM(output_tokens) AS out_tokens
		     FROM of_llm_calls WHERE source = 'engine' GROUP BY run_id
		 ) c ON c.run_id = r.id
		 ORDER BY r.started_at DESC, r.id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	builds = []BuildSummary{}
	for rows.Next() {
		var b BuildSummary
		var completedAt sql.NullString
		var stage string
		if rows.Scan(&b.ID, &b.StartedAt, &completedAt, &b.Status, &stage,
			&b.TotalInputTokens, &b.TotalOutputTokens) != nil {
			continue
		}
		if completedAt.Valid {
			s := completedAt.String
			b.CompletedAt = &s
		}
		if stage != "" {
			b.FinalStage = &stage
		}
		builds = append(builds, b)
	}
	return builds, total, rows.Err()
}

// LoadBuildTimeline merges of_run_log and of_llm_calls rows for runID chronologically.
func LoadBuildTimeline(pdb *db.ProjectDB, runID string) (*BuildSummary, []BuildTimelineRow, error) {
	reader := pdb.Reader()
	build, err := LoadBuildSummary(reader, runID)
	if err != nil {
		return nil, nil, err
	}

	var out []BuildTimelineRow

	if eRows, err := reader.Query(
		`SELECT kind, ref, payload, input_tokens, output_tokens, duration_ms, created_at
		 FROM of_run_log WHERE run_id = ? ORDER BY id ASC`, runID,
	); err == nil {
		for eRows.Next() {
			var kind, ref, payload, createdAt string
			var inT, outT int
			var dur int64
			if eRows.Scan(&kind, &ref, &payload, &inT, &outT, &dur, &createdAt) != nil {
				continue
			}
			var data map[string]any
			if payload != "" {
				_ = json.Unmarshal([]byte(payload), &data)
			}
			if data == nil {
				data = map[string]any{}
			}
			switch kind {
			case "stage":
				status, _ := data["status"].(string)
				if status == "started" {
					out = append(out, BuildTimelineRow{Kind: "stage_start", TS: createdAt, Stage: ref,
						Data: map[string]any{"status": status}})
				} else {
					data["input_tokens"] = inT
					data["output_tokens"] = outT
					data["duration_ms"] = dur
					out = append(out, BuildTimelineRow{Kind: "stage_complete", TS: createdAt, Stage: ref, Data: data})
				}
			default:
				summary, _ := data["text"].(string)
				out = append(out, BuildTimelineRow{Kind: "brain_event", TS: createdAt,
					Data: map[string]any{
						"event_type": kind,
						"summary":    summary,
						"details":    payload,
						"ref":        ref,
					}})
			}
		}
		_ = eRows.Err()
		eRows.Close()
	}

	if lRows, err := reader.Query(
		`SELECT id, stage, iteration, model, provider_type, cache_hit,
		 input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens,
		 duration_ms, COALESCE(response_stop_reason, ''), COALESCE(response_tool_calls, ''),
		 COALESCE(error_message, ''), created_at
		 FROM of_llm_calls WHERE run_id = ? AND source = 'engine' ORDER BY id ASC`, runID,
	); err == nil {
		for lRows.Next() {
			var id int64
			var stage, model, ptype, stopReason, toolCallsJSON, errMsg, createdAt string
			var iter, cacheHit, inT, outT, cacheR, cacheC, dur int
			if lRows.Scan(&id, &stage, &iter, &model, &ptype, &cacheHit, &inT, &outT, &cacheR, &cacheC, &dur, &stopReason, &toolCallsJSON, &errMsg, &createdAt) != nil {
				continue
			}
			out = append(out, BuildTimelineRow{
				Kind: "llm_call", TS: createdAt, Stage: stage, Iteration: iterPtr(iter),
				Data: map[string]any{
					"log_id":                      id,
					"model":                       model,
					"provider_type":               ptype,
					"replay_cache_hit":            cacheHit == 1,
					"input_tokens":                inT,
					"output_tokens":               outT,
					"cache_read_input_tokens":     cacheR,
					"cache_creation_input_tokens": cacheC,
					"duration_ms":                 dur,
					"stop_reason":                 stopReason,
					"error_message":               errMsg,
				},
			})
			if toolCallsJSON != "" && toolCallsJSON != "null" {
				var tcs []map[string]any
				if json.Unmarshal([]byte(toolCallsJSON), &tcs) == nil {
					for _, tc := range tcs {
						out = append(out, BuildTimelineRow{
							Kind: "tool_call", TS: createdAt, Stage: stage, Iteration: iterPtr(iter),
							Data: tc,
						})
					}
				}
			}
		}
		_ = lRows.Err()
		lRows.Close()
	}

	kindOrder := map[string]int{
		"stage_start": 0, "llm_call": 1, "tool_call": 2, "brain_event": 3, "stage_complete": 4,
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TS != out[j].TS {
			return out[i].TS < out[j].TS
		}
		return kindOrder[out[i].Kind] < kindOrder[out[j].Kind]
	})
	return build, out, nil
}

// LoadLLMStats returns aggregate LLM usage and per-model breakdown for a project.
func LoadLLMStats(pdb *db.ProjectDB) (map[string]any, error) {
	reader := pdb.Reader()
	out := map[string]any{
		"total_calls":                 0,
		"total_errors":                0,
		"total_input_tokens":          0,
		"total_output_tokens":         0,
		"total_cache_read_tokens":     0,
		"total_cache_creation_tokens": 0,
		"total_billed_input_tokens":   0,
		"cache_hit_ratio":             0.0,
		"by_model":                    []map[string]any{},
	}

	var totalCalls, totalErrors, totalInput, totalOutput int64
	if err := reader.QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(CASE WHEN error_message IS NOT NULL AND error_message <> '' THEN 1 ELSE 0 END), 0)
		 FROM of_llm_calls`,
	).Scan(&totalCalls, &totalInput, &totalOutput, &totalErrors); err != nil {
		return nil, err
	}
	out["total_calls"] = totalCalls
	out["total_errors"] = totalErrors
	out["total_input_tokens"] = totalInput
	out["total_output_tokens"] = totalOutput

	var totalCacheRead, totalCacheCreate int64
	if rows, err := reader.Query(
		`SELECT model, COUNT(*) AS calls,
		        COALESCE(SUM(input_tokens), 0)  AS input_tokens,
		        COALESCE(SUM(output_tokens), 0) AS output_tokens,
		        COALESCE(SUM(cache_read_input_tokens), 0)     AS cache_read,
		        COALESCE(SUM(cache_creation_input_tokens), 0) AS cache_creation
		 FROM of_llm_calls
		 GROUP BY model
		 ORDER BY calls DESC`,
	); err == nil {
		defer rows.Close()
		byModel := []map[string]any{}
		for rows.Next() {
			var model string
			var calls, inT, outT, cacheR, cacheC int64
			if rows.Scan(&model, &calls, &inT, &outT, &cacheR, &cacheC) != nil {
				continue
			}
			totalCacheRead += cacheR
			totalCacheCreate += cacheC
			byModel = append(byModel, map[string]any{
				"model":                 model,
				"calls":                 calls,
				"input_tokens":          inT,
				"output_tokens":         outT,
				"cache_read_tokens":     cacheR,
				"cache_creation_tokens": cacheC,
			})
		}
		_ = rows.Err()
		out["by_model"] = byModel
	}

	billedInput := totalInput + totalCacheRead + totalCacheCreate
	out["total_cache_read_tokens"] = totalCacheRead
	out["total_cache_creation_tokens"] = totalCacheCreate
	out["total_billed_input_tokens"] = billedInput
	if billedInput > 0 {
		out["cache_hit_ratio"] = float64(totalCacheRead) / float64(billedInput)
	}
	return out, nil
}
