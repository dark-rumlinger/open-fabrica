package admin

import (
	"errors"
	"net/http"

	"openfabrica/store"
)

// ProjectTriggersHandler is the operator window onto of_triggers.
type ProjectTriggersHandler struct {
	deps *Deps
}

// List returns every trigger with its event_type, action type, and config.
func (h *ProjectTriggersHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListTriggers(projectDB))
}

// Toggle flips a trigger's enabled flag.
func (h *ProjectTriggersHandler) Toggle(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	triggerID, ok := requireIntParam(w, r, "triggerID", "invalid trigger ID")
	if !ok {
		return
	}
	isEnabled, err := store.ToggleTrigger(projectDB, triggerID)
	if errors.Is(err, store.ErrTriggerNotFound) {
		writeError(w, http.StatusNotFound, "trigger not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to toggle trigger")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": triggerID, "is_enabled": isEnabled})
}

// Delete removes a trigger.
func (h *ProjectTriggersHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	triggerID, ok := requireIntParam(w, r, "triggerID", "invalid trigger ID")
	if !ok {
		return
	}
	err := store.DeleteTrigger(projectDB, triggerID)
	if errors.Is(err, store.ErrTriggerNotFound) {
		writeError(w, http.StatusNotFound, "trigger not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete trigger")
		return
	}
	writeStatus(w, "deleted")
}
