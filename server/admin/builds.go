package admin

// Builds: unified admin view over the engine's run tables (of_runs +
// of_run_log + of_llm_calls). "build" in the API simply means a run.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	"openfabrica/providers/llm"
)

// BuildsHandler bundles list / timeline / cost / export handlers.
type BuildsHandler struct {
	deps *Deps
}

type buildSummary struct {
	ID                string  `json:"id"`
	StartedAt         string  `json:"started_at"`
	CompletedAt       *string `json:"completed_at,omitempty"`
	Status            string  `json:"status"`
	FinalStage        *string `json:"final_stage,omitempty"`
	TotalInputTokens  int     `json:"total_input_tokens"`
	TotalOutputTokens int     `json:"total_output_tokens"`
	// EngineVersion / EngineBuiltAt identify the binary that produced this run
	// (stamped at run creation). Empty for runs created by a dev build or before
	// this stamp existed. Surfaced so a post-mortem can tell which gates were live.
	EngineVersion string `json:"engine_version,omitempty"`
	EngineBuiltAt string `json:"engine_built_at,omitempty"`
}

func loadBuildSummary(db *sql.DB, runID string) (*buildSummary, error) {
	b := &buildSummary{}
	var completedAt sql.NullString
	var stage string
	var engineVersion, engineBuiltAt sql.NullString
	err := db.QueryRow(
		`SELECT id, started_at, completed_at, status, stage,
		 engine_version, engine_built_at
		 FROM of_runs WHERE id = ?`, runID,
	).Scan(&b.ID, &b.StartedAt, &completedAt, &b.Status, &stage,
		&engineVersion, &engineBuiltAt)
	if err != nil {
		return nil, err
	}
	// Token totals are read live from of_llm_calls (the source of truth the cost
	// panel and daily budget also use), not the of_runs.total_* counters, which
	// lag a running build (bumped only at stage completion) and never include
	// the title stage - summing of_llm_calls keeps every surface consistent.
	if err := db.QueryRow(
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

// List returns every run for a project, newest first.
// GET /admin/api/projects/{projectID}/builds
func (h *BuildsHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	const buildsLimit = 200
	var total int
	if err := projectDB.Reader().QueryRow("SELECT COUNT(*) FROM of_runs").Scan(&total); err != nil {
		h.deps.Logger.Debug("builds: failed to count runs", "error", err)
	}
	// Token totals per run are summed from of_llm_calls (source of truth), not
	// the of_runs.total_* counters, so the pills match the cost panel and include
	// the title stage. See loadBuildSummary for the rationale.
	rows, err := projectDB.Reader().Query(
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
		buildsLimit,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list builds")
		return
	}
	out := []buildSummary{}
	out = append(out, collectRows(rows, func(rows *sql.Rows) (buildSummary, error) {
		var b buildSummary
		var completedAt sql.NullString
		var stage string
		if err := rows.Scan(&b.ID, &b.StartedAt, &completedAt, &b.Status, &stage,
			&b.TotalInputTokens, &b.TotalOutputTokens); err != nil {
			return b, err
		}
		if completedAt.Valid {
			s := completedAt.String
			b.CompletedAt = &s
		}
		if stage != "" {
			b.FinalStage = &stage
		}
		return b, nil
	})...)
	writeJSON(w, http.StatusOK, map[string]any{
		"builds":    out,
		"total":     total,
		"truncated": total > len(out),
	})
}

type timelineRow struct {
	Kind  string `json:"kind"` // stage_start / stage_complete / llm_call / tool_call / brain_event
	TS    string `json:"ts"`
	Stage string `json:"stage,omitempty"`
	// Iteration is a pointer so iteration=0 serialises as 0, not omitted.
	Iteration *int           `json:"iteration,omitempty"`
	Data      map[string]any `json:"data"`
}

func iterPtr(n int) *int { return &n }

// Timeline returns merged chronological rows for one run.
// GET /admin/api/projects/{projectID}/builds/{buildID}/timeline
func (h *BuildsHandler) Timeline(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	runID := chi.URLParam(r, "buildID")
	if runID == "" {
		writeError(w, http.StatusBadRequest, "missing build ID")
		return
	}

	build, err := loadBuildSummary(projectDB.Reader(), runID)
	if err != nil {
		writeError(w, http.StatusNotFound, "build not found")
		return
	}

	var rows []timelineRow

	// Run-log rows: stage transitions render as stage_start/stage_complete;
	// everything else (message/task/issue/session) renders as brain_event so
	// the existing timeline UI shows it without changes.
	eRows, err := projectDB.Reader().Query(
		`SELECT kind, ref, payload, input_tokens, output_tokens, duration_ms, created_at
		 FROM of_run_log WHERE run_id = ? ORDER BY id ASC`, runID)
	if err != nil {
		h.deps.Logger.Error("builds timeline: query run_log failed", "run_id", runID, "error", err)
	}
	if eRows != nil {
		for eRows.Next() {
			var kind, ref, payload, createdAt string
			var inT, outT int
			var dur int64
			if err := eRows.Scan(&kind, &ref, &payload, &inT, &outT, &dur, &createdAt); err != nil {
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
					rows = append(rows, timelineRow{Kind: "stage_start", TS: createdAt, Stage: ref,
						Data: map[string]any{"status": status}})
				} else {
					data["input_tokens"] = inT
					data["output_tokens"] = outT
					data["duration_ms"] = dur
					rows = append(rows, timelineRow{Kind: "stage_complete", TS: createdAt, Stage: ref, Data: data})
				}
			default:
				summary, _ := data["text"].(string)
				rows = append(rows, timelineRow{Kind: "brain_event", TS: createdAt,
					Data: map[string]any{
						"event_type": kind,
						"summary":    summary,
						"details":    payload,
						"ref":        ref,
					}})
			}
		}
		eRows.Close()
		if err := eRows.Err(); err != nil {
			h.deps.Logger.Error("builds timeline: iterate run_log failed", "run_id", runID, "error", err)
		}
	}

	// LLM call rows (also one tool_call row per tool call in the response).
	lRows, err := projectDB.Reader().Query(
		`SELECT id, stage, iteration, model, provider_type, cache_hit,
		 input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens,
		 duration_ms, COALESCE(response_stop_reason, ''), COALESCE(response_tool_calls, ''),
		 COALESCE(error_message, ''), created_at
		 FROM of_llm_calls WHERE run_id = ? AND source = 'engine' ORDER BY id ASC`, runID)
	if err != nil {
		h.deps.Logger.Error("builds timeline: query llm_calls failed", "run_id", runID, "error", err)
	}
	if lRows != nil {
		for lRows.Next() {
			var id int64
			var stage, model, ptype, stopReason, toolCallsJSON, errMsg, createdAt string
			var iter, cacheHit, inT, outT, cacheR, cacheC, dur int
			if err := lRows.Scan(&id, &stage, &iter, &model, &ptype, &cacheHit, &inT, &outT, &cacheR, &cacheC, &dur, &stopReason, &toolCallsJSON, &errMsg, &createdAt); err != nil {
				continue
			}
			rows = append(rows, timelineRow{
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
				if err := json.Unmarshal([]byte(toolCallsJSON), &tcs); err == nil {
					for _, tc := range tcs {
						rows = append(rows, timelineRow{
							Kind: "tool_call", TS: createdAt, Stage: stage, Iteration: iterPtr(iter),
							Data: tc,
						})
					}
				}
			}
		}
		lRows.Close()
		if err := lRows.Err(); err != nil {
			h.deps.Logger.Error("builds timeline: iterate llm_calls failed", "run_id", runID, "error", err)
		}
	}

	kindOrder := map[string]int{
		"stage_start": 0, "llm_call": 1, "tool_call": 2, "brain_event": 3, "stage_complete": 4,
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].TS != rows[j].TS {
			return rows[i].TS < rows[j].TS
		}
		return kindOrder[rows[i].Kind] < kindOrder[rows[j].Kind]
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"build": build,
		"rows":  rows,
	})
}

// LLMCallDetail returns the full request/response body of one LLM call.
// GET /admin/api/projects/{projectID}/builds/llm/{logID}
func (h *BuildsHandler) LLMCallDetail(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	logID, err := strconv.ParseInt(chi.URLParam(r, "logID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid log ID")
		return
	}
	row := projectDB.Reader().QueryRow(
		`SELECT `+llmCallColumns+` FROM of_llm_calls WHERE id = ?`, logID,
	)
	c, scanErr := scanLLMCall(row)
	if scanErr != nil {
		writeError(w, http.StatusNotFound, "llm call not found")
		return
	}
	out := c.fields()
	// A row may hold only the messages it appended; replay the session chain so the caller
	// always sees the full request the model was actually sent.
	chain := llm.CallChain{Source: c.Source, RunID: c.RunID, SessionID: c.SessionID}
	reqMsg, reqSys, reqTools := llm.ReplayRequestMessages(projectDB.Reader(), c.ID, chain, c.ReqMessages, c.ReqSystem, c.ReqTools)
	out["request_messages"] = reqMsg
	out["request_system"] = reqSys
	out["request_tools"] = reqTools
	writeJSON(w, http.StatusOK, out)
}

// Stats returns aggregate LLM usage for a project.
// GET /admin/api/projects/{projectID}/llm/stats
func (h *BuildsHandler) Stats(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

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
	if err := projectDB.Reader().QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(CASE WHEN error_message IS NOT NULL AND error_message <> '' THEN 1 ELSE 0 END), 0)
		 FROM of_llm_calls`,
	).Scan(&totalCalls, &totalInput, &totalOutput, &totalErrors); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read llm stats")
		return
	}
	out["total_calls"] = totalCalls
	out["total_errors"] = totalErrors
	out["total_input_tokens"] = totalInput
	out["total_output_tokens"] = totalOutput

	rows, err := projectDB.Reader().Query(
		`SELECT model, COUNT(*) AS calls,
		        COALESCE(SUM(input_tokens), 0)  AS input_tokens,
		        COALESCE(SUM(output_tokens), 0) AS output_tokens,
		        COALESCE(SUM(cache_read_input_tokens), 0)     AS cache_read,
		        COALESCE(SUM(cache_creation_input_tokens), 0) AS cache_creation
		 FROM of_llm_calls
		 GROUP BY model
		 ORDER BY calls DESC`,
	)
	var totalCacheRead, totalCacheCreate int64
	if err == nil {
		defer rows.Close()
		byModel := []map[string]any{}
		for rows.Next() {
			var model string
			var calls, inT, outT, cacheR, cacheC int64
			if scanErr := rows.Scan(&model, &calls, &inT, &outT, &cacheR, &cacheC); scanErr != nil {
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
		if err := rows.Err(); err != nil {
			h.deps.Logger.Debug("builds: failed to iterate llm stats", "error", err)
		}
		out["by_model"] = byModel
	}

	billedInput := totalInput + totalCacheRead + totalCacheCreate
	out["total_cache_read_tokens"] = totalCacheRead
	out["total_cache_creation_tokens"] = totalCacheCreate
	out["total_billed_input_tokens"] = billedInput
	if billedInput > 0 {
		out["cache_hit_ratio"] = float64(totalCacheRead) / float64(billedInput)
	}

	writeJSON(w, http.StatusOK, out)
}
