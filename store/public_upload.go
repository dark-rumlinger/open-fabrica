package store

import (
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/security"
)

// UploadEndpointRow holds raw columns from of_upload_endpoints.
type UploadEndpointRow struct {
	Path         string
	AllowedTypes string
	MaxSizeMB    int
	RequiresAuth bool
	RateLimit    int
	TableName    string
	OwnerColumn  string
	TenantColumn string
}

// LoadUploadEndpoint loads an upload endpoint configuration by path.
func LoadUploadEndpoint(reader *sql.DB, path string) (*UploadEndpointRow, error) {
	var r UploadEndpointRow
	var tableName, ownerCol, tenantCol sql.NullString
	err := reader.QueryRow(
		"SELECT path, allowed_types, max_size_mb, requires_auth, COALESCE(rate_limit,0), table_name, COALESCE(owner_column,''), COALESCE(tenant_column,'') FROM of_upload_endpoints WHERE path = ?",
		path,
	).Scan(&r.Path, &r.AllowedTypes, &r.MaxSizeMB, &r.RequiresAuth, &r.RateLimit, &tableName, &ownerCol, &tenantCol)
	if err != nil {
		return nil, err
	}
	r.TableName = tableName.String
	r.OwnerColumn = ownerCol.String
	r.TenantColumn = tenantCol.String
	return &r, nil
}

// SumDiskUploadBytes returns the total byte size of active on-disk uploads.
func SumDiskUploadBytes(reader *sql.DB) (int64, error) {
	var used sql.NullInt64
	err := reader.QueryRow(
		"SELECT COALESCE(SUM(size), 0) FROM of_files WHERE kind = 'file' AND storage = 'disk' AND path LIKE 'uploads/%' AND is_deleted = 0",
	).Scan(&used)
	return used.Int64, err
}

// UpsertUploadedFile records or updates an on-disk upload in of_files.
func UpsertUploadedFile(writer *sql.DB, storageName, contentType string, size int, diskPath string) error {
	_, err := writer.Exec(
		`INSERT INTO of_files (kind, storage, path, content_type, size, disk_path)
		 VALUES ('file', 'disk', ?, ?, ?, ?)
		 ON CONFLICT(kind, path) DO UPDATE SET storage = excluded.storage, content_type = excluded.content_type, size = excluded.size, disk_path = excluded.disk_path, updated_at = CURRENT_TIMESTAMP`,
		storageName, contentType, size, diskPath,
	)
	return err
}

// InsertUploadedFileRow inserts a linked row into tableName using RETURNING *
// and returns the inserted row as a map.
func InsertUploadedFileRow(writer *sql.DB, tableName string, quotedCols []string, vals []any) (map[string]any, error) {
	placeholders := strings.Repeat("?,", len(quotedCols))
	placeholders = placeholders[:len(placeholders)-1]
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING *",
		security.QuoteIdent(tableName), strings.Join(quotedCols, ", "), placeholders)
	rows, err := writer.Query(insertSQL, vals...)
	if err != nil {
		return nil, err
	}
	inserted := ScanRowsToMaps(rows)
	if len(inserted) == 0 {
		return nil, fmt.Errorf("insert returned no row")
	}
	return inserted[0], nil
}

// DeleteUploadedFileMeta removes an uploaded file's metadata row from of_files.
func DeleteUploadedFileMeta(writer *sql.DB, filePath string) error {
	_, err := writer.Exec(`DELETE FROM of_files WHERE kind = 'file' AND path = ?`, filePath)
	return err
}
