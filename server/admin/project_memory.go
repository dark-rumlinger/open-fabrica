package admin

import (
	"database/sql"
	"net/http"
	"time"
)

// ProjectMemoryHandler exposes a project's of_memory store (the engine's durable
// per-project facts/preferences) for operator viewing and editing. The engine
// reads/writes the same table via manage_memory; this is the human window onto it.
type ProjectMemoryHandler struct {
	deps *Deps
}

type memoryEntry struct {
	ID        int       `json:"id"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Category  string    `json:"category"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// List returns all memory entries for a project, newest first.
func (h *ProjectMemoryHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	rows, err := projectDB.Query(
		"SELECT id, key, value, category, created_at, updated_at FROM of_memory ORDER BY updated_at DESC",
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []memoryEntry{})
		return
	}
	entries := collectRows(rows, func(rows *sql.Rows) (memoryEntry, error) {
		var e memoryEntry
		if err := rows.Scan(&e.ID, &e.Key, &e.Value, &e.Category, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return e, err
		}
		return e, nil
	})
	if entries == nil {
		entries = []memoryEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
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
	_, err := projectDB.ExecWrite(
		`INSERT INTO of_memory (key, value, category, updated_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, category = excluded.category, updated_at = CURRENT_TIMESTAMP`,
		req.Key, req.Value, req.Category,
	)
	if err != nil {
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
	res, err := projectDB.ExecWrite(
		"UPDATE of_memory SET value = ?, category = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		req.Value, category, id,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update memory")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "memory not found")
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
	res, err := projectDB.ExecWrite("DELETE FROM of_memory WHERE id = ?", id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete memory")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "memory not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
