// Package diskpath resolves the file locations stored in of_files.disk_path and
// of_blobs.storage_path. Those columns hold a full path, so copying a project,
// importing one, or moving the data directory leaves them pointing at the old
// place; this package re-anchors them under the project's current directory and
// keeps every resolved path confined to it.
package diskpath

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// marker is the path segment that identifies a project directory inside a stored
// path, for the given project id.
func marker(id int) string { return fmt.Sprintf("/projects/%d/", id) }

// normalize turns either separator into "/" so a Windows-authored path is read
// the same on Linux.
func normalize(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// Inside reports whether p lies within dir, on a path-segment boundary:
// projects/1 does not contain projects/10.
func Inside(dir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Rebase re-anchors a stored path that points inside project oldID so it points
// inside newDir instead, whatever absolute or relative prefix it carried. A path
// with no such segment is returned unchanged.
func Rebase(p string, oldID int, newDir string) string {
	norm := "/" + strings.TrimPrefix(normalize(p), "/")
	i := strings.LastIndex(norm, marker(oldID))
	if i < 0 {
		return p
	}
	rest := norm[i+len(marker(oldID)):]
	out := filepath.Join(newDir, filepath.FromSlash(rest))
	if !Inside(newDir, out) {
		return p
	}
	return out
}

// Resolve returns where a stored path lives for the project whose directory is
// dir (named by its numeric id). A path already inside dir is used as is; one
// that points at a moved or renamed data directory is re-anchored. ok is false
// when the path cannot be placed inside dir, so callers refuse it.
func Resolve(dir, p string) (string, bool) {
	if Inside(dir, p) {
		return filepath.Clean(p), true
	}
	var id int
	if _, err := fmt.Sscanf(filepath.Base(filepath.Clean(dir)), "%d", &id); err != nil {
		return "", false
	}
	out := Rebase(p, id, dir)
	if out == p || !Inside(dir, out) {
		return "", false
	}
	return out, true
}

// DB is the slice of a project database RemapProject needs.
type DB interface {
	Query(query string, args ...any) (*sql.Rows, error)
	ExecWrite(query string, args ...any) (sql.Result, error)
}

// RemapProject rewrites every stored file location in a project database that
// points into project oldID so it points into newDir. Two tables embed such
// paths: of_files.disk_path (uploaded and disk artifacts) and
// of_blobs.storage_path (cached blobs). Used after a copy or an import, where the
// rows still name the source project. A missing table is tolerated.
func RemapProject(pdb DB, oldID int, newDir string) error {
	if err := remapColumn(pdb,
		"SELECT id, disk_path FROM of_files WHERE storage = 'disk' AND disk_path <> ''",
		"UPDATE of_files SET disk_path = ? WHERE id = ?", oldID, newDir); err != nil {
		return err
	}
	return remapColumn(pdb,
		"SELECT id, storage_path FROM of_blobs WHERE storage_path <> ''",
		"UPDATE of_blobs SET storage_path = ? WHERE id = ?", oldID, newDir)
}

func remapColumn(pdb DB, selectQuery, updateQuery string, oldID int, newDir string) error {
	rows, err := pdb.Query(selectQuery)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return err
	}
	type rewrite struct {
		id   int
		path string
	}
	var updates []rewrite
	for rows.Next() {
		var id int
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return err
		}
		if np := Rebase(p, oldID, newDir); np != p {
			updates = append(updates, rewrite{id: id, path: np})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range updates {
		if _, err := pdb.ExecWrite(updateQuery, u.path, u.id); err != nil {
			return err
		}
	}
	return nil
}
