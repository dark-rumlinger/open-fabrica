package store

// Disk storage helpers used by tools that persist file bytes to disk and
// record the path in of_files.disk_path.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"openfabrica/internal/dbh"
)

// StorageDir returns the storage directory for a project, creating it if needed.
// dirName is the subdir under {dataDir}/projects/{id}/. dataDir is passed in
// rather than spelled here: the server serves these files back against the
// configured DataDir, so a literal path would write where the reader does not look.
func StorageDir(dataDir string, projectID int, dirName string) (string, error) {
	dir := filepath.Join(dataDir, "projects", fmt.Sprintf("%d", projectID), dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s directory: %w", dirName, err)
	}
	return dir, nil
}

// ValidateStorageFilename rejects a filename that would escape a project's
// storage dir: an absolute path, or any ".." traversal segment. Every tool that
// turns a caller-supplied name into a disk path must run this first - it is the
// single containment guard, mirrored by the read-side check in server/public/static.go.
func ValidateStorageFilename(filename string) error {
	cleaned := filepath.ToSlash(filepath.Clean(filename))
	if cleaned != filepath.ToSlash(filename) || filepath.IsAbs(filename) || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return fmt.Errorf("invalid filename %q: path traversal or absolute path not allowed", filename)
	}
	return nil
}

// RegisterDiskArtifact upserts a kind='file', storage='disk' row in of_files
// for bytes already written at diskPath, always clearing is_deleted = 0 and
// deleted_at = NULL so overwriting a previously soft-deleted file works.
func RegisterDiskArtifact(db dbh.Handle, filename, contentType string, size int64, diskPath, metaJSON string) error {
	if metaJSON == "" {
		metaJSON = "{}"
	}
	_, err := db.Exec(
		`INSERT INTO of_files (kind, path, storage, content_type, size, disk_path, scope, metadata, is_deleted, updated_at)
		 VALUES ('file', ?, 'disk', ?, ?, ?, 'global', ?, 0, CURRENT_TIMESTAMP)
		 ON CONFLICT(kind, path) DO UPDATE SET
		   storage = 'disk',
		   body = '',
		   content_type = excluded.content_type,
		   size = excluded.size,
		   disk_path = excluded.disk_path,
		   scope = excluded.scope,
		   metadata = excluded.metadata,
		   is_deleted = 0,
		   deleted_at = NULL,
		   updated_at = CURRENT_TIMESTAMP`,
		filename, contentType, size, diskPath, metaJSON,
	)
	return err
}

// SaveDiskArtifact writes data to the project's subdir on disk and registers
// it in of_files. If the DB upsert fails, the newly written file is removed so
// failed writes do not leave orphaned files on disk.
func SaveDiskArtifact(db dbh.Handle, dataDir string, projectID int, subdir, filename, contentType string, data []byte, metaJSON string) (string, error) {
	storagePath, err := WriteFileToDisk(dataDir, projectID, subdir, filename, data)
	if err != nil {
		return "", fmt.Errorf("writing to disk: %w", err)
	}
	if err := RegisterDiskArtifact(db, filename, contentType, int64(len(data)), storagePath, metaJSON); err != nil {
		_ = os.Remove(storagePath)
		return "", fmt.Errorf("recording artifact: %w", err)
	}
	return storagePath, nil
}

// WriteFileToDisk writes data to data/projects/{projectID}/{subdir}/{filename} and
// returns the absolute path. Callers record the returned path in
// of_files.disk_path so the public handler can serve the bytes later.
func WriteFileToDisk(dataDir string, projectID int, subdir, filename string, data []byte) (string, error) {
	// Contain the write to the project's storage dir, before creating any
	// directories.
	if err := ValidateStorageFilename(filename); err != nil {
		return "", err
	}
	dir, err := StorageDir(dataDir, projectID, subdir)
	if err != nil {
		return "", err
	}
	fullPath := filepath.Join(dir, filepath.FromSlash(filename))
	// Make sure any nested directories from a "/"-containing filename exist.
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return "", fmt.Errorf("creating parent directory: %w", err)
	}
	if err := os.WriteFile(fullPath, data, 0o644); err != nil {
		return "", fmt.Errorf("writing file: %w", err)
	}
	return fullPath, nil
}
