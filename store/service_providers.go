package store

import (
	"database/sql"
	"errors"
	"time"

	"openfabrica/db"
)

// ErrServiceProviderNotFound is returned when a service provider row does not exist.
var ErrServiceProviderNotFound = errors.New("service provider not found")

// ServiceProvider represents a row in of_service_providers.
type ServiceProvider struct {
	ID          int       `json:"id"`
	ProjectID   int       `json:"project_id"`
	Name        string    `json:"name"`
	BaseURL     string    `json:"base_url"`
	AuthType    string    `json:"auth_type"`
	SecretName  *string   `json:"secret_name,omitempty"`
	Description string    `json:"description"`
	ApiDocs     string    `json:"api_docs"`
	IsEnabled   bool      `json:"is_enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// ListServiceProviders returns all of_service_providers rows ordered by name.
func ListServiceProviders(pdb *db.ProjectDB, projectID int) ([]ServiceProvider, error) {
	rows, err := pdb.Query(
		`SELECT id, name, base_url, auth_type, secret_name, description, api_docs, is_enabled, created_at
		 FROM of_service_providers ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	providers := []ServiceProvider{}
	for rows.Next() {
		var p ServiceProvider
		var secretName sql.NullString
		p.ProjectID = projectID
		if rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.AuthType, &secretName, &p.Description, &p.ApiDocs, &p.IsEnabled, &p.CreatedAt) != nil {
			continue
		}
		if secretName.Valid {
			p.SecretName = &secretName.String
		}
		providers = append(providers, p)
	}
	return providers, rows.Err()
}

// ToggleServiceProvider flips is_enabled on a service provider row.
func ToggleServiceProvider(pdb *db.ProjectDB, provID int) error {
	_, err := pdb.ExecWrite("UPDATE of_service_providers SET is_enabled = NOT is_enabled WHERE id = ?", provID)
	return err
}

// DeleteServiceProvider removes a service provider by ID.
func DeleteServiceProvider(pdb *db.ProjectDB, provID int) error {
	res, err := pdb.ExecWrite("DELETE FROM of_service_providers WHERE id = ?", provID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrServiceProviderNotFound
	}
	return nil
}
