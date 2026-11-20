package store

import (
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"

	"openfabrica/security"
)

// ExportEndpointRow holds raw columns from of_export_endpoints.
type ExportEndpointRow struct {
	TableName    string
	PublicCols   sql.NullString
	RequiresAuth bool
	OwnerColumn  string
	TenantColumn string
}

// LoadExportEndpoint loads an export endpoint configuration by path.
func LoadExportEndpoint(reader *sql.DB, path string) (*ExportEndpointRow, error) {
	var r ExportEndpointRow
	err := reader.QueryRow(
		`SELECT table_name, public_columns, requires_auth, COALESCE(owner_column,''), COALESCE(tenant_column,'')
		 FROM of_export_endpoints WHERE path = ?`,
		path,
	).Scan(&r.TableName, &r.PublicCols, &r.RequiresAuth, &r.OwnerColumn, &r.TenantColumn)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// CSVExportQuery describes a validated CSV export query.
type CSVExportQuery struct {
	Table        string
	Columns      []string
	WhereClauses []string
	WhereArgs    []any
}

// StreamCSVExport executes a parameterized SELECT query and invokes fn for
// each row formatted as strings.
func StreamCSVExport(reader *sql.DB, q CSVExportQuery, onStart func(), fn func([]string) error) error {
	quotedCols := make([]string, len(q.Columns))
	for i, c := range q.Columns {
		quotedCols[i] = security.QuoteIdent(c)
	}
	query := "SELECT " + strings.Join(quotedCols, ", ") + " FROM " + security.QuoteIdent(q.Table)
	if len(q.WhereClauses) > 0 {
		query += " WHERE " + strings.Join(q.WhereClauses, " AND ")
	}
	query += " ORDER BY rowid"

	rows, err := reader.Query(query, q.WhereArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if onStart != nil {
		onStart()
	}

	colCount := len(q.Columns)
	vals := make([]any, colCount)
	ptrs := make([]any, colCount)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	record := make([]string, colCount)

	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		for i, v := range vals {
			if v == nil {
				record[i] = ""
			} else if b, ok := v.([]byte); ok {
				record[i] = string(b)
			} else {
				record[i] = fmt.Sprint(v)
			}
		}
		if err := fn(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ImportEndpointRow holds raw columns from of_import_endpoints.
type ImportEndpointRow struct {
	TableName    string
	Mode         string
	UniqueColumn string
	RequiresAuth bool
	MaxRows      int
}

// LoadImportEndpoint loads an import endpoint configuration by path.
func LoadImportEndpoint(reader *sql.DB, path string) (*ImportEndpointRow, error) {
	var r ImportEndpointRow
	err := reader.QueryRow(
		`SELECT table_name, mode, COALESCE(unique_column,''), requires_auth, max_rows
		 FROM of_import_endpoints WHERE path = ?`,
		path,
	).Scan(&r.TableName, &r.Mode, &r.UniqueColumn, &r.RequiresAuth, &r.MaxRows)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// FindTableRLSScopeColumns returns the first configured owner_column and
// tenant_column for tableName in of_api_endpoints, or empty strings when none exist.
func FindTableRLSScopeColumns(reader *sql.DB, tableName string) (ownerCol, tenantCol string, err error) {
	err = reader.QueryRow(
		`SELECT COALESCE(owner_column,''), COALESCE(tenant_column,'')
		 FROM of_api_endpoints
		 WHERE table_name = ? AND (COALESCE(owner_column,'') != '' OR COALESCE(tenant_column,'') != '')
		 LIMIT 1`,
		tableName,
	).Scan(&ownerCol, &tenantCol)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	return ownerCol, tenantCol, err
}

// CSVImportError distinguishes transaction/prepare/row failures for HTTP status mapping.
type CSVImportError struct {
	Stage   string // "begin", "prepare", "row", "commit", or "caller"
	RowNum  int
	Wrapped error
}

func (e *CSVImportError) Error() string { return e.Wrapped.Error() }
func (e *CSVImportError) Unwrap() error { return e.Wrapped }

// CSVImportSpec describes a validated CSV insert/upsert target.
type CSVImportSpec struct {
	Table        string
	Columns      []string
	Mode         string // "upsert" or "insert"
	UniqueColumn string
	StampColumns map[string]bool
}

// RunCSVImport runs a prepared insert/upsert statement in a single transaction,
// pulling parameter slices from nextArgs until io.EOF.
func RunCSVImport(writer *sql.DB, spec CSVImportSpec, nextArgs func(rowNum int) ([]any, error)) (int, error) {
	quoted := make([]string, len(spec.Columns))
	for i, c := range spec.Columns {
		quoted[i] = security.QuoteIdent(c)
	}
	cols := strings.Join(quoted, ", ")
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(spec.Columns)), ",")

	var stmt string
	if spec.Mode == "upsert" && spec.UniqueColumn != "" {
		stampLower := make(map[string]bool, len(spec.StampColumns))
		stampList := make([]string, 0, len(spec.StampColumns))
		for sc := range spec.StampColumns {
			stampLower[strings.ToLower(sc)] = true
			stampList = append(stampList, sc)
		}
		sort.Strings(stampList)

		setClauses := make([]string, 0, len(spec.Columns))
		for _, c := range spec.Columns {
			// Skip the conflict key and the stamped RLS columns: an upsert must
			// not rewrite an existing row's owner/tenant to the caller's.
			if strings.EqualFold(c, spec.UniqueColumn) || stampLower[strings.ToLower(c)] {
				continue
			}
			setClauses = append(setClauses, security.QuoteIdent(c)+" = excluded."+security.QuoteIdent(c))
		}
		if len(setClauses) == 0 {
			stmt = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) DO NOTHING",
				security.QuoteIdent(spec.Table), cols, placeholders, security.QuoteIdent(spec.UniqueColumn))
		} else {
			whereSQL := ""
			if len(stampList) > 0 {
				// Enforce RLS on upsert conflicts: a colliding unique key must
				// only update rows belonging to the caller's own owner/tenant.
				conds := make([]string, 0, len(stampList))
				qTable := security.QuoteIdent(spec.Table)
				for _, sc := range stampList {
					qCol := security.QuoteIdent(sc)
					conds = append(conds, fmt.Sprintf("%s.%s = excluded.%s", qTable, qCol, qCol))
				}
				whereSQL = " WHERE " + strings.Join(conds, " AND ")
			}
			stmt = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) DO UPDATE SET %s%s",
				security.QuoteIdent(spec.Table), cols, placeholders, security.QuoteIdent(spec.UniqueColumn), strings.Join(setClauses, ", "), whereSQL)
		}
	} else {
		stmt = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", security.QuoteIdent(spec.Table), cols, placeholders)
	}

	tx, err := writer.Begin()
	if err != nil {
		return 0, &CSVImportError{Stage: "begin", Wrapped: err}
	}
	prep, err := tx.Prepare(stmt)
	if err != nil {
		_ = tx.Rollback()
		return 0, &CSVImportError{Stage: "prepare", Wrapped: err}
	}

	imported := 0
	for {
		args, err := nextArgs(imported + 1)
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = prep.Close()
			_ = tx.Rollback()
			return imported, &CSVImportError{Stage: "caller", RowNum: imported + 1, Wrapped: err}
		}
		if args == nil {
			continue
		}
		if _, err := prep.Exec(args...); err != nil {
			_ = prep.Close()
			_ = tx.Rollback()
			return imported, &CSVImportError{Stage: "row", RowNum: imported + 1, Wrapped: err}
		}
		imported++
	}
	_ = prep.Close()
	if err := tx.Commit(); err != nil {
		return imported, &CSVImportError{Stage: "commit", Wrapped: err}
	}
	return imported, nil
}
