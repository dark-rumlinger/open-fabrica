package admin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/internal/httpx"
	"openfabrica/probe"
	"openfabrica/store"
)

// ProjectFilesHandler handles all project files (uploads + engine-authored artifacts).
// Storage is of_files (kind='file'); history is of_file_history
// keyed on (kind, path). The list also carries the project's user-flow script
// (of_test.js), which lives under its own kind='test' so the public server never
// serves it and no page imports it: the owner sees and edits it here, and a save
// is validated exactly as manage_testing write_flows validates the builder's.
type ProjectFilesHandler struct {
	deps *Deps
}

type projectFile struct {
	ID           int       `json:"id"`
	ProjectID    int       `json:"project_id"`
	Filename     string    `json:"filename"`
	ContentType  *string   `json:"content_type"`
	Size         *int      `json:"size"`
	Description  *string   `json:"description"`
	CreatedAt    time.Time `json:"created_at"`
	VersionCount int       `json:"version_count"`
	// PlatformManaged marks files Open Fabrica owns and auto-emits when a feature
	// is enabled. The UI badges them and hides edit/delete/versions; the write
	// handlers refuse them server-side regardless.
	PlatformManaged bool `json:"platform_managed"`
	// Kind is "file", or "test" for the user-flow script: editable here (checked
	// before it is stored), never deleted or published.
	Kind string `json:"kind"`
}

// List returns all files for a project.
func (h *ProjectFilesHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	rows, err := projectDB.Query(
		`SELECT id, path, content_type, size,
		        COALESCE(json_extract(metadata, '$.description'), ''),
		        created_at,
		        (SELECT COUNT(*) FROM of_file_history h WHERE h.kind = of_files.kind AND h.path = of_files.path) AS version_count,
		        COALESCE(json_extract(metadata, '$.platform_managed'), 0),
		        kind
		 FROM of_files
		 WHERE is_deleted = 0 AND (kind = 'file' OR (kind = ? AND path = ?))
		 ORDER BY created_at DESC`,
		probe.FlowKind, probe.FlowPath,
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []projectFile{})
		return
	}
	files := collectRows(rows, func(rows *sql.Rows) (projectFile, error) {
		var f projectFile
		f.ProjectID = projectID
		var platformManaged int // json_extract yields 1/0 (or NULL -> 0 via COALESCE)
		if err := rows.Scan(&f.ID, &f.Filename, &f.ContentType, &f.Size, &f.Description, &f.CreatedAt, &f.VersionCount, &platformManaged, &f.Kind); err != nil {
			return f, err
		}
		f.PlatformManaged = platformManaged == 1
		return f, nil
	})

	if files == nil {
		files = []projectFile{}
	}

	writeJSON(w, http.StatusOK, files)
}

// textFileRequest is the JSON body for creating a text-based file (JS/CSS/HTML).
type textFileRequest struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

// Create handles both JSON (text file creation) and multipart (binary upload).
func (h *ProjectFilesHandler) Create(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		h.handleUpload(w, r, projectID)
	} else {
		h.handleTextCreate(w, r, projectID)
	}
}

func (h *ProjectFilesHandler) handleTextCreate(w http.ResponseWriter, r *http.Request, projectID int) {
	var req textFileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Filename == "" {
		writeError(w, http.StatusBadRequest, "filename is required")
		return
	}

	filename := filepath.Base(req.Filename)
	if filename == "." || filename == ".." {
		writeError(w, http.StatusBadRequest, "invalid filename")
		return
	}
	if refuseFlowScriptCopy(w, filename) {
		return
	}

	dir := filepath.Join(h.deps.ProjectDBManager.ProjectDir(projectID), "files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create directory")
		return
	}

	storagePath := filepath.Join(dir, filename)
	data := []byte(req.Content)
	if err := os.WriteFile(storagePath, data, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write file")
		return
	}

	ct := inferContentType(filename)

	projectDB := openProjectDB(w, h.deps.ProjectDBManager, projectID)
	if projectDB == nil {
		return
	}

	_, err := projectDB.ExecWrite(
		`INSERT INTO of_files (kind, path, storage, content_type, size, disk_path, is_deleted, updated_at)
		 VALUES ('file', ?, 'disk', ?, ?, ?, 0, CURRENT_TIMESTAMP)
		 ON CONFLICT(kind, path) DO UPDATE SET
		   storage = 'disk',
		   body = '',
		   content_type = excluded.content_type,
		   size = excluded.size,
		   disk_path = excluded.disk_path,
		   is_deleted = 0,
		   deleted_at = NULL,
		   updated_at = CURRENT_TIMESTAMP`,
		filename, ct, len(data), storagePath,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save file record")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"filename":     filename,
		"content_type": ct,
		"size":         len(data),
	})
}

func (h *ProjectFilesHandler) handleUpload(w http.ResponseWriter, r *http.Request, projectID int) {
	const maxUploadSize = 32 << 20   // 32 MB
	httpx.ExtendIO(w, 5*time.Minute) // a slow link outlives the 30 s read timeout
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		writeError(w, http.StatusBadRequest, "failed to parse form: "+err.Error())
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer func() { _ = file.Close() }()

	filename := filepath.Base(header.Filename)
	if filename == "." || filename == ".." {
		writeError(w, http.StatusBadRequest, "invalid filename")
		return
	}
	if refuseFlowScriptCopy(w, filename) {
		return
	}

	dir := filepath.Join(h.deps.ProjectDBManager.ProjectDir(projectID), "files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create directory")
		return
	}

	storagePath := filepath.Join(dir, filename)
	dst, err := os.Create(storagePath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create file")
		return
	}
	defer func() { _ = dst.Close() }()

	written, err := io.Copy(dst, file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write file")
		return
	}
	if written > maxUploadSize {
		_ = os.Remove(storagePath)
		writeError(w, http.StatusBadRequest, "file too large (max 32 MB)")
		return
	}

	ct := inferContentType(filename)
	if header.Header.Get("Content-Type") != "" && header.Header.Get("Content-Type") != "application/octet-stream" {
		ct = header.Header.Get("Content-Type")
	}

	description := r.FormValue("description")
	descMeta := `{}`
	if description != "" {
		b, _ := json.Marshal(map[string]string{"description": description})
		descMeta = string(b)
	}

	projectDB := openProjectDB(w, h.deps.ProjectDBManager, projectID)
	if projectDB == nil {
		return
	}

	_, err = projectDB.ExecWrite(
		`INSERT INTO of_files (kind, path, storage, content_type, size, disk_path, metadata, is_deleted, updated_at)
		 VALUES ('file', ?, 'disk', ?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
		 ON CONFLICT(kind, path) DO UPDATE SET
		   storage = 'disk',
		   body = '',
		   content_type = excluded.content_type,
		   size = excluded.size,
		   disk_path = excluded.disk_path,
		   metadata = excluded.metadata,
		   is_deleted = 0,
		   deleted_at = NULL,
		   updated_at = CURRENT_TIMESTAMP`,
		filename, ct, int(written), storagePath, descMeta,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save file record")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"filename":     filename,
		"content_type": ct,
		"size":         written,
	})
}

// Delete soft-deletes a file by ID.
func (h *ProjectFilesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	fileID, ok := requireIntParam(w, r, "fileID", "invalid file ID")
	if !ok {
		return
	}
	if kind, _ := fileRowKind(projectDB, fileID); kind == probe.FlowKind {
		writeError(w, http.StatusForbidden, probe.FlowPath+" is the project's user flow script. Builds expect one and report a missing script as a flow that did not pass, so it cannot be deleted. Edit it instead.")
		return
	}

	var storagePath, filePath string
	if err := projectDB.QueryRow(
		"SELECT disk_path, path FROM of_files WHERE kind = 'file' AND id = ?",
		fileID,
	).Scan(&storagePath, &filePath); err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	// Platform-managed files are OpenFabrica-owned and loaded by the project's
	// pages at runtime; deleting one ships a broken app. The model is already
	// blocked from deleting them; refuse the owner-facing path too. The UI hides
	// the delete button for these, so this is the backstop for a direct API call.
	if filePath != "" && store.IsPlatformManagedArtifact(projectDB.Reader(), "file", filePath) {
		writeError(w, http.StatusForbidden, "This is an OpenFabrica-managed file ("+filePath+") that your project loads at runtime; it cannot be deleted. It is re-created automatically when the relevant feature (auth or end-to-end encryption) is enabled.")
		return
	}

	if _, err := softDeleteArtifact(projectDB, "file", fileID); err != nil {
		h.deps.serverError(w, "failed to delete file", err)
		return
	}

	if storagePath != "" {
		_ = os.Remove(storagePath) // best-effort disk cleanup; the row is already soft-deleted
	}

	writeStatus(w, "deleted")
}

// Content serves the raw content of a file. Handles both storage modes:
// engine-authored text artifacts (CSS, JS, SVG) live in the body column,
// admin-uploaded binaries live on disk.
func (h *ProjectFilesHandler) Content(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	fileID, ok := requireIntParam(w, r, "fileID", "invalid file ID")
	if !ok {
		return
	}

	kind, found := fileRowKind(projectDB, fileID)
	if !found {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	var storage, body, diskPath sql.NullString
	err := projectDB.QueryRow(
		"SELECT storage, body, disk_path FROM of_files WHERE kind = ? AND id = ?",
		kind, fileID,
	).Scan(&storage, &body, &diskPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	data, err := store.ReadArtifactBytes(storage, body, diskPath, h.deps.ProjectDBManager.ProjectDir(projectID))
	if err != nil {
		writeError(w, http.StatusNotFound, "file content unavailable")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

// UpdateContent updates the content of a text file. Respects the artifact's
// existing storage mode: writes to the body column for inline artifacts,
// to disk for disk-stored ones.
func (h *ProjectFilesHandler) UpdateContent(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	fileID, ok := requireIntParam(w, r, "fileID", "invalid file ID")
	if !ok {
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if kind, _ := fileRowKind(projectDB, fileID); kind == probe.FlowKind {
		h.updateFlowScript(w, projectDB, fileID, req.Content)
		return
	}

	var storage, diskPath sql.NullString
	var filePath string
	err := projectDB.QueryRow(
		"SELECT storage, disk_path, path FROM of_files WHERE kind = 'file' AND id = ?",
		fileID,
	).Scan(&storage, &diskPath, &filePath)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	// Platform-managed files are OpenFabrica-owned and vetted; overwriting one
	// breaks the guarantee it provides. This admin path writes the row directly
	// (bypassing the store-level save guard), so it needs its own check. The UI
	// hides edit for these; this is the API backstop.
	if store.IsPlatformManagedArtifact(projectDB.Reader(), "file", filePath) {
		writeError(w, http.StatusForbidden, "This is an OpenFabrica-managed file ("+filePath+") and cannot be edited; the platform owns and re-emits it.")
		return
	}

	switch storage.String {
	case "inline", "":
		if _, err := projectDB.ExecWrite(
			"UPDATE of_files SET body = ?, size = ?, updated_at = CURRENT_TIMESTAMP WHERE kind = 'file' AND id = ?",
			req.Content, len(req.Content), fileID,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update file")
			return
		}
	case "disk":
		if diskPath.String == "" {
			writeError(w, http.StatusInternalServerError, "file storage path is missing")
			return
		}
		if err := os.WriteFile(diskPath.String, []byte(req.Content), 0644); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to write file")
			return
		}
		if _, err := projectDB.ExecWrite("UPDATE of_files SET size = ? WHERE kind = 'file' AND id = ?", len(req.Content), fileID); err != nil {
			h.deps.Logger.Warn("failed to update artifact size after disk write", "error", err, "id", fileID)
		}
	default:
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("unknown storage mode %q", storage.String))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"updated": fileID})
}

// ListVersions returns version history for a file.
func (h *ProjectFilesHandler) ListVersions(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	fileID, ok := requireIntParam(w, r, "fileID", "invalid file ID")
	if !ok {
		return
	}

	kind, _ := fileRowKind(projectDB, fileID)
	respondArtifactVersions(w, projectDB, kind, fileID)
}

// Restore reverts a file to a previous snapshotted body.
func (h *ProjectFilesHandler) Restore(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	fileID, ok := requireIntParam(w, r, "fileID", "invalid file ID")
	if !ok {
		return
	}
	kind, _ := fileRowKind(projectDB, fileID)
	respondArtifactRestore(w, r, projectDB, kind, fileID)
}

func inferContentType(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".webp":
		return "image/webp"
	case ".css":
		return "text/css"
	case ".js":
		return "application/javascript"
	case ".html", ".htm":
		return "text/html"
	case ".json":
		return "application/json"
	case ".pdf":
		return "application/pdf"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".ttf":
		return "font/ttf"
	default:
		return "application/octet-stream"
	}
}

// fileRowKind returns the kind of the Files-view row with this id: "file", or
// probe.FlowKind for the user-flow script. found is false for any other row, so
// the Files routes never reach a page, a layout or another kind by id. An unknown
// id reads as "file", which keeps the existing not-found answers.
func fileRowKind(projectDB *db.ProjectDB, id int) (kind string, found bool) {
	err := projectDB.QueryRow(
		`SELECT kind FROM of_files WHERE id = ? AND (kind = 'file' OR (kind = ? AND path = ?))`,
		id, probe.FlowKind, probe.FlowPath,
	).Scan(&kind)
	if err != nil {
		return "file", false
	}
	return kind, true
}

// refuseFlowScriptCopy answers 400 for a new file named of_test.js: it would be a
// separate, public copy that the flow runner never reads (it reads the kind='test'
// script listed alongside the files).
func refuseFlowScriptCopy(w http.ResponseWriter, filename string) bool {
	if !strings.EqualFold(filename, probe.FlowPath) {
		return false
	}
	writeError(w, http.StatusBadRequest, probe.FlowPath+" is the project's user-flow script and already has its own entry in this list: edit that one. A new file with this name would be a separate public copy the flows never read.")
	return true
}

// updateFlowScript saves an owner's edit of the user-flow script. It goes through
// the same parse and checks as manage_testing write_flows, so a script that would
// fail to load never replaces a working one; the error names what to change.
// Unlike write_flows it does not replay: the next test_flows or build does.
func (h *ProjectFilesHandler) updateFlowScript(w http.ResponseWriter, projectDB *db.ProjectDB, fileID int, content string) {
	suite, err := probe.ParseFlowSource(content)
	if err != nil {
		writeError(w, http.StatusBadRequest, probe.FlowPath+" was not saved: "+err.Error())
		return
	}
	if _, _, err := store.ArtifactSave(projectDB.Writer(), &store.Artifact{
		Kind:        probe.FlowKind,
		Path:        probe.FlowPath,
		Storage:     store.StorageInline,
		Body:        content,
		ContentType: "text/javascript",
		Scope:       probe.FlowKind,
	}, "admin"); err != nil {
		h.deps.serverError(w, "failed to save "+probe.FlowPath, err)
		return
	}
	resp := map[string]any{"updated": fileID, "flows": len(suite.Flows)}
	if warnings := suite.Lint(); len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, resp)
}
