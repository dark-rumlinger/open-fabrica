package admin

import (
	"net/http"

	"openfabrica/db/models"
	"openfabrica/engine"
	"openfabrica/events"
)

// EngineHandler handles engine control endpoints.
type EngineHandler struct {
	deps *Deps
}

// Start starts the engine worker for a project.
func (h *EngineHandler) Start(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	h.deps.recordReplyLanguage(r, projectID)
	if err := h.deps.Engine.StartProject(projectID); err != nil {
		h.deps.Logger.Error("failed to start engine", "project_id", projectID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to start engine")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": projectID,
		"status":     "started",
	})
}

// Stop stops the engine worker for a project.
func (h *EngineHandler) Stop(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	if err := h.deps.Engine.StopProject(projectID); err != nil {
		h.deps.Logger.Error("failed to stop engine", "project_id", projectID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to stop engine")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": projectID,
		"status":     "stopped",
	})
}

// Interrupt asks a project's pipeline to stop at the next safe boundary. Unlike
// Stop it leaves the worker running and the run resumable: the build parks
// paused where it got to, and setting the mode back to building picks it up.
func (h *EngineHandler) Interrupt(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	if err := h.deps.Engine.RequestStop(projectID); err != nil {
		// No worker means nothing to interrupt, which is the caller's answer
		// rather than a server fault: the build already finished or was stopped.
		h.deps.Logger.Info("interrupt requested with no worker running", "project_id", projectID, "error", err)
		writeError(w, http.StatusConflict, "nothing is running for this project")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": projectID,
		"status":     "stopping",
	})
}

// announcePhase publishes a project's current phase for the admin UI and the
// bot. Workers announce their own transitions; this covers the ones made while
// no worker is running.
func (h *EngineHandler) announcePhase(projectID int) {
	if h.deps.Bus == nil {
		return
	}
	project, err := models.GetProjectByID(h.deps.DB.DB, projectID)
	if err != nil {
		return
	}
	h.deps.Bus.Publish(events.NewEvent(events.EventProjectPhaseChanged, projectID, map[string]any{
		"project_id": projectID,
		"mode":       project.Mode,
		"phase":      h.deps.viewProject(project).Phase,
	}))
}

// Status reports whether a worker exists for a project and the project's phase.
func (h *EngineHandler) Status(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	project, err := models.GetProjectByID(h.deps.DB.DB, projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": projectID,
		"running":    h.deps.Engine.IsRunning(projectID),
		"phase":      h.deps.viewProject(project).Phase,
	})
}

type modeChangeRequest struct {
	Mode string `json:"mode"`
}

// Mode sets a project's stored mode. projects.mode is the single
// source of truth for the owner-facing mode (manual pause is sticky there);
// the running worker is informed via CommandModeChange.
func (h *EngineHandler) Mode(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	var req modeChangeRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if !models.ValidMode(req.Mode) {
		writeError(w, http.StatusBadRequest, "invalid mode; must be building, live, or paused")
		return
	}

	h.deps.recordReplyLanguage(r, projectID)
	if err := models.UpdateProjectMode(h.deps.DB.DB, projectID, req.Mode); err != nil {
		h.deps.Logger.Error("failed to update project mode", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update mode")
		return
	}

	cmd := engine.Command{
		Type:      engine.CommandModeChange,
		ProjectID: projectID,
		Payload: map[string]any{
			"mode": req.Mode,
		},
	}
	if err := h.deps.Engine.Send(projectID, cmd); err != nil {
		// No worker to tell. A project that is building or live needs one to
		// carry on, and a new worker starts from the stored mode; a paused one
		// stays without. Either way the API announces the change, since no
		// worker will.
		if req.Mode != models.ModePaused {
			if err := h.deps.Engine.StartProject(projectID); err != nil {
				h.deps.Logger.Error("failed to start engine after mode change", "project_id", projectID, "error", err)
			}
		}
		h.announcePhase(projectID)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": projectID,
		"mode":       req.Mode,
	})
}
