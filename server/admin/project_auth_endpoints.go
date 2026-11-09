package admin

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

// ProjectAuthEndpointsHandler handles auth endpoint listing/deletion for admin.
type ProjectAuthEndpointsHandler struct {
	deps *Deps
}

type authEndpoint struct {
	ID             int       `json:"id"`
	ProjectID      int       `json:"project_id"`
	Path           string    `json:"path"`
	TableName      string    `json:"table_name"`
	UsernameColumn string    `json:"username_column"`
	PasswordColumn string    `json:"password_column"`
	PublicColumns  string    `json:"public_columns"`
	CreatedAt      time.Time `json:"created_at"`
	Routes         []string  `json:"routes"`
}

// List returns all auth endpoints for a project.
func (h *ProjectAuthEndpointsHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	rows, err := projectDB.Query(
		"SELECT id, path, table_name, username_column, password_column, public_columns, created_at FROM of_auth_endpoints ORDER BY path",
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []authEndpoint{})
		return
	}

	endpoints := collectRows(rows, func(rows *sql.Rows) (authEndpoint, error) {
		var e authEndpoint
		e.ProjectID = projectID
		if err := rows.Scan(&e.ID, &e.Path, &e.TableName, &e.UsernameColumn, &e.PasswordColumn, &e.PublicColumns, &e.CreatedAt); err != nil {
			return e, err
		}
		e.Routes = []string{
			fmt.Sprintf("POST /api/%s/register", e.Path),
			fmt.Sprintf("POST /api/%s/login", e.Path),
			fmt.Sprintf("GET /api/%s/me", e.Path),
		}
		return e, nil
	})

	if endpoints == nil {
		endpoints = []authEndpoint{}
	}

	writeJSON(w, http.StatusOK, endpoints)
}

// Delete removes an auth endpoint by ID.
func (h *ProjectAuthEndpointsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	endpointID, ok := requireIntParam(w, r, "endpointID", "invalid endpoint ID")
	if !ok {
		return
	}
	res, err := projectDB.ExecWrite(
		"DELETE FROM of_auth_endpoints WHERE id = ?",
		endpointID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete auth endpoint")
		return
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		writeError(w, http.StatusNotFound, "auth endpoint not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": endpointID})
}
