package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"openfabrica/db"
)

var (
	ErrPageNotFound  = errors.New("page not found")
	ErrPageDuplicate = errors.New("page already exists")
)

// ProjectPage represents a page row in of_files (kind='page').
type ProjectPage struct {
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

// PageUpdate describes partial updates to a page.
type PageUpdate struct {
	Title   *string
	Content *string
	Status  *string
}

// ListPages returns all non-deleted pages for a project (without body content).
func ListPages(pdb *db.ProjectDB, projectID int) []ProjectPage {
	rows, err := pdb.Query(
		`SELECT id, path,
		        json_extract(metadata, '$.title'),
		        json_extract(metadata, '$.template'),
		        status, metadata, created_at, updated_at,
		        (SELECT COUNT(*) FROM of_file_history WHERE kind = 'page' AND path = of_files.path) AS version_count
		 FROM of_files WHERE kind = 'page' AND is_deleted = 0 ORDER BY path ASC`,
	)
	if err != nil {
		return []ProjectPage{}
	}
	defer rows.Close()

	pages := []ProjectPage{}
	for rows.Next() {
		var p ProjectPage
		p.ProjectID = projectID
		if rows.Scan(&p.ID, &p.Path, &p.Title, &p.Template, &p.Status, &p.Metadata, &p.CreatedAt, &p.UpdatedAt, &p.VersionCount) == nil {
			pages = append(pages, p)
		}
	}
	_ = rows.Err()
	return pages
}

// GetPage returns a single non-deleted page with its content.
func GetPage(pdb *db.ProjectDB, projectID, pageID int) (*ProjectPage, error) {
	var p ProjectPage
	p.ProjectID = projectID
	err := pdb.QueryRow(
		`SELECT id, path,
		        json_extract(metadata, '$.title'),
		        body,
		        json_extract(metadata, '$.template'),
		        status, metadata, created_at, updated_at
		 FROM of_files WHERE kind = 'page' AND id = ? AND is_deleted = 0`,
		pageID,
	).Scan(&p.ID, &p.Path, &p.Title, &p.Content, &p.Template, &p.Status, &p.Metadata, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPageNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePage inserts a new published page at path, returning its ID.
func CreatePage(pdb *db.ProjectDB, path string, title, content *string) (int64, error) {
	var existing int
	_ = pdb.QueryRow("SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND path = ? AND is_deleted = 0", path).Scan(&existing)
	if existing > 0 {
		return 0, ErrPageDuplicate
	}
	body := ""
	if content != nil {
		body = *content
	}
	meta := map[string]any{}
	if title != nil {
		meta["title"] = *title
	}
	metaJSON, _ := json.Marshal(meta)

	res, err := pdb.ExecWrite(
		`INSERT INTO of_files (kind, path, body, content_type, metadata, status)
		 VALUES ('page', ?, ?, 'text/html; charset=utf-8', ?, 'published')`,
		path, body, string(metaJSON),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdatePage applies partial updates to a page.
func UpdatePage(pdb *db.ProjectDB, pageID int, u PageUpdate) error {
	var setClauses []string
	var values []any
	if u.Title != nil {
		setClauses = append(setClauses, "metadata = json_set(COALESCE(metadata, '{}'), '$.title', ?)")
		values = append(values, *u.Title)
	}
	if u.Content != nil {
		setClauses = append(setClauses, "body = ?")
		values = append(values, *u.Content)
	}
	if u.Status != nil {
		setClauses = append(setClauses, "status = ?")
		values = append(values, *u.Status)
	}
	if len(setClauses) == 0 {
		return nil
	}
	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")
	values = append(values, pageID)

	query := "UPDATE of_files SET " + strings.Join(setClauses, ", ") + " WHERE kind = 'page' AND id = ? AND is_deleted = 0"
	res, err := pdb.ExecWrite(query, values...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPageNotFound
	}
	return nil
}
