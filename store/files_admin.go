package store

import (
	"database/sql"
	"errors"
	"time"

	"openfabrica/db"
)

var ErrFileNotFound = errors.New("file not found")

// ProjectFileEntry represents a file or flow-test row in of_files for admin listing.
type ProjectFileEntry struct {
	ID              int       `json:"id"`
	ProjectID       int       `json:"project_id"`
	Filename        string    `json:"filename"`
	ContentType     *string   `json:"content_type"`
	Size            *int      `json:"size"`
	Description     *string   `json:"description"`
	CreatedAt       time.Time `json:"created_at"`
	VersionCount    int       `json:"version_count"`
	PlatformManaged bool      `json:"platform_managed"`
	Kind            string    `json:"kind"`
}

// FileLocation holds the storage metadata for a file row.
type FileLocation struct {
	Storage  sql.NullString
	Body     sql.NullString
	DiskPath sql.NullString
	Path     string
}

// ListProjectFiles returns all non-deleted files (plus the flow-test script) for a project.
func ListProjectFiles(pdb *db.ProjectDB, projectID int, flowKind, flowPath string) []ProjectFileEntry {
	rows, err := pdb.Query(
		`SELECT id, path, content_type, size,
		        COALESCE(json_extract(metadata, '$.description'), ''),
		        created_at,
		        (SELECT COUNT(*) FROM of_file_history h WHERE h.kind = of_files.kind AND h.path = of_files.path) AS version_count,
		        COALESCE(json_extract(metadata, '$.platform_managed'), 0),
		        kind
		 FROM of_files
		 WHERE is_deleted = 0 AND (kind = 'file' OR (kind = ? AND path = ?))
		 ORDER BY created_at DESC`,
		flowKind, flowPath,
	)
	if err != nil {
		return []ProjectFileEntry{}
	}
	defer rows.Close()

	files := []ProjectFileEntry{}
	for rows.Next() {
		var f ProjectFileEntry
		f.ProjectID = projectID
		var platformManaged int
		if rows.Scan(&f.ID, &f.Filename, &f.ContentType, &f.Size, &f.Description, &f.CreatedAt, &f.VersionCount, &platformManaged, &f.Kind) == nil {
			f.PlatformManaged = platformManaged == 1
			files = append(files, f)
		}
	}
	_ = rows.Err()
	return files
}

// UpsertDiskFile records a disk-backed file row in of_files.
func UpsertDiskFile(pdb *db.ProjectDB, filename, contentType string, size int, diskPath, metadataJSON string) error {
	if metadataJSON == "" {
		_, err := pdb.ExecWrite(
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
			filename, contentType, size, diskPath,
		)
		return err
	}
	_, err := pdb.ExecWrite(
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
		filename, contentType, size, diskPath, metadataJSON,
	)
	return err
}

// FileRowKind returns the kind of the Files-view row with this id ("file" or flowKind).
func FileRowKind(pdb *db.ProjectDB, id int, flowKind, flowPath string) (kind string, found bool) {
	err := pdb.QueryRow(
		`SELECT kind FROM of_files WHERE id = ? AND (kind = 'file' OR (kind = ? AND path = ?))`,
		id, flowKind, flowPath,
	).Scan(&kind)
	if err != nil {
		return "file", false
	}
	return kind, true
}

// LoadFileLocation returns storage, body, disk_path, and path for (kind, fileID).
func LoadFileLocation(pdb *db.ProjectDB, kind string, fileID int) (*FileLocation, error) {
	var loc FileLocation
	err := pdb.QueryRow(
		"SELECT storage, body, disk_path, path FROM of_files WHERE kind = ? AND id = ?",
		kind, fileID,
	).Scan(&loc.Storage, &loc.Body, &loc.DiskPath, &loc.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFileNotFound
	}
	if err != nil {
		return nil, err
	}
	return &loc, nil
}

// UpdateInlineFileBody updates the body and size of an inline file row.
func UpdateInlineFileBody(pdb *db.ProjectDB, fileID int, content string) error {
	_, err := pdb.ExecWrite(
		"UPDATE of_files SET body = ?, size = ?, updated_at = CURRENT_TIMESTAMP WHERE kind = 'file' AND id = ?",
		content, len(content), fileID,
	)
	return err
}

// UpdateFileSize updates the stored size of a disk-backed file row.
func UpdateFileSize(pdb *db.ProjectDB, fileID, size int) error {
	_, err := pdb.ExecWrite("UPDATE of_files SET size = ? WHERE kind = 'file' AND id = ?", size, fileID)
	return err
}
