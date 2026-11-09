package admin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ProjectPagesHandler handles page listing for project detail views. All storage
// goes through of_files (kind=page); titles/templates/status live in the
// metadata JSON column.
type ProjectPagesHandler struct {
	deps *Deps
}

type projectPage struct {
	ID           int       `json:"id"`
	ProjectID    int       `json:"project_id"`
	Path         string    `json:"path"`
	Title        *string   `json:"title"`
	Content      *string   `json:"content,omitempty"`
	Template     *string   `json:"template"`
	Status       string    `json:"status"`
	Metadata     string    `json:"metadata"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	VersionCount int       `json:"version_count"`
}

// List returns all pages for a project (without content to keep response small).
func (h *ProjectPagesHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	rows, err := projectDB.Query(
		`SELECT id, path,
		        json_extract(metadata, '$.title'),
		        json_extract(metadata, '$.template'),
		        status, metadata, created_at, updated_at,
		        (SELECT COUNT(*) FROM of_file_history WHERE kind = 'page' AND path = of_files.path) AS version_count
		 FROM of_files WHERE kind = 'page' AND is_deleted = 0 ORDER BY path ASC`,
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []projectPage{})
		return
	}
	pages := collectRows(rows, func(rows *sql.Rows) (projectPage, error) {
		var p projectPage
		if err := rows.Scan(&p.ID, &p.Path, &p.Title, &p.Template, &p.Status, &p.Metadata, &p.CreatedAt, &p.UpdatedAt, &p.VersionCount); err != nil {
			return p, err
		}
		p.ProjectID = projectID
		return p, nil
	})
	if pages == nil {
		pages = []projectPage{}
	}

	writeJSON(w, http.StatusOK, pages)
}

// Get returns a single page with its content.
func (h *ProjectPagesHandler) Get(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	pageID, ok := requireIntParam(w, r, "pageID", "invalid page ID")
	if !ok {
		return
	}

	var p projectPage
	err := projectDB.QueryRow(
		`SELECT id, path,
		        json_extract(metadata, '$.title'),
		        body,
		        json_extract(metadata, '$.template'),
		        status, metadata, created_at, updated_at
		 FROM of_files WHERE kind = 'page' AND id = ? AND is_deleted = 0`,
		pageID,
	).Scan(&p.ID, &p.Path, &p.Title, &p.Content, &p.Template, &p.Status, &p.Metadata, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load page")
		return
	}

	p.ProjectID = projectID
	writeJSON(w, http.StatusOK, p)
}

type createPageRequest struct {
	Path    string  `json:"path"`
	Title   *string `json:"title"`
	Content *string `json:"content"`
}

// Create inserts a new page.
func (h *ProjectPagesHandler) Create(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	var req createPageRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if !strings.HasPrefix(req.Path, "/") {
		req.Path = "/" + req.Path
	}
	req.Path = strings.TrimRight(req.Path, "/")
	if strings.Contains(req.Path, "..") {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	// Reject if a page already exists at this path.
	var existing int
	_ = projectDB.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND path = ? AND is_deleted = 0", req.Path).Scan(&existing)
	if existing > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("page %q already exists", req.Path))
		return
	}

	content := ""
	if req.Content != nil {
		content = *req.Content
	}
	meta := map[string]any{}
	if req.Title != nil {
		meta["title"] = *req.Title
	}
	metaJSON, _ := json.Marshal(meta)

	res, err := projectDB.ExecWrite(
		`INSERT INTO of_files (kind, path, body, content_type, metadata, status)
		 VALUES ('page', ?, ?, 'text/html; charset=utf-8', ?, 'published')`,
		req.Path, content, string(metaJSON),
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create page")
		return
	}

	id, _ := res.LastInsertId()
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "path": req.Path})
}

type updatePageRequest struct {
	Title   *string `json:"title"`
	Content *string `json:"content"`
	Status  *string `json:"status"`
}

// Update modifies a page's content, title, or status.
func (h *ProjectPagesHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	pageID, ok := requireIntParam(w, r, "pageID", "invalid page ID")
	if !ok {
		return
	}

	var req updatePageRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	var setClauses []string
	var values []any

	if req.Title != nil {
		// Merge the title into metadata JSON so other fields stay intact.
		setClauses = append(setClauses, "metadata = json_set(COALESCE(metadata, '{}'), '$.title', ?)")
		values = append(values, *req.Title)
	}
	if req.Content != nil {
		setClauses = append(setClauses, "body = ?")
		values = append(values, *req.Content)
	}
	if req.Status != nil {
		setClauses = append(setClauses, "status = ?")
		values = append(values, *req.Status)
	}

	if len(setClauses) == 0 {
		writeError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")
	values = append(values, pageID)

	query := "UPDATE of_files SET " + strings.Join(setClauses, ", ") + " WHERE kind = 'page' AND id = ? AND is_deleted = 0"
	res, err := projectDB.ExecWrite(query, values...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update page")
		return
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		writeError(w, http.StatusNotFound, "page not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"updated": pageID})
}

// Delete soft-deletes a page.
func (h *ProjectPagesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	pageID, ok := requireIntParam(w, r, "pageID", "invalid page ID")
	if !ok {
		return
	}

	n, err := softDeleteArtifact(projectDB, "page", pageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete page")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "page not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": pageID})
}

// ListVersions returns version history for a page.
func (h *ProjectPagesHandler) ListVersions(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	pageID, ok := requireIntParam(w, r, "pageID", "invalid page ID")
	if !ok {
		return
	}

	respondArtifactVersions(w, projectDB, "page", pageID)
}

// Restore reverts a page to a previous snapshotted body.
func (h *ProjectPagesHandler) Restore(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	pageID, ok := requireIntParam(w, r, "pageID", "invalid page ID")
	if !ok {
		return
	}
	respondArtifactRestore(w, r, projectDB, "page", pageID)
}
