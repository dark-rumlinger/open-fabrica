package admin

import (
	"errors"
	"net/http"

	"openfabrica/store"
)

// ProjectMemoryHandler exposes a project's of_memory store for operator viewing and editing.
type ProjectMemoryHandler struct {
	deps *Deps
}

// List returns all memory entries for a project, newest first.
func (h *ProjectMemoryHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListMemories(projectDB))
}

type memoryRequest struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Category string `json:"category"`
}

// Create stores a new memory entry (upsert by key).
func (h *ProjectMemoryHandler) Create(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	var req memoryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Key == "" || req.Value == "" {
		writeError(w, http.StatusBadRequest, "key and value are required")
		return
	}
	if req.Category == "" {
		req.Category = "general"
	}
	if err := store.UpsertMemory(projectDB, req.Key, req.Value, req.Category); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store memory")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": req.Key})
}

// Update edits an entry's value/category by id.
func (h *ProjectMemoryHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	id, ok := requireIntParam(w, r, "memoryID", "invalid memory ID")
	if !ok {
		return
	}
	var req memoryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}
	category := req.Category
	if category == "" {
		category = "general"
	}
	err := store.UpdateMemory(projectDB, id, req.Value, category)
	if errors.Is(err, store.ErrMemoryNotFound) {
		writeError(w, http.StatusNotFound, "memory not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update memory")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": id})
}

// Delete removes a memory entry by id.
func (h *ProjectMemoryHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	id, ok := requireIntParam(w, r, "memoryID", "invalid memory ID")
	if !ok {
		return
	}
	err := store.DeleteMemory(projectDB, id)
	if errors.Is(err, store.ErrMemoryNotFound) {
		writeError(w, http.StatusNotFound, "memory not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete memory")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
