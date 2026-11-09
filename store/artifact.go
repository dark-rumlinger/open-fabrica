package store

// ArtifactStore is the single DB access layer for every file-like artifact in the
// project. "Artifact" is the umbrella (one of_files row discriminated by `kind`);
// "file" is one of the four kinds (page, layout, component, file).
//
// Storage is explicit per row, picked once at save time: "inline" -> bytes in
// body, "disk" -> bytes at DiskPath. Readers branch on `storage`; there is no
// "try body, fall back to disk". Page/layout/component are always inline.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"openfabrica/internal/dbh"
	"openfabrica/internal/diskpath"
	"openfabrica/internal/sqliteretry"
)

// StorageMode is the authoritative switch for where an artifact's bytes live.
type StorageMode string

const (
	StorageInline StorageMode = "inline" // bytes in Artifact.Body
	StorageDisk   StorageMode = "disk"   // bytes at Artifact.DiskPath
)

// MaxSingleReadBody is the largest artifact body, in bytes, delivered in a single
// shot - shared by the pre-inline per-file cap and one manage_files `get` page.
// They MUST stay coupled so a file that fits the inline cap is delivered whole;
// manage_files' MaxResult must sit above this so the get envelope is never
// truncated mid-body.
const MaxSingleReadBody = 24576

// MaxSingleSaveBody is the largest body, in bytes, a single manage_files `save`
// accepts: a body past this risks the model truncating mid-write, shipping a
// corrupt artifact. 2x MaxSingleReadBody; larger bodies are refused with a guide
// to write smaller and extend with patch.
const MaxSingleSaveBody = 2 * MaxSingleReadBody

// MaxClientModuleBody is the byte ceiling for a single full-body save of a client
// JS module (the module_too_large_client gate), 1.5x MaxSingleReadBody. The
// ladder: past ~24 KB a module is NUDGED to split, past 36 KB a one-shot client-JS
// save is REFUSED so a forgotten import can't hide below an oversized blob, and
// past ~48 KB any body is hard-stopped as oversized_body.
const MaxClientModuleBody = 3 * MaxSingleReadBody / 2

// HumanSize renders a byte count as a compact human-readable size for LLM-facing
// prose, so every ceiling quoted to the model derives from the constants above and
// cannot silently drift. Exact KB multiples drop the fraction; sub-KB renders in bytes.
func HumanSize(bytes int) string {
	const kb = 1024
	if bytes < kb {
		return fmt.Sprintf("%d bytes", bytes)
	}
	if bytes%kb == 0 {
		return fmt.Sprintf("%d KB", bytes/kb)
	}
	return fmt.Sprintf("%.1f KB", float64(bytes)/float64(kb))
}

// Artifact mirrors one row in of_files.
type Artifact struct {
	ID          int64
	Kind        string
	Path        string
	Storage     StorageMode
	Body        string
	DiskPath    string
	ContentType string
	Metadata    map[string]any
	Scope       string
	Size        int
	Status      string
	IsDeleted   bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// isJSFilePath reports whether the path is a JS file the renderer would
// inject as <script>. Mirrors the suffix check in render.go.
func isJSFilePath(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".js") || strings.HasSuffix(lower, ".mjs")
}

// isPageScopeCodePath reports whether the path is a local JS/CSS code file that
// defaults to scope="page" (a single-page split) rather than "global". Global is
// reserved for genuinely sitewide assets (svg/img/font/json), marked explicitly by
// the model. Single source for the JS (.js/.mjs) + CSS (.css) test.
func isPageScopeCodePath(path string) bool {
	return isJSFilePath(path) || strings.HasSuffix(strings.ToLower(path), ".css")
}

// isNewlineNormalizableArtifact reports whether an artifact body is text we can
// safely normalize to LF on store. Layouts/pages/components are always text; for
// kind="file" we restrict to known text extensions so inline binary blobs are untouched.
func isNewlineNormalizableArtifact(kind, path string) bool {
	switch kind {
	case "layout", "page", "component":
		return true
	case "file":
		lower := strings.ToLower(path)
		for _, ext := range []string{".js", ".mjs", ".cjs", ".ts", ".jsx", ".tsx", ".css", ".html", ".htm", ".json", ".svg", ".md", ".txt", ".xml", ".csv", ".yml", ".yaml"} {
			if strings.HasSuffix(lower, ext) {
				return true
			}
		}
	}
	return false
}

// MetadataJSON serializes Metadata to a JSON string, defaulting to "{}".
func (a *Artifact) MetadataJSON() string {
	if a.Metadata == nil {
		return "{}"
	}
	b, err := json.Marshal(a.Metadata)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// DefaultStorageFor picks the storage mode when the caller hasn't set one.
func DefaultStorageFor(kind, contentType string) StorageMode {
	switch kind {
	case "page", "layout", "component":
		return StorageInline
	}
	ct := strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(ct, "text/"),
		strings.HasPrefix(ct, "application/javascript"),
		strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "image/svg+xml"):
		return StorageInline
	}
	return StorageDisk
}

// Save-time content gates (validateLayoutBody, validatePageBody,
// validateNoCrossOriginAssets, validateNoDuplicatePlanImports,
// validateInlineHandlerScope, ValidateFilePath, validatePagePath, ...)
// live in artifact_content_gates.go.

// MaxBodySnapshots is the number of most-recent body snapshots retained per artifact.
// Older history rows keep their attribution but release their blob reference, so
// restore/diff reaches back this many versions while storage stays bounded.
const MaxBodySnapshots = 20

// bestEffortExec runs a fire-and-forget accounting write (blob refcount, file history,
// asset links) with busy-retry. These must never fail the caller's save, but a
// persistent failure is logged so refcount/history drift becomes observable.
func bestEffortExec(db dbh.Handle, op, query string, args ...any) {
	if _, err := sqliteretry.ExecHandle(db, query, args...); err != nil {
		slog.Warn("best-effort db write failed", "op", op, "err", err)
	}
}

// putBlob stores a body in the content-addressed blob table, keyed by the hash of its
// bytes, and returns the hash. Identical content is inserted once; each referencing
// history row bumps the refcount so orphaned blobs can be reclaimed. Best-effort: a
// snapshot failure must never fail the save.
func putBlob(db dbh.Handle, body string) string {
	sum := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(sum[:])
	bestEffortExec(db, "blob insert", `INSERT INTO of_snapshot_blobs (hash, body, size, refcount)
		VALUES (?, ?, ?, 0) ON CONFLICT(hash) DO NOTHING`, hash, body, len(body))
	bestEffortExec(db, "blob refcount bump", `UPDATE of_snapshot_blobs SET refcount = refcount + 1 WHERE hash = ?`, hash)
	return hash
}

// ArtifactSave upserts an artifact and appends a history row. Resolves the
// storage mode from Artifact.Storage (if set) or content_type heuristics.
// Returns the artifact ID and the new history version_number.
func ArtifactSave(db dbh.Handle, a *Artifact, changedBy string) (int64, int, error) {
	if a.Kind == "" || a.Path == "" {
		return 0, 0, fmt.Errorf("artifact kind and path are required")
	}
	if err := ValidateFilePath(a.Kind, a.Path); err != nil {
		return 0, 0, err
	}
	// Normalize line endings to LF on store so the corpus is uniformly LF: a patch
	// authored with CRLF never diverges from the stored body, and search/replace
	// anchors land on the exact layer instead of missing on a \r and triggering a
	// re-read loop. Text artifacts only; bodies containing a NUL byte are left as is.
	if a.Body != "" && isNewlineNormalizableArtifact(a.Kind, a.Path) &&
		strings.ContainsRune(a.Body, '\r') && !strings.ContainsRune(a.Body, 0) {
		a.Body = strings.ReplaceAll(a.Body, "\r\n", "\n")
		a.Body = strings.ReplaceAll(a.Body, "\r", "\n")
	}
	if a.Kind == "layout" {
		if err := validateLayoutBody(a.Body); err != nil {
			return 0, 0, err
		}
		if err := validateNoCrossOriginAssets(a.Body, "layout"); err != nil {
			return 0, 0, err
		}
		if err := validateNoSameOriginAssetTags(a.Body, "layout"); err != nil {
			return 0, 0, err
		}
		if err := validateLayoutInlineCSSBudget(a.Body); err != nil {
			return 0, 0, err
		}
		if err := validateLayoutLateGlobals(db, a.Body); err != nil {
			return 0, 0, err
		}
		if err := validateNoBlockedIframeDocAccess(a.Body, "layout"); err != nil {
			return 0, 0, err
		}
	}
	if a.Kind == "page" {
		if err := validatePagePath(a.Path); err != nil {
			return 0, 0, err
		}
		if err := validatePageBody(a.Body); err != nil {
			return 0, 0, err
		}
		if err := validateNoCrossOriginAssets(a.Body, "page"); err != nil {
			return 0, 0, err
		}
		if err := validateNoSameOriginAssetTags(a.Body, "page"); err != nil {
			return 0, 0, err
		}
		if err := validateNoBlockedIframeDocAccess(a.Body, "page"); err != nil {
			return 0, 0, err
		}
	}
	if a.Kind == "file" && isJSFilePath(a.Path) {
		if err := validateNoBlockedIframeDocAccess(a.Body, "script"); err != nil {
			return 0, 0, err
		}
		if err := validateNoCrossOriginScriptInjection(a.Body, "script"); err != nil {
			return 0, 0, err
		}
	}
	// A component body is HTML inlined into a page at render time, so a hand-written
	// cross-origin <script src>/<link> in one bypasses the page/layout gate above and
	// reaches the browser unregistered. Same cross-origin steer, one home.
	if a.Kind == "component" {
		if err := validateNoCrossOriginAssets(a.Body, "component"); err != nil {
			return 0, 0, err
		}
		if err := validateNoSameOriginAssetTags(a.Body, "component"); err != nil {
			return 0, 0, err
		}
	}
	if a.Kind == "page" || a.Kind == "layout" || a.Kind == "component" {
		if err := validateInlineHandlerScope(a.Body); err != nil {
			return 0, 0, err
		}
	}
	if err := validateAuthBootstrapUsage(a); err != nil {
		return 0, 0, err
	}
	// Reject `data:` URL bodies. The LLM occasionally tries to "inline" an SVG/image
	// as a data: URL, which saves but produces a file whose contents are the URL
	// string (browsers don't render that). Hard-fail with a hint instead of writing
	// the wrong bytes.
	if a.Body != "" && strings.HasPrefix(strings.TrimLeft(a.Body, " \t\r\n"), "data:") {
		return 0, 0, fmt.Errorf(
			"body looks like a data: URL, pass the raw file content instead " +
				"(e.g. for SVG, the literal <svg…>…</svg> markup with no \"data:image/svg+xml,\" prefix)")
	}
	if a.Metadata == nil {
		a.Metadata = map[string]any{}
	}
	// Read the prior row's id and scope if an active one exists. id attributes the
	// history row to the existing artifact; scope is read before defaulting below so a
	// plain re-save can inherit it. The outgoing body needs no capture here (the
	// history row snapshots the body being saved, which ArtifactRestore reads back).
	var priorID int64
	var priorScope string
	row := db.QueryRow(`SELECT id, scope
		FROM of_files WHERE kind = ? AND path = ? AND is_deleted = 0`, a.Kind, a.Path)
	hadPrior := row.Scan(&priorID, &priorScope) == nil

	// Resolve scope: inherit-then-infer. Explicit caller scope wins; else inherit
	// the prior save's scope, so a plain re-save never flips a linked page asset
	// back to global; else infer page for local JS/CSS code files, global otherwise.
	if a.Scope == "" {
		switch {
		case hadPrior && priorScope != "":
			a.Scope = priorScope
		case a.Kind == "file" && isPageScopeCodePath(a.Path):
			a.Scope = "page"
		default:
			a.Scope = "global"
		}
	}
	if a.Status == "" {
		a.Status = "published"
	}
	// CSS-bearing artifacts: reject @import URLs that duplicate plan.external_libraries.
	// Layouts/pages may carry inline <style>; standalone .css artifacts carry pure CSS.
	if a.Kind == "layout" || a.Kind == "page" || (a.Kind == "file" && strings.HasSuffix(strings.ToLower(a.Path), ".css")) {
		if err := validateNoDuplicatePlanImports(db, a.Body, a.Kind); err != nil {
			return 0, 0, err
		}
	}
	if a.Storage == "" {
		a.Storage = DefaultStorageFor(a.Kind, a.ContentType)
	}
	// A disk artifact with no disk_path would have its body cleared below and read back
	// empty. A text artifact is stored inline instead; anything else is refused rather
	// than reported as saved.
	if a.Storage == StorageDisk && a.DiskPath == "" && a.Body != "" {
		if !isNewlineNormalizableArtifact(a.Kind, a.Path) {
			return 0, 0, fmt.Errorf("cannot store a body for %s %q (content type %q): that type is kept on disk, which this save has no file for. Save text files with a text extension (.js .css .json .svg .md .txt ...)", a.Kind, a.Path, a.ContentType)
		}
		a.Storage = StorageInline
	}
	// Enforce the contract: body populated for inline, disk_path populated for disk.
	switch a.Storage {
	case StorageInline:
		a.DiskPath = ""
		if a.Size == 0 {
			a.Size = len(a.Body)
		}
	case StorageDisk:
		a.Body = ""
	default:
		return 0, 0, fmt.Errorf("invalid storage mode %q", a.Storage)
	}
	metaJSON := a.MetadataJSON()

	_, err := db.Exec(`INSERT INTO of_files
		(kind, path, storage, body, disk_path, content_type, metadata, scope, size, status, is_deleted, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
		ON CONFLICT(kind, path) DO UPDATE SET
			storage = excluded.storage,
			body = excluded.body,
			disk_path = excluded.disk_path,
			content_type = excluded.content_type,
			metadata = excluded.metadata,
			scope = excluded.scope,
			size = excluded.size,
			status = excluded.status,
			is_deleted = 0,
			deleted_at = NULL,
			updated_at = CURRENT_TIMESTAMP`,
		a.Kind, a.Path, string(a.Storage), a.Body, a.DiskPath, a.ContentType, metaJSON, a.Scope, a.Size, a.Status)
	if err != nil {
		return 0, 0, fmt.Errorf("saving artifact: %w", err)
	}

	var id int64
	_ = db.QueryRow(`SELECT id FROM of_files WHERE kind = ? AND path = ?`, a.Kind, a.Path).Scan(&id)
	a.ID = id

	var nextVersion int
	_ = db.QueryRow(`SELECT COALESCE(MAX(version_number), 0) + 1 FROM of_file_history WHERE kind = ? AND path = ?`,
		a.Kind, a.Path).Scan(&nextVersion)
	if changedBy == "" {
		changedBy = "brain"
	}
	historyFileID := id
	if hadPrior {
		historyFileID = priorID
	}
	// Snapshot the live body so this version can be restored or diffed later.
	// Inline text only; disk artifacts keep attribution-only history (their bytes
	// already live on the filesystem), so body_hash stays empty for them.
	var bodyHash string
	if a.Storage == StorageInline && a.Body != "" {
		bodyHash = putBlob(db, a.Body)
	}
	bestEffortExec(db, "file history insert", `INSERT INTO of_file_history
		(file_id, kind, path, version_number, changed_by, body_hash)
		VALUES (?, ?, ?, ?, ?, ?)`,
		historyFileID, a.Kind, a.Path, nextVersion, changedBy, bodyHash)
	// Keep only the most-recent snapshots for this artifact; older rows retain
	// attribution but release their blob reference.
	pruneBodySnapshots(db, a.Kind, a.Path, MaxBodySnapshots)

	// Refresh the symbol index for this artifact so resolve/where_used can point
	// the model straight at file:line without a body read. Text inline artifacts
	// only (disk blobs carry no body here); best-effort, an index failure must
	// never fail the save.
	if a.Storage == StorageInline && isNewlineNormalizableArtifact(a.Kind, a.Path) {
		if err := IndexArtifactSymbols(db, a.Kind, a.Path, a.Body, a.ContentType); err != nil {
			slog.Warn("symbol index update failed", "kind", a.Kind, "path", a.Path, "err", err)
		}
	}

	return id, nextVersion, nil
}

// ArtifactGet loads a single artifact by (kind, path). Returns nil on
// not-found (no error).
func ArtifactGet(db dbh.Handle, kind, path string) (*Artifact, error) {
	rows, err := db.Query("SELECT "+artifactColsBody+" FROM of_files WHERE kind = ? AND path = ? AND is_deleted = 0",
		kind, path)
	if err != nil {
		return nil, fmt.Errorf("loading artifact: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("loading artifact: %w", err)
		}
		return nil, nil
	}
	a, err := scanArtifact(rows, true)
	if err != nil {
		return nil, fmt.Errorf("loading artifact: %w", err)
	}
	return a, nil
}

// ArtifactCurrentVersion returns the live version number of an artifact, exactly
// the value the last ArtifactSave returned - used to stamp a read so a stale
// line-number edit can be refused. Returns 0 for an artifact with no history.
func ArtifactCurrentVersion(db dbh.Handle, kind, path string) int {
	var v int
	_ = db.QueryRow(`SELECT COALESCE(MAX(version_number), 0) FROM of_file_history WHERE kind = ? AND path = ?`,
		kind, path).Scan(&v)
	return v
}

// ArtifactRestore reverts an artifact to the body captured at the given version,
// written back through ArtifactSave so the restore is itself a new attributed
// version. Fails if the version has no snapshot or its blob has been pruned.
func ArtifactRestore(db dbh.Handle, kind, path string, version int, changedBy string) (int, error) {
	var bodyHash string
	err := db.QueryRow(`SELECT body_hash FROM of_file_history
		WHERE kind = ? AND path = ? AND version_number = ?`, kind, path, version).Scan(&bodyHash)
	if err != nil {
		return 0, fmt.Errorf("version %d not found for %s %s", version, kind, path)
	}
	if bodyHash == "" {
		return 0, fmt.Errorf("version %d has no restorable snapshot", version)
	}
	var body string
	if err := db.QueryRow(`SELECT body FROM of_snapshot_blobs WHERE hash = ?`, bodyHash).Scan(&body); err != nil {
		return 0, fmt.Errorf("snapshot for version %d is no longer retained", version)
	}
	// Reuse the current artifact's metadata/content_type/scope and swap only the
	// body. If the artifact was deleted, save re-creates it from a minimal row.
	a, err := ArtifactGet(db, kind, path)
	if err != nil {
		return 0, err
	}
	if a == nil {
		// Deleted: the tombstone row still carries metadata, content type and scope.
		if rows, qerr := db.Query("SELECT "+artifactColsBody+" FROM of_files WHERE kind = ? AND path = ? AND is_deleted = 1", kind, path); qerr == nil {
			if rows.Next() {
				a, _ = scanArtifact(rows, true)
			}
			rows.Close()
		}
	}
	if a == nil {
		a = &Artifact{Kind: kind, Path: path}
	}
	a.Body = body
	a.Storage = StorageInline
	a.Size = len(body)
	_, newVersion, err := ArtifactSave(db, a, changedBy)
	return newVersion, err
}

// ArtifactPreviousBody returns the body of the most recent retained snapshot
// below the current live version, or "" when none is kept. Diagnostic use: a
// failed patch checks it to tell the model its anchor matches an OLDER version
// of the file rather than a hallucination.
func ArtifactPreviousBody(db dbh.Handle, kind, path string) string {
	v := ArtifactRestorableVersion(db, kind, path)
	if v == 0 {
		return ""
	}
	var bodyHash string
	if err := db.QueryRow(`SELECT body_hash FROM of_file_history
		WHERE kind = ? AND path = ? AND version_number = ?`, kind, path, v).Scan(&bodyHash); err != nil || bodyHash == "" {
		return ""
	}
	var body string
	if err := db.QueryRow(`SELECT body FROM of_snapshot_blobs WHERE hash = ?`, bodyHash).Scan(&body); err != nil {
		return ""
	}
	return body
}

// ArtifactRestorableVersion returns the version number of the most recent body
// snapshot that predates the current live version - the target of a plain "undo" when
// no explicit version is given. Delete-marker rows (no snapshot) are skipped, so this
// reaches the last real body even after a delete. Returns 0 when none is retained.
func ArtifactRestorableVersion(db dbh.Handle, kind, path string) int {
	current := ArtifactCurrentVersion(db, kind, path)
	var v int
	_ = db.QueryRow(`SELECT COALESCE(MAX(version_number), 0) FROM of_file_history
		WHERE kind = ? AND path = ? AND body_hash <> '' AND version_number < ?`,
		kind, path, current).Scan(&v)
	return v
}

// pruneBodySnapshots keeps only the newest keepN snapshotted versions of an artifact.
// Older history rows are stripped of their body_hash (attribution preserved) and the
// referenced blobs have their refcount decremented; any blob reaching zero references
// is deleted. Best-effort: pruning must never fail a save.
func pruneBodySnapshots(db dbh.Handle, kind, path string, keepN int) {
	rows, err := db.Query(`SELECT id, body_hash FROM of_file_history
		WHERE kind = ? AND path = ? AND body_hash <> ''
		ORDER BY version_number DESC LIMIT -1 OFFSET ?`, kind, path, keepN)
	if err != nil {
		return
	}
	type stale struct {
		id   int64
		hash string
	}
	var drop []stale
	for rows.Next() {
		var s stale
		if rows.Scan(&s.id, &s.hash) == nil {
			drop = append(drop, s)
		}
	}
	_ = rows.Err() // best-effort prune; a late iteration error just leaves extra history for the next save
	rows.Close()
	for _, s := range drop {
		bestEffortExec(db, "history blob unlink", `UPDATE of_file_history SET body_hash = '' WHERE id = ?`, s.id)
		bestEffortExec(db, "blob refcount decrement", `UPDATE of_snapshot_blobs SET refcount = refcount - 1 WHERE hash = ?`, s.hash)
	}
	if len(drop) > 0 {
		bestEffortExec(db, "orphan blob prune", `DELETE FROM of_snapshot_blobs WHERE refcount <= 0`)
	}
}

// ArtifactKindsForPath returns the kinds of every live artifact sharing the given
// path, sorted, letting read handlers infer kind from path and demand it only when
// ambiguous. Returns an empty slice (no error) when nothing lives at the path.
func ArtifactKindsForPath(db dbh.Handle, path string) ([]string, error) {
	rows, err := db.Query(
		`SELECT DISTINCT kind FROM of_files WHERE path = ? AND is_deleted = 0 ORDER BY kind`,
		path,
	)
	if err != nil {
		return nil, fmt.Errorf("resolving kinds for path %q: %w", path, err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			continue
		}
		kinds = append(kinds, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolving kinds for path %q: %w", path, err)
	}
	return kinds, nil
}

// CanonicalFilePath returns the bare of_files storage path for a kind=file
// reference, stripping the "/files/" URL prefix (and a stray leading slash). The
// "files/" strip requires the trailing slash, so a file named "files.js" is left
// intact. Apply only to file references; page/layout/component paths legitimately
// start with "/".
func CanonicalFilePath(path string) string {
	p := strings.TrimPrefix(path, "/")
	p = strings.TrimPrefix(p, "files/")
	return p
}

// ArtifactListOpts filters ArtifactList results. Empty fields are ignored.
type ArtifactListOpts struct {
	Kind           string
	Kinds          []string
	Scope          string
	PathPrefix     string
	PathContains   string
	IncludeBody    bool
	IncludeDeleted bool
	Limit          int
}

// ReadArtifactBytes is the strict, security-checked reader for the HTTP file
// surfaces (public and admin): storage is authoritative ("inline"/"" -> body,
// "disk" -> disk_path) with no fallback, and a disk_path must stay inside
// projectDir. It is the one home for that containment check so it cannot drift or
// be forgotten on one surface.
func ReadArtifactBytes(storage, body, diskPath sql.NullString, projectDir string) ([]byte, error) {
	switch storage.String {
	case "inline", "":
		return []byte(body.String), nil
	case "disk":
		if diskPath.String == "" {
			return nil, fmt.Errorf("artifact has storage=disk but disk_path is empty")
		}
		resolved, ok := diskpath.Resolve(projectDir, diskPath.String)
		if !ok {
			return nil, fmt.Errorf("disk_path escapes project directory")
		}
		return os.ReadFile(resolved)
	}
	return nil, fmt.Errorf("unknown storage mode %q", storage.String)
}

// artifactColsBody / artifactColsNoBody are the one pair of column lists every
// artifact read selects, and scanArtifact is their scanning twin - Get and List share
// them so a column added to one read can never silently drift from the others.
const (
	artifactColsNoBody = "id, kind, path, storage, disk_path, content_type, metadata, scope, size, status, is_deleted, created_at, updated_at"
	artifactColsBody   = "id, kind, path, storage, body, disk_path, content_type, metadata, scope, size, status, is_deleted, created_at, updated_at"
)

// scanArtifact scans the current row - selected via artifactColsBody or
// artifactColsNoBody per includeBody - into an Artifact.
func scanArtifact(rows *sql.Rows, includeBody bool) (*Artifact, error) {
	a := &Artifact{}
	var metadata, storage string
	var err error
	if includeBody {
		err = rows.Scan(&a.ID, &a.Kind, &a.Path, &storage, &a.Body, &a.DiskPath, &a.ContentType, &metadata, &a.Scope, &a.Size, &a.Status, &a.IsDeleted, &a.CreatedAt, &a.UpdatedAt)
	} else {
		err = rows.Scan(&a.ID, &a.Kind, &a.Path, &storage, &a.DiskPath, &a.ContentType, &metadata, &a.Scope, &a.Size, &a.Status, &a.IsDeleted, &a.CreatedAt, &a.UpdatedAt)
	}
	if err != nil {
		return nil, err
	}
	a.Storage = StorageMode(storage)
	if metadata != "" {
		if err := json.Unmarshal([]byte(metadata), &a.Metadata); err != nil {
			slog.Debug("artifact metadata unmarshal failed", "kind", a.Kind, "path", a.Path, "err", err)
		}
	}
	return a, nil
}

// ArtifactList returns artifacts matching the given filters.
func ArtifactList(db dbh.Handle, opts ArtifactListOpts) ([]*Artifact, error) {
	cols := artifactColsNoBody
	if opts.IncludeBody {
		cols = artifactColsBody
	}
	q := "SELECT " + cols + " FROM of_files WHERE 1=1"
	var args []any
	if opts.Kind != "" {
		q += " AND kind = ?"
		args = append(args, opts.Kind)
	}
	if len(opts.Kinds) > 0 {
		placeholders := strings.Repeat("?,", len(opts.Kinds))
		placeholders = placeholders[:len(placeholders)-1]
		q += " AND kind IN (" + placeholders + ")"
		for _, k := range opts.Kinds {
			args = append(args, k)
		}
	}
	if opts.Scope != "" {
		q += " AND scope = ?"
		args = append(args, opts.Scope)
	}
	if opts.PathPrefix != "" {
		q += " AND path LIKE ?"
		args = append(args, opts.PathPrefix+"%")
	}
	if opts.PathContains != "" {
		q += " AND path LIKE ?"
		args = append(args, "%"+opts.PathContains+"%")
	}
	if !opts.IncludeDeleted {
		q += " AND is_deleted = 0"
	}
	q += " ORDER BY kind, path"
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", opts.Limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("listing artifacts: %w", err)
	}
	defer rows.Close()

	var out []*Artifact
	for rows.Next() {
		a, err := scanArtifact(rows, opts.IncludeBody)
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing artifacts: %w", err)
	}
	return out, nil
}

// ArtifactDelete soft-deletes an artifact and records the deletion in history.
func ArtifactDelete(db dbh.Handle, kind, path, changedBy string) (bool, error) {
	a, err := ArtifactGet(db, kind, path)
	if err != nil {
		return false, err
	}
	if a == nil {
		return false, nil
	}

	var nextVersion int
	_ = db.QueryRow(`SELECT COALESCE(MAX(version_number), 0) + 1 FROM of_file_history WHERE kind = ? AND path = ?`,
		kind, path).Scan(&nextVersion)
	if changedBy == "" {
		changedBy = "brain"
	}
	bestEffortExec(db, "file history insert", `INSERT INTO of_file_history
		(file_id, kind, path, version_number, changed_by)
		VALUES (?, ?, ?, ?, ?)`,
		a.ID, kind, path, nextVersion, changedBy)

	_, err = db.Exec(`UPDATE of_files SET is_deleted = 1, deleted_at = CURRENT_TIMESTAMP WHERE kind = ? AND path = ?`,
		kind, path)
	if err != nil {
		return false, fmt.Errorf("deleting artifact: %w", err)
	}
	// Drop the asset links the artifact is part of: a deleted page's attachments are
	// meaningless, and a deleted file left attached keeps every page it was on loading
	// a URL that now 404s (a load failure on each of those pages).
	switch kind {
	case "page":
		bestEffortExec(db, "page asset cleanup", `DELETE FROM of_page_assets WHERE page_path = ?`, path)
	case "file":
		bestEffortExec(db, "page asset cleanup", `DELETE FROM of_page_assets WHERE file_path = ?`, path)
	}
	// Drop the artifact's symbol-index rows; best-effort.
	if err := RemoveArtifactSymbols(db, kind, path); err != nil {
		slog.Warn("symbol index removal failed", "kind", kind, "path", path, "err", err)
	}
	return true, nil
}

// IsPlatformManagedArtifact reports whether the stored artifact at (kind, path) is
// platform-owned and must not be deleted or overwritten by the model. Fires on the
// metadata.platform_managed flag, so any platform emitter that sets it is protected.
func IsPlatformManagedArtifact(db dbh.Handle, kind, path string) bool {
	a, err := ArtifactGet(db, kind, path)
	if err != nil || a == nil {
		return false
	}
	pm, _ := a.Metadata["platform_managed"].(bool)
	return pm
}

// ArtifactRename moves an artifact from (kind, oldPath) to (kind, newPath).
func ArtifactRename(db dbh.Handle, kind, oldPath, newPath string) error {
	if oldPath == newPath {
		return nil
	}
	var count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM of_files WHERE kind = ? AND path = ? AND is_deleted = 0`, kind, newPath).Scan(&count)
	if count > 0 {
		return fmt.Errorf("artifact %s %s already exists", kind, newPath)
	}
	// A deleted artifact at the target keeps its row (the unique key covers
	// tombstones), so clear it and its history before the move.
	pruneBodySnapshots(db, kind, newPath, 0)
	bestEffortExec(db, "tombstone history clear", `DELETE FROM of_file_history WHERE kind = ? AND path = ?`, kind, newPath)
	if _, err := db.Exec(`DELETE FROM of_files WHERE kind = ? AND path = ? AND is_deleted = 1`, kind, newPath); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE of_files SET path = ?, updated_at = CURRENT_TIMESTAMP WHERE kind = ? AND path = ? AND is_deleted = 0`,
		newPath, kind, oldPath)
	if err != nil {
		return err
	}
	// History follows the artifact so its version counter and restore keep working.
	bestEffortExec(db, "history move", `UPDATE of_file_history SET path = ? WHERE kind = ? AND path = ?`, newPath, kind, oldPath)
	// Keep page-asset link table pointing at the renamed artifact.
	if kind == "file" {
		bestEffortExec(db, "page asset relink", `UPDATE of_page_assets SET file_path = ? WHERE file_path = ?`, newPath, oldPath)
	}
	if kind == "page" {
		bestEffortExec(db, "page asset relink", `UPDATE of_page_assets SET page_path = ? WHERE page_path = ?`, newPath, oldPath)
	}
	// Move the artifact's symbols to the new path: drop the old rows, re-extract
	// under the new path (its line numbers and selectors are unchanged but the
	// owning path is). Best-effort.
	if err := RemoveArtifactSymbols(db, kind, oldPath); err != nil {
		slog.Warn("symbol index removal failed", "kind", kind, "path", oldPath, "err", err)
	}
	if a, _ := ArtifactGet(db, kind, newPath); a != nil &&
		a.Storage == StorageInline && isNewlineNormalizableArtifact(kind, newPath) {
		if err := IndexArtifactSymbols(db, kind, newPath, a.Body, a.ContentType); err != nil {
			slog.Warn("symbol index update failed", "kind", kind, "path", newPath, "err", err)
		}
	}
	return nil
}

// ArtifactExists returns true if an active artifact with the given kind+path exists.
func ArtifactExists(db dbh.Handle, kind, path string) bool {
	var count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM of_files WHERE kind = ? AND path = ? AND is_deleted = 0`, kind, path).Scan(&count)
	return count > 0
}
