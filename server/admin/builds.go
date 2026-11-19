package admin

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"openfabrica/providers/llm"
	"openfabrica/store"
)

// BuildsHandler bundles list / timeline / cost / export handlers.
type BuildsHandler struct {
	deps *Deps
}

// List returns every run for a project, newest first.
func (h *BuildsHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	const buildsLimit = 200
	out, total, err := store.ListBuilds(projectDB, buildsLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list builds")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"builds":    out,
		"total":     total,
		"truncated": total > len(out),
	})
}

// Timeline returns merged chronological rows for one run.
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
	build, rows, err := store.LoadBuildTimeline(projectDB, runID)
	if err != nil {
		writeError(w, http.StatusNotFound, "build not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"build": build,
		"rows":  rows,
	})
}

// LLMCallDetail returns the full request/response body of one LLM call.
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
	c, scanErr := store.LoadLLMCallByID(projectDB.Reader(), logID)
	if scanErr != nil {
		writeError(w, http.StatusNotFound, "llm call not found")
		return
	}
	out := c.Fields()
	chain := llm.CallChain{Source: c.Source, RunID: c.RunID, SessionID: c.SessionID}
	reqMsg, reqSys, reqTools := llm.ReplayRequestMessages(projectDB.Reader(), c.ID, chain, c.ReqMessages, c.ReqSystem, c.ReqTools)
	out["request_messages"] = reqMsg
	out["request_system"] = reqSys
	out["request_tools"] = reqTools
	writeJSON(w, http.StatusOK, out)
}

// Stats returns aggregate LLM usage for a project.
func (h *BuildsHandler) Stats(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	out, err := store.LoadLLMStats(projectDB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read llm stats")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
