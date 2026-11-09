package public

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/internal/endpoints"
	"openfabrica/internal/httpx"
	"openfabrica/security"
	"openfabrica/server/middleware"
	"openfabrica/store"
)

// uploadBasenameSanitizer matches characters disallowed in on-disk upload filenames.
// Anything outside a small ASCII whitelist collapses to underscores.
var uploadBasenameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeUploadBasename(name string) string {
	cleaned := uploadBasenameSanitizer.ReplaceAllString(name, "_")
	cleaned = strings.Trim(cleaned, "._-")
	if cleaned == "" {
		cleaned = "file"
	}
	if len(cleaned) > 80 {
		cleaned = cleaned[:80]
	}
	return cleaned
}

// File Upload handler

type uploadEndpointConfig struct {
	Path         string
	AllowedTypes []string
	MaxSizeMB    int
	RequiresAuth bool
	RateLimit    int
	TableName    string
	OwnerColumn  string
	TenantColumn string
}

func (h *Handler) loadUploadEndpoint(projectDB *sql.DB, path string) (*uploadEndpointConfig, error) {
	var ue uploadEndpointConfig
	var allowedTypesJSON string
	var tableName, ownerColumn, tenantColumn sql.NullString
	err := projectDB.QueryRow(
		"SELECT path, allowed_types, max_size_mb, requires_auth, COALESCE(rate_limit,0), table_name, COALESCE(owner_column,''), COALESCE(tenant_column,'') FROM of_upload_endpoints WHERE path = ?",
		path,
	).Scan(&ue.Path, &allowedTypesJSON, &ue.MaxSizeMB, &ue.RequiresAuth, &ue.RateLimit, &tableName, &ownerColumn, &tenantColumn)
	if err != nil {
		return nil, err
	}
	if allowedTypesJSON != "" {
		if err := json.Unmarshal([]byte(allowedTypesJSON), &ue.AllowedTypes); err != nil {
			slog.Warn("invalid allowed_types JSON in upload endpoint", "path", path, "error", err)
		}
	}
	if ue.MaxSizeMB == 0 {
		ue.MaxSizeMB = 5
	}
	if ue.MaxSizeMB > endpoints.MaxUploadSizeMB {
		ue.MaxSizeMB = endpoints.MaxUploadSizeMB
	}
	ue.TableName = tableName.String
	ue.OwnerColumn = ownerColumn.String
	ue.TenantColumn = tenantColumn.String
	return &ue, nil
}

// mimeTypeMatches checks if a content type matches a glob pattern (e.g. "image/*").
func mimeTypeMatches(contentType string, pattern string) bool {
	if pattern == "*/*" || pattern == "*" {
		return true
	}
	// Try exact match first.
	if strings.EqualFold(contentType, pattern) {
		return true
	}
	// Glob match: "image/*" matches "image/png".
	if strings.HasSuffix(pattern, "/*") {
		prefix := pattern[:len(pattern)-1] // "image/"
		return strings.HasPrefix(strings.ToLower(contentType), strings.ToLower(prefix))
	}
	return false
}

// apiUpload handles POST /api/{path}/upload: multipart file uploads.
func (h *Handler) apiUpload(w http.ResponseWriter, r *http.Request, projectID int, projectDB *db.ProjectDB, endpointPath string) {
	// Cap and drain the body BEFORE any early return, including the not-found
	// below. Without this a missing endpoint row resets the connection while the
	// client is still sending the multipart body instead of returning a clean
	// 404. The precise per-endpoint cap is applied once the config row loads.
	// The body size cap is the real bound, so the deadline only has to outlast a
	// slow upload of that size rather than the server's 30 s read default.
	httpx.ExtendIO(w, 10*time.Minute)
	r.Body = http.MaxBytesReader(w, r.Body, int64(endpoints.MaxUploadSizeMB)*1024*1024+1024)

	ue, err := h.loadUploadEndpoint(projectDB.Reader(), endpointPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "upload endpoint not found")
		return
	}

	// Retighten to this endpoint's configured cap now that it is known.
	maxBytes := int64(ue.MaxSizeMB) * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1024)

	// Per-endpoint rate limit (requests/min per IP), configured via manage_uploads.
	// Checked after MaxBytesReader is installed so a rejected request still drains
	// its body gracefully. rate_limit=0 disables the cap.
	if !checkEndpointRateLimit("upload:"+projectKeyOf(r)+":"+endpointPath+":"+middleware.RateLimitIdentity(r), ue.RateLimit) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	// Check auth and extract claims for owner/tenant stamping. Even when
	// RequiresAuth is false, we still try to read claims so an upload by an
	// authenticated user can be tenant-scoped.
	var authUserID, authTenantID int
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if ue.RequiresAuth {
		ok, claims := h.validateProjectTokenOrJWTWithClaims(projectID, token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if claims != nil {
			authUserID = claims.UserID
			authTenantID = claims.TenantID
		}
	} else if token != "" {
		if ok, claims := h.validateProjectTokenOrJWTWithClaims(projectID, token); ok && claims != nil {
			authUserID = claims.UserID
			authTenantID = claims.TenantID
		}
	}

	// Tenanted endpoint requires an active tenant session; refuse uploads
	// from anonymous or tenant-less callers so an orphaned file can't land
	// under the project files tree.
	if ue.TenantColumn != "" && authTenantID == 0 {
		writeError(w, http.StatusForbidden, "no active tenant; call /tenant/switch first")
		return
	}

	// Parse multipart form.
	if err := r.ParseMultipartForm(maxBytes); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("file too large (max %d MB)", ue.MaxSizeMB))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file field is required (use multipart form with field name 'file')")
		return
	}
	defer func() { _ = file.Close() }()

	// Read file data first so we can sniff the real MIME type from magic bytes
	// rather than trusting the client-supplied Content-Type header. Without
	// this, a request could upload "image.jpg" with Content-Type: text/html
	// and end up with HTML being served as a trusted image from this project's
	// asset path - stored XSS.
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("file too large (max %d MB)", ue.MaxSizeMB))
		return
	}

	allowed := ue.AllowedTypes
	if len(allowed) == 0 {
		allowed = defaultUploadTypes
	}
	ct, typeAllowed := sniffUploadContentType(data, header.Header.Get("Content-Type"), allowed)
	if !typeAllowed {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("file type %q not allowed (accepted: %v)", ct, allowed))
		return
	}

	// Per-project disk quota: refuse the upload if it would push the project's
	// total stored uploads past the cap. Without this an internet visitor can
	// fill the host disk by spamming an upload endpoint.
	var usedBytes sql.NullInt64
	if err := projectDB.Reader().QueryRow(
		"SELECT COALESCE(SUM(size), 0) FROM of_files WHERE kind = 'file' AND storage = 'disk' AND path LIKE 'uploads/%' AND is_deleted = 0",
	).Scan(&usedBytes); err == nil {
		if usedBytes.Int64+int64(len(data)) > endpoints.MaxProjectUploadBytes {
			writeError(w, http.StatusInsufficientStorage, "project upload quota exceeded")
			return
		}
	}

	// Generate unique filename {unixnano}_{rand4}_{sanitized}{ext}: nanosecond keeps
	// names chronologically sortable; the 4-byte crypto/rand hex suffix makes
	// same-nanosecond concurrent-upload collisions astronomically unlikely. Still
	// paired with O_EXCL below so a genuine collision fails loudly instead of silently
	// overwriting the first writer's bytes - which would be a cross-user data-leak.
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		// Try to get extension from content type.
		exts, _ := mime.ExtensionsByType(ct)
		if len(exts) > 0 {
			ext = exts[0]
		}
	}
	baseStem := strings.TrimSuffix(filepath.Base(header.Filename), ext)
	uniqueName := fmt.Sprintf("%d_%s_%s%s", time.Now().UnixNano(), randomHex4(), sanitizeUploadBasename(baseStem), ext)

	// Optional subfolder from form field (e.g. "user_5" or "post_42/images").
	// Sanitized to prevent path traversal.
	subFolder := sanitizeUploadFolder(r.FormValue("folder"))
	dirParts := []string{h.deps.ProjectDBManager.ProjectDir(projectID), "files", "uploads"}
	storageName := "uploads/"
	// Tenant prefix: when the upload endpoint is tenanted, every file lands
	// under uploads/t<tenant_id>/ so guessing a URL across tenants is blocked
	// at the filesystem layer, not just by RLS on the linked table.
	if ue.TenantColumn != "" && authTenantID > 0 {
		tenantSeg := fmt.Sprintf("t%d", authTenantID)
		dirParts = append(dirParts, tenantSeg)
		storageName += tenantSeg + "/"
	}
	if subFolder != "" {
		dirParts = append(dirParts, subFolder)
		storageName += subFolder + "/"
	}
	storageName += uniqueName

	// Write to disk. O_EXCL turns the random-suffixed name into a
	// genuine cross-process write-lock: if a second uploader on a
	// different core happened to derive the same name (vanishingly
	// rare but mathematically possible), the second writer fails
	// instead of silently overwriting the first writer's bytes - a
	// cross-user data leak we'd otherwise never see in logs.
	dir := filepath.Join(dirParts...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "storage error")
		return
	}
	storagePath := filepath.Join(dir, uniqueName)
	if err := writeExclusive(storagePath, data); err != nil {
		// One retry with a fresh random suffix is enough; if even that
		// collides we have bigger problems than this upload.
		if errors.Is(err, os.ErrExist) {
			uniqueName = fmt.Sprintf("%d_%s_%s%s", time.Now().UnixNano(), randomHex4(), sanitizeUploadBasename(baseStem), ext)
			storagePath = filepath.Join(dir, uniqueName)
			// Also refresh storageName so the URL and DB row match.
			storageName = strings.TrimSuffix(storageName, filepath.Base(storageName)) + uniqueName
			if err := writeExclusive(storagePath, data); err != nil {
				writeError(w, http.StatusInternalServerError, "storage error")
				return
			}
		} else {
			writeError(w, http.StatusInternalServerError, "storage error")
			return
		}
	}

	// Insert metadata into files table.
	// Must match what ServeFile reads: kind='file' + storage='disk' so
	// artifactBytes loads the on-disk bytes via disk_path instead of returning
	// an empty inline body.
	_, err = projectDB.Writer().Exec(
		`INSERT INTO of_files (kind, storage, path, content_type, size, disk_path)
		 VALUES ('file', 'disk', ?, ?, ?, ?)
		 ON CONFLICT(kind, path) DO UPDATE SET storage = excluded.storage, content_type = excluded.content_type, size = excluded.size, disk_path = excluded.disk_path, updated_at = CURRENT_TIMESTAMP`,
		storageName, ct, len(data), storagePath,
	)
	if err != nil {
		slog.Error("upload: failed to insert file metadata", "error", err)
		_ = os.Remove(storagePath) // clean up orphaned file
		writeError(w, http.StatusInternalServerError, "storage error")
		return
	}

	// If linked to a table, also insert a row there. The table+column names
	// come from of_upload_endpoints, which the AI builder controls via the
	// uploads tool, so they're trusted at the policy layer - but defence
	// in depth: re-validate the grammar AND quote the identifier on the way
	// into raw SQL, matching the same pattern security/admin handlers use
	// (see security.ValidateTableName / QuoteIdent).
	fileURL := "/files/" + storageName
	var inserted []map[string]any
	if ue.TableName != "" {
		if err := security.ValidateTableName(ue.TableName); err != nil {
			// Bad table name in config - clean up the file we just wrote
			// and tell the caller the upload failed. Leaving the file on
			// disk would orphan it forever (no UI surfaces orphans).
			cleanupOrphanedUpload(projectDB.Writer(), storagePath, storageName, "invalid linked table name", ue.TableName, err)
			writeError(w, http.StatusInternalServerError, "upload endpoint misconfigured")
			return
		}
		cols := []string{"filename", "url", "content_type", "size"}
		vals := []any{header.Filename, fileURL, ct, len(data)}
		if ue.OwnerColumn != "" && authUserID > 0 {
			if err := security.ValidateColumnName(ue.OwnerColumn); err == nil {
				cols = append(cols, ue.OwnerColumn)
				vals = append(vals, authUserID)
			}
		}
		if ue.TenantColumn != "" && authTenantID > 0 {
			if err := security.ValidateColumnName(ue.TenantColumn); err == nil {
				cols = append(cols, ue.TenantColumn)
				vals = append(vals, authTenantID)
			}
		}
		// Quote every identifier. The hardcoded ones (filename/url/...) can't
		// collide with keywords today, but quoting them is free insurance
		// against the day someone adds a column called "order" or "user".
		quotedCols := make([]string, len(cols))
		for i, c := range cols {
			quotedCols[i] = security.QuoteIdent(c)
		}
		placeholders := strings.Repeat("?,", len(cols))
		placeholders = placeholders[:len(placeholders)-1]
		// RETURNING hands back the row as stored, with its id and every column the
		// table generates (a slug, an expiry): a page that shares a link to the upload
		// needs those, and has no other way to learn them.
		insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING *",
			security.QuoteIdent(ue.TableName), strings.Join(quotedCols, ", "), placeholders)
		rows, err := projectDB.Writer().Query(insertSQL, vals...)
		if err == nil {
			inserted = h.scanRowsToMaps(rows)
			if len(inserted) == 0 {
				err = fmt.Errorf("insert returned no row")
			}
		}
		if err != nil {
			// Fail loudly when the linked-table insert fails: the file and
			// artifact row would otherwise be orphaned and invisible to the
			// user-facing app. Clean both up before returning.
			cleanupOrphanedUpload(projectDB.Writer(), storagePath, storageName, "linked table insert failed", ue.TableName, err)
			writeError(w, http.StatusInternalServerError, "failed to record upload in linked table")
			return
		}
		// The row goes back to the uploader, so a PASSWORD/ENCRYPTED column never
		// leaves with it; a lookup that fails sends no row rather than an unstripped one.
		if secureCols, serr := store.LoadSecureColumns(projectDB.Reader(), ue.TableName); serr == nil {
			stripSecureCols(inserted, secureCols)
		} else {
			inserted = nil
		}
	}

	resp := map[string]any{
		"url":      fileURL,
		"filename": header.Filename,
		"size":     len(data),
		"type":     ct,
	}
	if len(inserted) > 0 {
		resp["row"] = inserted[0]
	}
	writeJSON(w, http.StatusCreated, resp)
}

// defaultUploadTypes is the accepted content-type allow-list applied when an
// upload endpoint declares no explicit AllowedTypes.
var defaultUploadTypes = []string{"image/*", "application/pdf", "text/*", "application/json", "application/zip"}

// sniffUploadContentType determines an upload's content type from its magic
// bytes, never trusting the client-supplied header for the security decision,
// since "image.jpg" sent as text/html would otherwise be served as a trusted
// image and become stored XSS - and reports whether it matches the allow-list.
// http.DetectContentType yields application/octet-stream for bytes it can't
// recognise (legitimate for many binary formats); only then do we fall back to
// the client's Content-Type hint.
func sniffUploadContentType(data []byte, clientCT string, allowed []string) (ct string, ok bool) {
	sniffed := http.DetectContentType(data)
	ct = sniffed
	sniffedBase := strings.TrimSpace(strings.SplitN(sniffed, ";", 2)[0])
	if sniffedBase == "application/octet-stream" && clientCT != "" {
		ct = clientCT
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	for _, pattern := range allowed {
		if mimeTypeMatches(ct, pattern) {
			return ct, true
		}
	}
	return ct, false
}

// writeExclusive writes data to path with O_CREATE|O_EXCL|O_WRONLY, so a
// pre-existing file at path causes a deterministic os.ErrExist error
// instead of silently overwriting. Callers branch on errors.Is(err,
// os.ErrExist) to retry with a fresh name.
func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, werr := f.Write(data); werr != nil {
		_ = f.Close()
		_ = os.Remove(path) // partial write isn't useful to anyone
		return werr
	}
	return f.Close()
}

// randomHex4 returns 8 hex characters from 4 bytes of crypto-random.
// Used to disambiguate same-nanosecond uploads without making the filename
// uncomfortably long. crypto/rand never returns an error on real systems,
// but if it does we fall back to a nanosecond-derived value rather than
// panicking the upload path.
func randomHex4() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

// cleanupOrphanedUpload best-effort removes a half-committed upload after a
// downstream error. The on-disk file and the of_files row would otherwise
// linger forever (no UI lists orphans), so leaving them around is a slow
// disk leak plus a confusing "I see the file but the app doesn't" UX.
// Errors during cleanup are logged but never bubbled - the caller is
// already reporting a primary failure and we don't want to mask it.
func cleanupOrphanedUpload(writer *sql.DB, diskPath, filePath, reason, tableName string, primary error) {
	slog.Warn("upload: rolling back",
		"reason", reason,
		"table", tableName,
		"disk_path", diskPath,
		"file_path", filePath,
		"error", primary,
	)
	if err := os.Remove(diskPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("upload: cleanup remove failed", "path", diskPath, "error", err)
	}
	if _, err := writer.Exec(
		`DELETE FROM of_files WHERE kind = 'file' AND path = ?`,
		filePath,
	); err != nil {
		slog.Warn("upload: cleanup artifact row delete failed", "path", filePath, "error", err)
	}
}

// sanitizeUploadFolder cleans a user-provided subfolder path for safe storage.
// Allows forward-slash-separated subdirectories but rejects path traversal.
func sanitizeUploadFolder(folder string) string {
	if folder == "" {
		return ""
	}
	folder = strings.ReplaceAll(folder, "\\", "/")
	parts := strings.Split(folder, "/")
	var clean []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "." || p == ".." {
			continue
		}
		clean = append(clean, p)
	}
	return strings.Join(clean, "/")
}

// SSE Stream handler
