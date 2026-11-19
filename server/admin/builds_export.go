package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	"openfabrica/providers/llm"
	"openfabrica/store"
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
	build, err := store.LoadBuildSummary(projectDB.Reader(), runID)
	if err != nil {
		return nil, err
	}

	// Stage rows derived from the run log.
	stages, err := store.LoadRunExportStages(projectDB.Reader(), runID)
	if err != nil {
		h.deps.Logger.Error("build export: query run_log failed", "run_id", runID, "error", err)
		stages = []map[string]any{}
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
	rawCalls, err := store.LoadRunEngineLLMCalls(projectDB.Reader(), runID)
	if err != nil {
		h.deps.Logger.Error("build export: query llm_calls failed", "run_id", runID, "error", err)
	}
	for _, c := range rawCalls {
		call := c.Fields()
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

	// Non-stage run-log events (messages, tasks, issues, sessions).
	brainEvents, err := store.LoadRunExportEvents(projectDB.Reader(), runID)
	if err != nil {
		h.deps.Logger.Error("build export: query run_log events failed", "run_id", runID, "error", err)
		brainEvents = []map[string]any{}
	}

	// Chat messages within the run window.
	chatMessages, err := store.LoadRunChatMessages(projectDB.Reader(), build.StartedAt, build.CompletedAt)
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
