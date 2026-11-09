package admin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	"openfabrica/providers/llm"
)

// request_system_ref are kept. v2 adds engine_version / engine_built_at to the
// `build` block (the binary that produced the run).
// GET /admin/api/projects/{projectID}/builds/{buildID}/export
func (h *BuildsHandler) Export(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	runID := chi.URLParam(r, "buildID")
	if runID == "" {
		writeError(w, http.StatusBadRequest, "missing build ID")
		return
	}
	payload, err := h.exportPayload(projectDB, runID)
	if err != nil {
		writeError(w, http.StatusNotFound, "build not found")
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="build-%s.json"`, runID))
	writeJSON(w, http.StatusOK, payload)
}

// exportPayload assembles the llm-dedup-v2 export payload for one run: the build
// summary, per-stage token breakdown, deduped message/system pools the llm_calls
// reference by index, the brain run-log events, and the chat history. Shared by
// the JWT-gated Export endpoint and the loopback triage raw view.
func (h *BuildsHandler) exportPayload(projectDB *db.ProjectDB, runID string) (map[string]any, error) {
	build, err := loadBuildSummary(projectDB.Reader(), runID)
	if err != nil {
		return nil, err
	}

	// Stage rows derived from the run log.
	stages := []map[string]any{}
	sRows, err := projectDB.Reader().Query(
		`SELECT ref, payload, input_tokens, output_tokens, duration_ms, created_at
		 FROM of_run_log WHERE run_id = ? AND kind = 'stage' ORDER BY id ASC`, runID)
	if err != nil {
		h.deps.Logger.Error("build export: query run_log failed", "run_id", runID, "error", err)
	}
	if sRows != nil {
		stages = append(stages, collectRows(sRows, func(sRows *sql.Rows) (map[string]any, error) {
			var ref, payload, createdAt string
			var inT, outT int
			var dur int64
			if err := sRows.Scan(&ref, &payload, &inT, &outT, &dur, &createdAt); err != nil {
				return nil, err
			}
			var data map[string]any
			_ = json.Unmarshal([]byte(payload), &data)
			status, _ := data["status"].(string)
			errMsg, _ := data["error"].(string)
			return map[string]any{
				"stage":         ref,
				"status":        status,
				"input_tokens":  inT,
				"output_tokens": outT,
				"duration_ms":   dur,
				"error_message": errMsg,
				"at":            createdAt,
			}, nil
		})...)
	}

	// Reference-based dedup pools.
	msgPool := []json.RawMessage{}
	msgIndex := map[string]int{}
	internMsg := func(raw json.RawMessage) int {
		key := string(raw)
		if i, ok := msgIndex[key]; ok {
			return i
		}
		i := len(msgPool)
		msgPool = append(msgPool, raw)
		msgIndex[key] = i
		return i
	}
	sysPool := []string{}
	sysIndex := map[string]int{}
	internSys := func(s string) int {
		if i, ok := sysIndex[s]; ok {
			return i
		}
		i := len(sysPool)
		sysPool = append(sysPool, s)
		sysIndex[s] = i
		return i
	}

	llmCalls := []map[string]any{}
	// Folds each session's delta chain back into full requests as the rows stream past.
	var replay llm.RequestReplayer
	lRows, err := projectDB.Reader().Query(
		`SELECT `+llmCallColumns+` FROM of_llm_calls WHERE run_id = ? AND source = 'engine'
		 ORDER BY id ASC`, runID)
	if err != nil {
		h.deps.Logger.Error("build export: query llm_calls failed", "run_id", runID, "error", err)
	}
	if lRows != nil {
		for lRows.Next() {
			c, err := scanLLMCall(lRows)
			if err != nil {
				continue
			}
			call := c.fields()
			// A row may store only the messages it appended since an earlier row in the same
			// session. The query walks a run in id order, which is the order the replayer
			// needs, so folding here reconstructs every row in one pass. Self-contained rows
			// (the shape written before delta storage, and every session's first row) fold to
			// themselves.
			chain := llm.CallChain{Source: c.Source, RunID: runID, SessionID: c.SessionID}
			msgs, reqSys, reqTools := replay.Next(chain, c.ReqMessages, c.ReqSystem, c.ReqTools)
			call["request_tools"] = reqTools

			refs := make([]int, 0, len(msgs))
			for _, m := range msgs {
				refs = append(refs, internMsg(m))
			}
			call["request_message_refs"] = refs
			if reqSys != "" {
				call["request_system_ref"] = internSys(reqSys)
			}
			llmCalls = append(llmCalls, call)
		}
		lRows.Close()
		if err := lRows.Err(); err != nil {
			h.deps.Logger.Error("build export: iterate llm_calls failed", "run_id", runID, "error", err)
		}
	}

	// Non-stage run-log events (messages, tasks, issues, sessions).
	brainEvents := []map[string]any{}
	bRows, err := projectDB.Reader().Query(
		`SELECT kind, ref, payload, created_at
		 FROM of_run_log WHERE run_id = ? AND kind != 'stage' ORDER BY id ASC`, runID)
	if err != nil {
		h.deps.Logger.Error("build export: query run_log events failed", "run_id", runID, "error", err)
	}
	if bRows != nil {
		brainEvents = append(brainEvents, collectRows(bRows, func(bRows *sql.Rows) (map[string]any, error) {
			var kind, ref, payload, createdAt string
			if err := bRows.Scan(&kind, &ref, &payload, &createdAt); err != nil {
				return nil, err
			}
			var data map[string]any
			_ = json.Unmarshal([]byte(payload), &data)
			summary, _ := data["text"].(string)
			return map[string]any{
				"event_type": kind,
				"summary":    summary,
				"details":    payload,
				"ref":        ref,
				"created_at": createdAt,
			}, nil
		})...)
	}

	// Chat messages within the run window.
	chatMessages, err := runChatMessages(projectDB.Reader(), build.StartedAt, build.CompletedAt)
	if err != nil {
		h.deps.Logger.Error("build export: query chat history failed", "run_id", runID, "error", err)
	}

	tokenSummary, err := buildTokenSummary(projectDB.Reader(), runID)
	if err != nil {
		h.deps.Logger.Error("build export: token summary failed", "run_id", runID, "error", err)
		tokenSummary = nil
	}

	return map[string]any{
		"schema_version": "llm-dedup-v2",
		"build":          build,
		"token_summary":  tokenSummary,
		"stages":         stages,
		"messages_pool":  msgPool,
		"systems_pool":   sysPool,
		"llm_calls":      llmCalls,
		"brain_events":   brainEvents,
		"chat_messages":  chatMessages,
		"exported_at":    time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// runChatMessages loads the brain chat transcript within a run's
// [startedAt, completedAt] window (empty/nil bound = unbounded on that side).
//
// Both bounds AND the stored created_at are wrapped in datetime() on purpose:
// the bounds read back RFC3339 (...T...Z) while created_at is written
// space-separated, and a raw string compare ('T' > ' ') silently empties the
// exported transcript. datetime() compares them as instants. See
// chat.LoadTimeline and TestRunChatMessages_SpaceFormatWindow.
func runChatMessages(rdb *sql.DB, startedAt string, completedAt *string) ([]map[string]any, error) {
	chatMessages := []map[string]any{}
	chatQuery := `SELECT role, COALESCE(content, ''), COALESCE(tool_calls, ''),
		 COALESCE(tool_call_id, ''), created_at
		 FROM of_chat_messages WHERE session_id = 'brain'`
	chatArgs := []any{}
	if startedAt != "" {
		chatQuery += ` AND datetime(created_at) >= datetime(?)`
		chatArgs = append(chatArgs, startedAt)
	}
	if completedAt != nil && *completedAt != "" {
		chatQuery += ` AND datetime(created_at) <= datetime(?)`
		chatArgs = append(chatArgs, *completedAt)
	}
	chatQuery += ` ORDER BY created_at ASC, id ASC`
	cRows, err := rdb.Query(chatQuery, chatArgs...)
	if err != nil {
		return chatMessages, err
	}
	chatMessages = append(chatMessages, collectRows(cRows, func(cRows *sql.Rows) (map[string]any, error) {
		var role, content, toolCalls, toolCallID, createdAt string
		if err := cRows.Scan(&role, &content, &toolCalls, &toolCallID, &createdAt); err != nil {
			return nil, err
		}
		return map[string]any{
			"role":         role,
			"content":      content,
			"tool_calls":   toolCalls,
			"tool_call_id": toolCallID,
			"created_at":   createdAt,
		}, nil
	})...)
	return chatMessages, nil
}
