package store

import (
	"errors"
	"fmt"
	"time"

	"openfabrica/db"
)

// ErrAuthEndpointNotFound is returned when an auth endpoint row does not exist.
var ErrAuthEndpointNotFound = errors.New("auth endpoint not found")

// AuthEndpointInfo represents a row in of_auth_endpoints for admin listing.
type AuthEndpointInfo struct {
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

// ListAuthEndpoints returns all of_auth_endpoints rows for projectID.
func ListAuthEndpoints(pdb *db.ProjectDB, projectID int) []AuthEndpointInfo {
	rows, err := pdb.Query(
		"SELECT id, path, table_name, username_column, password_column, public_columns, created_at FROM of_auth_endpoints ORDER BY path",
	)
	if err != nil {
		return []AuthEndpointInfo{}
	}
	defer rows.Close()

	endpoints := []AuthEndpointInfo{}
	for rows.Next() {
		var e AuthEndpointInfo
		e.ProjectID = projectID
		if rows.Scan(&e.ID, &e.Path, &e.TableName, &e.UsernameColumn, &e.PasswordColumn, &e.PublicColumns, &e.CreatedAt) != nil {
			continue
		}
		e.Routes = []string{
			fmt.Sprintf("POST /api/%s/register", e.Path),
			fmt.Sprintf("POST /api/%s/login", e.Path),
			fmt.Sprintf("GET /api/%s/me", e.Path),
		}
		endpoints = append(endpoints, e)
	}
	_ = rows.Err()
	return endpoints
}

// DeleteAuthEndpoint deletes an auth endpoint by ID.
func DeleteAuthEndpoint(pdb *db.ProjectDB, endpointID int) error {
	res, err := pdb.ExecWrite("DELETE FROM of_auth_endpoints WHERE id = ?", endpointID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAuthEndpointNotFound
	}
	return nil
}
