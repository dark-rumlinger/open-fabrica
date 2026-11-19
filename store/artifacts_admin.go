package store

import (
	"database/sql"
	"errors"
	"time"

	"openfabrica/db"
)

var (
	ErrComponentNotFound = errors.New("component not found")
	ErrLayoutNotFound    = errors.New("layout not found")
)

// ArtifactVersionEntry represents an attribution-only row in of_file_history.
type ArtifactVersionEntry struct {
	VersionNumber int       `json:"version_number"`
	ChangedBy     string    `json:"changed_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// ComponentEntry represents a component row in of_files (kind='component').
type ComponentEntry struct {
	ID           int       `json:"id"`
	Name         string    `json:"name"`
	Body         string    `json:"body"`
	Description  string    `json:"description"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	VersionCount int       `json:"version_count"`
}

// LayoutEntry represents a layout row in of_files (kind='layout').
type LayoutEntry struct {
	ID           int       `json:"id"`
	Name         string    `json:"name"`
	Template     string    `json:"template"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	VersionCount int       `json:"version_count"`
}

// ArtifactPathByID looks up an of_files path by (kind, id) on the read pool.
func ArtifactPathByID(pdb *db.ProjectDB, kind string, id int) (string, error) {
	var path string
	err := pdb.QueryRow("SELECT path FROM of_files WHERE kind = ? AND id = ?", kind, id).Scan(&path)
	return path, err
}

// ListArtifactVersions returns the last 20 history rows for (kind, id).
func ListArtifactVersions(pdb *db.ProjectDB, kind string, id int) []ArtifactVersionEntry {
	path, err := ArtifactPathByID(pdb, kind, id)
	if err != nil {
		return []ArtifactVersionEntry{}
	}
	rows, err := pdb.Query(
		"SELECT version_number, changed_by, created_at FROM of_file_history WHERE kind = ? AND path = ? ORDER BY version_number DESC LIMIT 20",
		kind, path,
	)
	if err != nil {
		return []ArtifactVersionEntry{}
	}
	defer rows.Close()

	versions := []ArtifactVersionEntry{}
	for rows.Next() {
		var v ArtifactVersionEntry
		if rows.Scan(&v.VersionNumber, &v.ChangedBy, &v.CreatedAt) == nil {
			versions = append(versions, v)
		}
	}
	_ = rows.Err()
	return versions
}

// RecordArtifactEdit appends an attribution-only history row for an admin edit.
func RecordArtifactEdit(writer *sql.DB, kind string, id int, path string) {
	var maxVer int
	_ = writer.QueryRow("SELECT COALESCE(MAX(version_number), 0) FROM of_file_history WHERE kind = ? AND path = ?", kind, path).Scan(&maxVer)
	bestEffortExec(writer, "file history insert",
		"INSERT INTO of_file_history (file_id, kind, path, version_number, changed_by) VALUES (?, ?, ?, ?, 'admin')",
		id, kind, path, maxVer+1,
	)
}

// SoftDeleteArtifact marks (kind, id) deleted and returns the rows affected.
func SoftDeleteArtifact(pdb *db.ProjectDB, kind string, id int) (int64, error) {
	res, err := pdb.ExecWrite(
		"UPDATE of_files SET is_deleted = 1, deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE kind = ? AND id = ? AND is_deleted = 0",
		kind, id,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListComponents returns all non-deleted components for a project.
func ListComponents(pdb *db.ProjectDB) []ComponentEntry {
	rows, err := pdb.Query(
		`SELECT id, path, COALESCE(body, '') AS body,
		        COALESCE(json_extract(metadata, '$.description'), '') AS description,
		        created_at, updated_at,
		        (SELECT COUNT(*) FROM of_file_history WHERE kind = 'component' AND path = of_files.path) AS version_count
		 FROM of_files WHERE kind = 'component' AND is_deleted = 0 ORDER BY path`,
	)
	if err != nil {
		return []ComponentEntry{}
	}
	defer rows.Close()

	out := []ComponentEntry{}
	for rows.Next() {
		var c ComponentEntry
		if rows.Scan(&c.ID, &c.Name, &c.Body, &c.Description, &c.CreatedAt, &c.UpdatedAt, &c.VersionCount) == nil {
			out = append(out, c)
		}
	}
	_ = rows.Err()
	return out
}

// GetComponent returns a single non-deleted component by ID.
func GetComponent(pdb *db.ProjectDB, componentID int) (*ComponentEntry, error) {
	var c ComponentEntry
	err := pdb.QueryRow(
		`SELECT id, path, COALESCE(body, '') AS body,
		        COALESCE(json_extract(metadata, '$.description'), '') AS description,
		        created_at, updated_at
		 FROM of_files WHERE kind = 'component' AND id = ? AND is_deleted = 0`,
		componentID,
	).Scan(&c.ID, &c.Name, &c.Body, &c.Description, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrComponentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CountComponentReferences returns how many pages/layouts include {{include:name}}.
func CountComponentReferences(pdb *db.ProjectDB, componentID int) (int, error) {
	var name string
	if err := pdb.QueryRow("SELECT path FROM of_files WHERE kind = 'component' AND id = ? AND is_deleted = 0", componentID).Scan(&name); err != nil {
		return 0, ErrComponentNotFound
	}
	needle := "{{include:" + name + "}}"
	var refCount int
	_ = pdb.QueryRow(
		`SELECT COUNT(*) FROM of_files
		 WHERE kind IN ('page', 'layout') AND is_deleted = 0 AND body LIKE ?`,
		"%"+needle+"%",
	).Scan(&refCount)
	return refCount, nil
}

// UpdateComponentBody records an admin edit and updates the component body.
func UpdateComponentBody(pdb *db.ProjectDB, componentID int, body string) error {
	writer := pdb.Writer()
	var name string
	if err := writer.QueryRow("SELECT path FROM of_files WHERE kind = 'component' AND id = ? AND is_deleted = 0", componentID).Scan(&name); err != nil {
		return ErrComponentNotFound
	}
	RecordArtifactEdit(writer, "component", componentID, name)
	_, err := writer.Exec(
		"UPDATE of_files SET body = ?, updated_at = CURRENT_TIMESTAMP WHERE kind = 'component' AND id = ?",
		body, componentID,
	)
	return err
}

// ListLayouts returns all non-deleted layouts for a project.
func ListLayouts(pdb *db.ProjectDB) []LayoutEntry {
	rows, err := pdb.Query(
		`SELECT id, path, COALESCE(body, '') AS template, created_at, updated_at,
		        (SELECT COUNT(*) FROM of_file_history WHERE kind = 'layout' AND path = of_files.path) AS version_count
		 FROM of_files WHERE kind = 'layout' AND is_deleted = 0 ORDER BY path`,
	)
	if err != nil {
		return []LayoutEntry{}
	}
	defer rows.Close()

	out := []LayoutEntry{}
	for rows.Next() {
		var l LayoutEntry
		if rows.Scan(&l.ID, &l.Name, &l.Template, &l.CreatedAt, &l.UpdatedAt, &l.VersionCount) == nil {
			out = append(out, l)
		}
	}
	_ = rows.Err()
	return out
}

// GetLayout returns a single non-deleted layout by ID.
func GetLayout(pdb *db.ProjectDB, layoutID int) (*LayoutEntry, error) {
	var l LayoutEntry
	err := pdb.QueryRow(
		"SELECT id, path, COALESCE(body, '') AS template, created_at, updated_at FROM of_files WHERE kind = 'layout' AND id = ? AND is_deleted = 0",
		layoutID,
	).Scan(&l.ID, &l.Name, &l.Template, &l.CreatedAt, &l.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLayoutNotFound
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// CountLayoutReferences returns how many non-deleted pages reference this layout.
func CountLayoutReferences(pdb *db.ProjectDB, layoutID int) (int, error) {
	var name string
	if err := pdb.QueryRow("SELECT path FROM of_files WHERE kind = 'layout' AND id = ? AND is_deleted = 0", layoutID).Scan(&name); err != nil {
		return 0, ErrLayoutNotFound
	}
	var pageCount int
	_ = pdb.QueryRow(
		"SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND json_extract(metadata, '$.layout') = ? AND is_deleted = 0",
		name,
	).Scan(&pageCount)
	return pageCount, nil
}

// UpdateLayoutTemplate records an admin edit and updates the layout body.
func UpdateLayoutTemplate(pdb *db.ProjectDB, layoutID int, template string) error {
	writer := pdb.Writer()
	var name string
	if err := writer.QueryRow("SELECT path FROM of_files WHERE kind = 'layout' AND id = ? AND is_deleted = 0", layoutID).Scan(&name); err != nil {
		return ErrLayoutNotFound
	}
	RecordArtifactEdit(writer, "layout", layoutID, name)
	_, err := writer.Exec(
		"UPDATE of_files SET body = ?, updated_at = CURRENT_TIMESTAMP WHERE kind = 'layout' AND id = ?",
		template, layoutID,
	)
	return err
}
