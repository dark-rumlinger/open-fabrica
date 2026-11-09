package admin

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/engine"
	"openfabrica/events"
	"openfabrica/internal/diskpath"
	"openfabrica/internal/httpx"
	"openfabrica/internal/msgcat"
	"openfabrica/security"
)

// ProjectTransferHandler implements project portability: exporting a live
// project to a single `.of` file (a zip) and importing such a file as a new
// project. This is "Copy, but serialized to a portable file" so it reuses the
// same duplication semantics as ProjectDBManager.CopyFrom: a VACUUM INTO
// snapshot of the per-project DB, a recursive copy of on-disk assets, and a
// reset of ephemeral state (db.EphemeralResetStmts).
type ProjectTransferHandler struct {
	deps *Deps
}

const (
	// projectExportFormat / Version identify the .of manifest so import can
	// reject foreign or future archives instead of corrupting a new project.
	projectExportFormat  = "openfabrica-project"
	projectExportVersion = 1

	// maxImportSize caps the uploaded .of file. Generous because exports bundle
	// the project DB plus all on-disk assets (images, uploads).
	maxImportSize = 512 << 20 // 512 MB

	// maxImportUncompressed caps total decompressed bytes across all archive
	// entries so a zip bomb (tiny compressed, enormous decompressed) can't fill
	// the disk on import. Generous relative to a real export; the import path is
	// admin-only, so this is a backstop, not a tight quota.
	maxImportUncompressed = 4 << 30 // 4 GB

	// transferTimeout bounds one export download or import upload, replacing the
	// server's 30 s read / 120 s write defaults for those handlers only.
	transferTimeout = 15 * time.Minute
)

// projectManifest is the metadata carried in manifest.json inside a .of file.
// The LLM model is recorded by stable identity (provider vendor + upstream
// model id), never the instance-local llm_models.id, so import can resolve it
// on a different instance and fall back to the system default if absent.
type projectManifest struct {
	Format          string  `json:"format"`
	FormatVersion   int     `json:"format_version"`
	ExportedAt      string  `json:"exported_at"`
	SourceProjectID int     `json:"source_project_id"`
	Name            string  `json:"name"`
	Description     *string `json:"description"`
	ProviderVendor  string  `json:"provider_vendor"`
	ModelID         string  `json:"model_id"`
	AmbitionLevel   string  `json:"ambition_level"`
	// UserFlows is the owner's effective choice about user-flow tests; absent (an older
	// archive) means the platform default.
	UserFlows *bool `json:"user_flows,omitempty"`
}

// Export streams a live project as a `<slug>.of` zip archive.
//
//	GET /admin/api/projects/{projectID}/export
func (h *ProjectTransferHandler) Export(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	// A large project's archive takes longer to stream than the server's blanket
	// write timeout allows; a cut-off zip is a corrupt export.
	httpx.ExtendIO(w, transferTimeout)

	project, err := models.GetProjectByID(h.deps.DB.DB, projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	// Sharing is for working apps: only allow exporting a project that has gone
	// live. Mid-build projects are incomplete and not meant to be distributed.
	if project.Mode != "live" {
		writeError(w, http.StatusConflict, "project must be live to export")
		return
	}

	// Snapshot the per-project DB into a temp file, then sanitise the copy.
	tmpDir, err := os.MkdirTemp("", "of-export-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp dir")
		return
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	snapPath := filepath.Join(tmpDir, "project.db")
	if err := h.deps.ProjectDBManager.SnapshotTo(projectID, snapPath); err != nil {
		h.deps.Logger.Error("export snapshot failed", "project_id", projectID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to snapshot project database")
		return
	}
	if err := sanitizeExportDB(snapPath); err != nil {
		h.deps.Logger.Error("export sanitise failed", "project_id", projectID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to prepare project database")
		return
	}

	manifest := projectManifest{
		Format:          projectExportFormat,
		FormatVersion:   projectExportVersion,
		ExportedAt:      time.Now().UTC().Format(time.RFC3339),
		SourceProjectID: projectID,
		Name:            project.Name,
		Description:     project.Description,
		AmbitionLevel:   project.AmbitionLevel,
	}
	enabled := models.ProjectUserFlowsEnabled(project)
	manifest.UserFlows = &enabled
	// Resolve the model's portable identity (vendor + upstream model id). Best
	// effort: a missing model just yields empty fields and import falls back to
	// the system default.
	_ = h.deps.DB.DB.QueryRow(
		`SELECT p.vendor, m.model_id
		   FROM llm_models m JOIN providers p ON p.id = m.provider_id
		  WHERE m.id = ?`, project.LLMModelID,
	).Scan(&manifest.ProviderVendor, &manifest.ModelID)

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build manifest")
		return
	}

	filename := slugifyProjectName(project.Name) + ".of"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }() // response already streaming; a close error can't change it

	// manifest.json
	if err := writeZipBytes(zw, "manifest.json", manifestBytes); err != nil {
		h.deps.Logger.Error("export zip manifest failed", "project_id", projectID, "error", err)
		return
	}
	// project.db (the sanitised snapshot)
	if err := writeZipFile(zw, "project.db", snapPath); err != nil {
		h.deps.Logger.Error("export zip db failed", "project_id", projectID, "error", err)
		return
	}
	// All on-disk assets: walk the project dir and bundle every file except the
	// live project.db and its WAL/SHM sidecars (the snapshot supersedes them).
	// Mirrors CopyFrom, which copies every aux entry verbatim.
	projectDir := h.deps.ProjectDBManager.ProjectDir(projectID)
	if err := addProjectAssets(zw, projectDir); err != nil {
		h.deps.Logger.Error("export zip assets failed", "project_id", projectID, "error", err)
		return
	}
}

// Import creates a new project from an uploaded `.of` file.
//
//	POST /admin/api/projects/import  (multipart form, field "file")
func (h *ProjectTransferHandler) Import(w http.ResponseWriter, r *http.Request) {
	// Up to maxImportSize arrives over a possibly slow link; the blanket 30 s read
	// timeout would drop it.
	httpx.ExtendIO(w, transferTimeout)
	r.Body = http.MaxBytesReader(w, r.Body, maxImportSize)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid upload (file too large or malformed)")
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer func() { _ = file.Close() }()

	// zip.NewReader needs an io.ReaderAt + size, so spool the upload to a temp
	// file rather than holding it all in memory.
	tmpZip, err := os.CreateTemp("", "of-import-*.zip")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp file")
		return
	}
	defer func() { _ = os.Remove(tmpZip.Name()) }()
	defer func() { _ = tmpZip.Close() }()

	size, err := io.Copy(tmpZip, file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read upload")
		return
	}

	zr, err := zip.NewReader(tmpZip, size)
	if err != nil {
		writeError(w, http.StatusBadRequest, "not a valid .of file (bad zip)")
		return
	}

	manifest, err := readManifest(zr)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve the LLM model on this instance, falling back to the default.
	modelID, err := h.resolveImportModel(manifest)
	if err != nil {
		writeError(w, http.StatusBadRequest, "no usable LLM model on this instance: "+err.Error())
		return
	}

	// Create the global row first to allocate a fresh project ID. Imported
	// projects land unpublished + paused (no domain) so the owner reviews,
	// re-enters secrets, sets a domain, and goes live deliberately. Paused
	// reads as "parked, awaiting your action" rather than an active build.
	name := strings.TrimSpace(manifest.Name)
	if name == "" {
		name = "Imported project"
	}
	name += " (imported)"

	project, err := models.CreateProject(h.deps.DB.DB, name, nil, manifest.Description, modelID, nil, engine.NormalizeAmbition(manifest.AmbitionLevel))
	if err != nil {
		h.deps.Logger.Error("import create project failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create project")
		return
	}
	newID := project.ID
	if manifest.UserFlows != nil {
		if err := models.SetProjectUserFlows(h.deps.DB.DB, newID, *manifest.UserFlows); err != nil {
			h.deps.Logger.Warn("import: failed to store the user-flows setting", "error", err)
		}
	}

	// From here on, any failure must roll back the half-created project.
	cleanup := func() {
		_ = h.deps.ProjectDBManager.Delete(newID) // closes handles (if open) + removes dir
		_ = models.DeleteProject(h.deps.DB.DB, newID)
	}

	if err := models.SetProjectPublished(h.deps.DB.DB, newID, false); err != nil {
		cleanup()
		writeError(w, http.StatusInternalServerError, "failed to set imported project unpublished")
		return
	}

	if err := models.UpdateProjectMode(h.deps.DB.DB, newID, models.ModePaused); err != nil {
		cleanup()
		writeError(w, http.StatusInternalServerError, "failed to set imported project paused")
		return
	}
	project.Mode = models.ModePaused

	destDir := h.deps.ProjectDBManager.ProjectDir(newID)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		cleanup()
		writeError(w, http.StatusInternalServerError, "failed to create project directory")
		return
	}

	// Extract every archive entry into the new project dir. manifest.json is
	// metadata only; project.db and all assets land verbatim under destDir.
	sawDB := false
	var extractedTotal int64
	for _, f := range zr.File {
		if f.Name == "manifest.json" {
			continue
		}
		target, ok := safeJoin(destDir, f.Name)
		if !ok {
			cleanup()
			writeError(w, http.StatusBadRequest, "archive contains an unsafe path")
			return
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				cleanup()
				writeError(w, http.StatusInternalServerError, "failed to extract directory")
				return
			}
			continue
		}
		if f.Name == "project.db" {
			sawDB = true
		}
		n, err := extractZipFile(f, target, maxImportUncompressed-extractedTotal)
		if err != nil {
			cleanup()
			writeError(w, http.StatusInternalServerError, "failed to extract file")
			return
		}
		extractedTotal += n
	}
	if !sawDB {
		cleanup()
		writeError(w, http.StatusBadRequest, "archive is missing project.db")
		return
	}

	// Open via the manager so idempotent migrations bring the imported DB up to
	// this instance's project schema.
	pdb, err := h.deps.ProjectDBManager.Open(newID)
	if err != nil {
		cleanup()
		writeError(w, http.StatusInternalServerError, "failed to open imported database")
		return
	}

	// Rewrite disk_path on disk-stored artifacts from the source project ID to
	// the new one. Without this the public file handler rejects every uploaded
	// asset (it enforces a data/projects/{thisID} prefix). Done row-by-row in Go
	// so it also normalises separators across OSes (a Windows export imported on
	// Linux, or vice versa).
	if err := remapProjectDiskPaths(pdb, manifest.SourceProjectID, h.deps.ProjectDBManager.ProjectDir(newID)); err != nil {
		h.deps.Logger.Error("import disk_path remap failed", "new_id", newID, "error", err)
		cleanup()
		writeError(w, http.StatusInternalServerError, "failed to relink project assets")
		return
	}

	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventProjectCreated, newID, map[string]any{
			"name":     project.Name,
			"imported": true,
		}))
	}

	// Reload so the response reflects the unpublished state.
	if reloaded, err := models.GetProjectByID(h.deps.DB.DB, newID); err == nil {
		project = reloaded
	}
	writeJSON(w, http.StatusCreated, h.deps.viewProject(project))
}

// resolveImportModel maps the manifest's portable model identity to a local
// llm_models.id, falling back to the system default model.
func (h *ProjectTransferHandler) resolveImportModel(m projectManifest) (int, error) {
	if m.ProviderVendor != "" && m.ModelID != "" {
		var id int
		err := h.deps.DB.DB.QueryRow(
			`SELECT m.id FROM llm_models m JOIN providers p ON p.id = m.provider_id
			  WHERE p.vendor = ? AND m.model_id = ? LIMIT 1`,
			m.ProviderVendor, m.ModelID,
		).Scan(&id)
		if err == nil && id > 0 {
			return id, nil
		}
	}
	def, _, err := models.GetDefaultModel(h.deps.DB.DB)
	if err != nil {
		return 0, errors.New(msgcat.Mark("configure a provider and model first"))
	}
	return def.ID, nil
}

// sanitizeExportDB strips per-instance state and secret values from a snapshot
// DB before it is bundled into a shareable .of file. Foreign keys are disabled
// so the deletes are order-independent.
func sanitizeExportDB(path string) error {
	dsn := fmt.Sprintf("%s?_pragma=foreign_keys(0)&_pragma=busy_timeout(30000)", path)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("opening snapshot: %w", err)
	}
	defer d.Close()
	d.SetMaxOpenConns(1)

	stmts := make([]string, 0, len(db.EphemeralResetStmts)+18)
	stmts = append(stmts, db.EphemeralResetStmts...)
	stmts = append(stmts,
		// History and its body snapshots are stripped per the export policy
		// (smaller, cleaner file). of_snapshot_blobs holds version snapshots and is
		// distinct from of_blobs (the general blob store, which is preserved).
		"DELETE FROM of_file_history",
		"DELETE FROM of_snapshot_blobs",
		// Secret values are encrypted with this instance's key and useless /
		// unsafe elsewhere. Keep the names so the importer knows what to set.
		"UPDATE of_secrets SET value_encrypted = ''",
		// A shared project is a clean template: it carries the app and its
		// authored content but none of its secrets or end-user data. Blank the
		// plaintext webhook signing secret (keeping the webhook config so the
		// importer just regenerates a secret; an incoming webhook is rejected
		// until one is set), and drop per-user 2FA secrets, live login/room/invite
		// tokens, and tenant records - all instance-scoped and meaningless or
		// unsafe elsewhere. End-user account rows are cleared separately below.
		"UPDATE of_webhooks SET secret_encrypted = ''",
		"DELETE FROM of_user_totp",
		"DELETE FROM of_magic_tokens",
		"DELETE FROM of_ws_room_tokens",
		"DELETE FROM of_tenant_invites",
		"DELETE FROM of_tenant_members",
		"DELETE FROM of_tenants",
		// End-user records that are not account tables: orders and subscriptions
		// (payer identity, amounts), push endpoints (browser keys), webhook
		// delivery logs and replay ids (request bodies), agent run transcripts,
		// live room state and per-day LLM spend. Config rows (of_payment_config,
		// of_subscription_plans, of_webhooks) are authored content and stay.
		"DELETE FROM of_payments",
		"DELETE FROM of_subscriptions",
		"DELETE FROM of_push_subscriptions",
		"DELETE FROM of_webhook_logs",
		"DELETE FROM of_processed_webhooks",
		"DELETE FROM of_agent_runs",
		"DELETE FROM of_ws_room_state",
		"DELETE FROM of_llm_usage_daily",
	)
	for _, stmt := range stmts {
		if _, err := d.Exec(stmt); err != nil {
			// Tolerate tables that don't exist in this schema version.
			if !strings.Contains(err.Error(), "no such table") {
				return fmt.Errorf("sanitising (%s): %w", stmt, err)
			}
		}
	}

	// Empty every dynamic account table so the template ships with no users.
	if err := clearAuthAccountTables(d); err != nil {
		return err
	}

	// Reclaim space freed by the deletes.
	if _, err := d.Exec("VACUUM"); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}

// clearAuthAccountTables empties every dynamic table an auth endpoint
// authenticates against, so an exported template carries no end-user accounts
// (emails, password hashes). The table names come from of_auth_endpoints.table_name
// - the authoritative pointer to each account table, alongside its username/password
// columns - and are quoted as identifiers before interpolation. Only auth-referenced
// tables are cleared; other business tables are preserved as app content. A missing
// of_auth_endpoints table (older schema) is tolerated as a no-op.
func clearAuthAccountTables(d *sql.DB) error {
	rows, err := d.Query("SELECT DISTINCT table_name FROM of_auth_endpoints WHERE table_name <> ''")
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return fmt.Errorf("listing auth account tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("scanning auth account table: %w", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing auth account tables: %w", err)
	}

	for _, name := range tables {
		stmt := fmt.Sprintf("DELETE FROM %s", security.QuoteIdent(name))
		if _, err := d.Exec(stmt); err != nil {
			// An auth endpoint may point at a table that no longer exists.
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			return fmt.Errorf("clearing account table %q: %w", name, err)
		}
	}
	return nil
}

// diskPathDB is the subset of *db.ProjectDB the remap needs: a read query and a
// serialized write. Declared as an interface so the remap is unit-testable
// against a plain *sql.DB adapter.
type diskPathDB interface {
	Query(query string, args ...any) (*sql.Rows, error)
	ExecWrite(query string, args ...any) (sql.Result, error)
}

// remapProjectDiskPaths re-anchors every stored file location from the source
// project to newDir (see diskpath.RemapProject).
func remapProjectDiskPaths(pdb diskPathDB, oldID int, newDir string) error {
	return diskpath.RemapProject(pdb, oldID, newDir)
}

// remapDiskPath re-anchors one stored path (see diskpath.Rebase).
func remapDiskPath(p string, oldID int, newDir string) string {
	return diskpath.Rebase(p, oldID, newDir)
}

// readManifest locates and validates manifest.json inside the archive.
func readManifest(zr *zip.Reader) (projectManifest, error) {
	var m projectManifest
	var entry *zip.File
	for _, f := range zr.File {
		if f.Name == "manifest.json" {
			entry = f
			break
		}
	}
	if entry == nil {
		return m, errors.New(msgcat.Mark("not a valid .of file (missing manifest.json)"))
	}
	rc, err := entry.Open()
	if err != nil {
		return m, errors.New(msgcat.Mark("failed to read manifest"))
	}
	defer rc.Close()
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		return m, errors.New(msgcat.Mark("malformed manifest.json"))
	}
	if m.Format != projectExportFormat {
		return m, errors.New(msgcat.Mark("unrecognised file format"))
	}
	if m.FormatVersion > projectExportVersion {
		return m, errors.New(msgcat.Mark("file was created by a newer version of OpenFabrica"))
	}
	return m, nil
}

// addProjectAssets bundles every file under projectDir into the zip, skipping
// the live project.db and its WAL/SHM sidecars. Entry names are the project-dir
// relative path in forward-slash form.
func addProjectAssets(zw *zip.Writer, projectDir string) error {
	return filepath.WalkDir(projectDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // project dir may have no assets yet
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()
		if base == "project.db" || strings.HasPrefix(base, "project.db-") {
			return nil
		}
		rel, err := filepath.Rel(projectDir, path)
		if err != nil {
			return err
		}
		return writeZipFile(zw, filepath.ToSlash(rel), path)
	})
}

// writeZipBytes adds an in-memory entry to the archive.
func writeZipBytes(zw *zip.Writer, name string, data []byte) error {
	wr, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = wr.Write(data)
	return err
}

// writeZipFile streams a file from disk into the archive under name.
func writeZipFile(zw *zip.Writer, name, srcPath string) error {
	wr, err := zw.Create(name)
	if err != nil {
		return err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	_, err = io.Copy(wr, in)
	return err
}

// extractZipFile writes one archive entry to target on disk, refusing to write
// more than limit bytes (the remaining decompression budget). This bounds a zip
// bomb - tiny compressed, enormous decompressed - since the zip header's
// declared uncompressed size can't be trusted. Returns the bytes written.
func extractZipFile(f *zip.File, target string, limit int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	// LimitReader to limit+1: an entry that exactly consumes the remaining budget
	// still extracts, but one byte beyond it is detectable as an overflow.
	n, err := io.Copy(out, io.LimitReader(rc, limit+1))
	if err != nil {
		_ = out.Close()
		return n, err
	}
	if cerr := out.Close(); cerr != nil {
		return n, cerr
	}
	if n > limit {
		return n, fmt.Errorf(msgcat.Mark("import archive exceeds the %d-byte decompression budget"), maxImportUncompressed)
	}
	return n, nil
}

// safeJoin joins name onto baseDir and guards against zip-slip (entries that
// use ../ or absolute paths to escape the target directory).
func safeJoin(baseDir, name string) (string, bool) {
	target := filepath.Join(baseDir, filepath.FromSlash(name))
	cleanBase := filepath.Clean(baseDir)
	if target != cleanBase && !strings.HasPrefix(target, cleanBase+string(os.PathSeparator)) {
		return "", false
	}
	return target, true
}

// slugifyName turns a display name into a lowercase alnum slug, collapsing
// runs of any other characters into single dashes. Shared by project export
// filenames and vendor ids.
func slugifyName(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// slugifyProjectName turns a project name into a safe lowercase filename stem.
func slugifyProjectName(name string) string {
	if slug := slugifyName(name); slug != "" {
		return slug
	}
	return "project"
}
