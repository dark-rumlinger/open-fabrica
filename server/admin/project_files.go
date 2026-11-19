package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	"openfabrica/internal/httpx"
	"openfabrica/internal/mediatype"
	"openfabrica/probe"
	"openfabrica/store"
)

// ProjectFilesHandler handles all project files (uploads + engine-authored artifacts).
type ProjectFilesHandler struct {
	deps *Deps
}

// List returns all files for a project.
func (h *ProjectFilesHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListProjectFiles(projectDB, projectID, probe.FlowKind, probe.FlowPath))
}

// Preview serves the latest build preview screenshot (preview.png) for a project.
func (h *ProjectFilesHandler) Preview(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}
	p := filepath.Join(h.deps.ProjectDBManager.ProjectDir(projectID), "preview.png")
	data, err := os.ReadFile(p)
	if err != nil {
		writeError(w, http.StatusNotFound, "preview not available")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ChatImage serves a user-uploaded chat image attachment for a project.
func (h *ProjectFilesHandler) ChatImage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}
	name := filepath.Base(chi.URLParam(r, "name"))
	if name == "" || name == "." || name == ".." {
		writeError(w, http.StatusBadRequest, "invalid image name")
		return
	}
	p := filepath.Join(h.deps.ProjectDBManager.ProjectDir(projectID), "chat_images", name)
	data, err := os.ReadFile(p)
	if err != nil {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	ct := "image/jpeg"
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		ct = "image/png"
	case ".webp":
		ct = "image/webp"
	case ".gif":
		ct = "image/gif"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=31536000")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

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
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
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

	filename, ok := cleanProjectFilePath(req.Filename)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid filename")
		return
	}
	if refuseFlowScriptCopy(w, filename) {
		return
	}

	dir := filepath.Join(h.deps.ProjectDBManager.ProjectDir(projectID), "files")
	storagePath := filepath.Join(dir, filepath.FromSlash(filename))
	if err := os.MkdirAll(filepath.Dir(storagePath), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create directory")
		return
	}

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

	if err := store.UpsertDiskFile(projectDB, filename, ct, len(data), storagePath, ""); err != nil {
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
	const maxUploadSize = 32 << 20
	httpx.ExtendIO(w, 5*time.Minute)
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

	baseName := filepath.Base(strings.ReplaceAll(header.Filename, "\\", "/"))
	if baseName == "" || baseName == "." || baseName == ".." {
		writeError(w, http.StatusBadRequest, "invalid filename")
		return
	}
	folder, ok := cleanProjectFolderPath(r.FormValue("folder"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid folder")
		return
	}
	filename := baseName
	if folder != "" {
		filename = folder + "/" + baseName
	}
	if refuseFlowScriptCopy(w, filename) {
		return
	}

	dir := filepath.Join(h.deps.ProjectDBManager.ProjectDir(projectID), "files")
	storagePath := filepath.Join(dir, filepath.FromSlash(filename))
	if err := os.MkdirAll(filepath.Dir(storagePath), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create directory")
		return
	}
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

	if err := store.UpsertDiskFile(projectDB, filename, ct, int(written), storagePath, descMeta); err != nil {
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
	if kind, _ := store.FileRowKind(projectDB, fileID, probe.FlowKind, probe.FlowPath); kind == probe.FlowKind {
		writeError(w, http.StatusForbidden, probe.FlowPath+" is the project's user flow script. Builds expect one and report a missing script as a flow that did not pass, so it cannot be deleted. Edit it instead.")
		return
	}

	loc, err := store.LoadFileLocation(projectDB, "file", fileID)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	if loc.Path != "" && store.IsPlatformManagedArtifact(projectDB.Reader(), "file", loc.Path) {
		writeError(w, http.StatusForbidden, "This is an OpenFabrica-managed file ("+loc.Path+") that your project loads at runtime; it cannot be deleted. It is re-created automatically when the relevant feature (auth or end-to-end encryption) is enabled.")
		return
	}

	if _, err := softDeleteArtifact(projectDB, "file", fileID); err != nil {
		h.deps.serverError(w, "failed to delete file", err)
		return
	}

	if loc.DiskPath.String != "" {
		_ = os.Remove(loc.DiskPath.String)
	}

	writeStatus(w, "deleted")
}

// Content serves the raw content of a file.
func (h *ProjectFilesHandler) Content(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	fileID, ok := requireIntParam(w, r, "fileID", "invalid file ID")
	if !ok {
		return
	}

	kind, found := store.FileRowKind(projectDB, fileID, probe.FlowKind, probe.FlowPath)
	if !found {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	loc, err := store.LoadFileLocation(projectDB, kind, fileID)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	data, err := store.ReadArtifactBytes(loc.Storage, loc.Body, loc.DiskPath, h.deps.ProjectDBManager.ProjectDir(projectID))
	if err != nil {
		writeError(w, http.StatusNotFound, "file content unavailable")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

// UpdateContent updates the content of a text file.
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
	if kind, _ := store.FileRowKind(projectDB, fileID, probe.FlowKind, probe.FlowPath); kind == probe.FlowKind {
		h.updateFlowScript(w, projectDB, fileID, req.Content)
		return
	}

	loc, err := store.LoadFileLocation(projectDB, "file", fileID)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	if store.IsPlatformManagedArtifact(projectDB.Reader(), "file", loc.Path) {
		writeError(w, http.StatusForbidden, "This is an OpenFabrica-managed file ("+loc.Path+") and cannot be edited; the platform owns and re-emits it.")
		return
	}

	switch loc.Storage.String {
	case "inline", "":
		if err := store.UpdateInlineFileBody(projectDB, fileID, req.Content); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update file")
			return
		}
	case "disk":
		if loc.DiskPath.String == "" {
			writeError(w, http.StatusInternalServerError, "file storage path is missing")
			return
		}
		if err := os.WriteFile(loc.DiskPath.String, []byte(req.Content), 0644); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to write file")
			return
		}
		if err := store.UpdateFileSize(projectDB, fileID, len(req.Content)); err != nil {
			h.deps.Logger.Warn("failed to update artifact size after disk write", "error", err, "id", fileID)
		}
	default:
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("unknown storage mode %q", loc.Storage.String))
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
	kind, _ := store.FileRowKind(projectDB, fileID, probe.FlowKind, probe.FlowPath)
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
	kind, _ := store.FileRowKind(projectDB, fileID, probe.FlowKind, probe.FlowPath)
	respondArtifactRestore(w, r, projectDB, kind, fileID)
}

func inferContentType(filename string) string {
	if ct := mediatype.ContentTypeByFilename(filename); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func refuseFlowScriptCopy(w http.ResponseWriter, filename string) bool {
	if !strings.EqualFold(filename, probe.FlowPath) {
		return false
	}
	writeError(w, http.StatusBadRequest, probe.FlowPath+" is the project's user-flow script and already has its own entry in this list: edit that one. A new file with this name would be a separate public copy the flows never read.")
	return true
}

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

// cleanProjectFilePath normalizes a relative file path (allowing subfolders)
// and rejects traversal or empty segments.
func cleanProjectFilePath(raw string) (string, bool) {
	norm := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	norm = strings.TrimLeft(norm, "/")
	if norm == "" {
		return "", false
	}
	parts := strings.Split(norm, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", false
		}
	}
	return strings.Join(parts, "/"), true
}

// cleanProjectFolderPath normalizes an optional folder path; empty means root.
func cleanProjectFolderPath(raw string) (string, bool) {
	norm := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	norm = strings.Trim(norm, "/")
	if norm == "" {
		return "", true
	}
	return cleanProjectFilePath(norm)
}
