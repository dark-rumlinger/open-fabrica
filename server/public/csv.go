package public

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"openfabrica/db"
	"openfabrica/internal/csvsafe"
	"openfabrica/security"
	"openfabrica/store"
)

// apiCSVExport streams a CSV of the bound table at GET /api/<path>/export.
func (h *Handler) apiCSVExport(w http.ResponseWriter, r *http.Request, projectID int, projectDB *sql.DB, path string) {
	var table string
	var publicColsJSON, ownerCol, tenantCol sql.NullString
	var requiresAuth bool
	err := projectDB.QueryRow(
		`SELECT table_name, public_columns, requires_auth, COALESCE(owner_column,''), COALESCE(tenant_column,'')
		 FROM of_export_endpoints WHERE path = ?`,
		path,
	).Scan(&table, &publicColsJSON, &requiresAuth, &ownerCol, &tenantCol)
	if err != nil {
		writeError(w, http.StatusNotFound, "export endpoint not found")
		return
	}

	authUserID := 0
	authTenantID := 0
	if requiresAuth || ownerCol.String != "" || tenantCol.String != "" {
		uid, tid, ok := h.requireJWTClaims(w, r, projectID)
		if !ok {
			return
		}
		authUserID, authTenantID = uid, tid
	}

	// A tenant-scoped export must run under an active tenant session; without
	// one the tenant filter below cannot bind and the export would stream every
	// tenant's rows. Mirrors the CRUD RLS tenancy gate and the import handler.
	if tenantCol.String != "" && authTenantID == 0 {
		writeError(w, http.StatusForbidden, "no active tenant; call /tenant/switch first")
		return
	}

	var publicCols []string
	if publicColsJSON.Valid && publicColsJSON.String != "" {
		_ = json.Unmarshal([]byte(publicColsJSON.String), &publicCols) // malformed config falls back to PRAGMA below
	}

	if !validTableName.MatchString(table) {
		writeError(w, http.StatusBadRequest, "invalid table")
		return
	}
	// PRAGMA-derived column list when public_columns is empty.
	if len(publicCols) == 0 {
		rows, err := projectDB.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var cid int
				var name, ctype string
				var notNull, pk int
				var dflt any
				if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err == nil {
					if !strings.EqualFold(ctype, "PASSWORD") {
						publicCols = append(publicCols, name)
					}
				}
			}
			if rows.Err() != nil {
				// Treat a failed iteration like a failed PRAGMA query: no
				// derived column list.
				publicCols = nil
			}
		}
	}
	// PASSWORD columns are stored as TEXT, so the PRAGMA type check above never sees
	// them, and an explicit public_columns list is not checked at all. The registered
	// secure-column metadata is the authority (the same set the function runtime hides);
	// drop every hash column from both lists, and fail closed if it cannot be read.
	hashed, err := store.LoadHashColumnNames(projectDB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not determine secret columns")
		return
	}
	kept := publicCols[:0]
	for _, c := range publicCols {
		if !hashed[strings.ToLower(c)] {
			kept = append(kept, c)
		}
	}
	publicCols = kept
	if len(publicCols) == 0 {
		writeError(w, http.StatusBadRequest, "no exportable columns")
		return
	}

	// Validate every column name; never interpolate untrusted strings into SQL.
	for _, c := range publicCols {
		if !validTableName.MatchString(c) {
			writeError(w, http.StatusBadRequest, "invalid column name")
			return
		}
	}

	quotedCols := make([]string, len(publicCols))
	for i, c := range publicCols {
		quotedCols[i] = security.QuoteIdent(c)
	}
	colList := strings.Join(quotedCols, ", ")
	query := "SELECT " + colList + " FROM " + security.QuoteIdent(table)
	args := []any{}
	var whereClauses []string
	if ownerCol.String != "" && authUserID > 0 {
		clause, err := security.OwnerFilterClause(ownerCol.String)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid owner column")
			return
		}
		whereClauses = append(whereClauses, clause)
		args = append(args, authUserID)
	}
	// Tenant scope stacks on top of owner scope (matches the CRUD RLS path). The
	// gate above guarantees authTenantID > 0 whenever tenant_column is set.
	if tenantCol.String != "" && authTenantID > 0 {
		clause, err := security.TenantFilterClause(tenantCol.String)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid tenant column")
			return
		}
		whereClauses = append(whereClauses, clause)
		args = append(args, authTenantID)
	}
	if len(whereClauses) > 0 {
		query += " WHERE " + strings.Join(whereClauses, " AND ")
	}
	query += " ORDER BY rowid"

	rows, err := projectDB.Query(query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(path)+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(publicCols) // streaming to the client; write errors are unactionable

	scanVals := make([]any, len(publicCols))
	scanPtrs := make([]any, len(publicCols))
	for i := range scanVals {
		scanPtrs[i] = &scanVals[i]
	}

	for rows.Next() {
		if err := rows.Scan(scanPtrs...); err != nil {
			continue
		}
		record := make([]string, len(publicCols))
		for i, v := range scanVals {
			record[i] = csvsafe.Cell(csvStringify(v))
		}
		_ = cw.Write(record)
	}
	if err := rows.Err(); err != nil {
		// Headers and rows are already streamed; deliver what we have.
		slog.Warn("csv export: row iteration failed", "path", path, "error", err)
	}
	cw.Flush()
}

func csvStringify(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case []byte:
		return string(t)
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return fmt.Sprintf("%v", v)
}

func sanitizeFilename(s string) string {
	out := strings.Builder{}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == '-' || r == '_':
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		return "export"
	}
	return out.String()
}

// apiCSVImport accepts multipart/form-data with file=<csv> and inserts/upserts
// rows into the bound table.
func (h *Handler) apiCSVImport(w http.ResponseWriter, r *http.Request, projectID int, projectDB *db.ProjectDB, path string) {
	var table, mode, uniqueCol string
	var requiresAuth bool
	var maxRows int
	var uniqueColNull sql.NullString
	err := projectDB.Reader().QueryRow(
		`SELECT table_name, mode, COALESCE(unique_column,''), requires_auth, max_rows
		 FROM of_import_endpoints WHERE path = ?`,
		path,
	).Scan(&table, &mode, &uniqueColNull, &requiresAuth, &maxRows)
	if err != nil {
		writeError(w, http.StatusNotFound, "import endpoint not found")
		return
	}
	uniqueCol = uniqueColNull.String

	var authUserID, authTenantID int
	if requiresAuth {
		uid, tid, ok := h.requireJWTClaims(w, r, projectID)
		if !ok {
			return
		}
		authUserID, authTenantID = uid, tid
	}

	if !validTableName.MatchString(table) {
		writeError(w, http.StatusBadRequest, "invalid table")
		return
	}

	// Bound the request body so a large or unbounded upload can't exhaust memory
	// or temp-disk. Generous cap; the per-endpoint max_rows still limits how many
	// rows are actually imported.
	const maxCSVImportSize = 512 << 20 // 512 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxCSVImportSize)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "bad multipart")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file field is required")
		return
	}
	defer func() { _ = file.Close() }()

	rdr := csv.NewReader(file)
	header, err := rdr.Read()
	if err != nil {
		writeError(w, http.StatusBadRequest, "empty csv")
		return
	}
	for _, c := range header {
		if !validTableName.MatchString(c) {
			writeError(w, http.StatusBadRequest, "invalid header column: "+c)
			return
		}
	}

	// Row-level security: if the bound table has an owner/tenant-scoped API
	// endpoint, those columns are stamped from the caller's identity and any
	// CSV-supplied value for them is ignored, so an authenticated user cannot
	// attribute imported rows to another owner or tenant.
	var ownerCol, tenantCol string
	_ = projectDB.Reader().QueryRow(
		`SELECT COALESCE(owner_column,''), COALESCE(tenant_column,'')
		 FROM of_api_endpoints
		 WHERE table_name = ? AND (COALESCE(owner_column,'') != '' OR COALESCE(tenant_column,'') != '')
		 LIMIT 1`, table,
	).Scan(&ownerCol, &tenantCol)
	if ownerCol != "" && !validTableName.MatchString(ownerCol) {
		ownerCol = ""
	}
	if tenantCol != "" && !validTableName.MatchString(tenantCol) {
		tenantCol = ""
	}
	stampCols := map[string]bool{}
	if ownerCol != "" {
		stampCols[ownerCol] = true
	}
	if tenantCol != "" {
		stampCols[tenantCol] = true
	}

	// Effective insert columns: the CSV data columns (minus any stamped RLS
	// column) followed by the stamped columns with their claim values.
	var dataIdx []int
	insertCols := make([]string, 0, len(header)+2)
	for i, c := range header {
		if stampCols[c] {
			continue
		}
		dataIdx = append(dataIdx, i)
		insertCols = append(insertCols, c)
	}
	var stampValues []any
	if ownerCol != "" {
		insertCols = append(insertCols, ownerCol)
		stampValues = append(stampValues, authUserID)
	}
	if tenantCol != "" {
		insertCols = append(insertCols, tenantCol)
		stampValues = append(stampValues, authTenantID)
	}

	// Build SQL once.
	cols := strings.Join(insertCols, ", ")
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(insertCols)), ",")
	var stmt string
	if mode == "upsert" && uniqueCol != "" {
		if !validTableName.MatchString(uniqueCol) {
			writeError(w, http.StatusBadRequest, "invalid unique column")
			return
		}
		setClauses := make([]string, 0, len(insertCols))
		for _, c := range insertCols {
			// Skip the conflict key and the stamped RLS columns: an upsert must
			// not rewrite an existing row's owner/tenant to the caller's.
			if c == uniqueCol || stampCols[c] {
				continue
			}
			setClauses = append(setClauses, c+" = excluded."+c)
		}
		stmt = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) DO UPDATE SET %s",
			table, cols, placeholders, uniqueCol, strings.Join(setClauses, ", "))
	} else {
		stmt = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, cols, placeholders)
	}

	tx, err := projectDB.Writer().Begin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "begin tx")
		return
	}
	prep, err := tx.Prepare(stmt)
	if err != nil {
		tx.Rollback()
		writeError(w, http.StatusBadRequest, "prepare: "+err.Error())
		return
	}

	imported := 0
	for {
		if imported >= maxRows {
			break
		}
		record, err := rdr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			tx.Rollback()
			writeError(w, http.StatusBadRequest, "csv parse: "+err.Error())
			return
		}
		if len(record) != len(header) {
			continue
		}
		args := make([]any, 0, len(insertCols))
		for _, idx := range dataIdx {
			args = append(args, record[idx])
		}
		args = append(args, stampValues...)
		if _, err := prep.Exec(args...); err != nil {
			tx.Rollback()
			writeError(w, http.StatusBadRequest, fmt.Sprintf("row %d: %v", imported+1, err))
			return
		}
		imported++
	}
	_ = prep.Close()
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "commit: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success":  true,
		"imported": imported,
	})
}

// requireJWTClaims validates the request's Bearer token against this project and
// returns the caller's user and tenant id. On a missing or invalid token it
// writes the 401 and returns ok=false; callers gate it behind their own
// "is auth required?" condition. This is a strict JWT-only check (no project
// token fallback), matching the CSV export/import handlers' access model.
func (h *Handler) requireJWTClaims(w http.ResponseWriter, r *http.Request, projectID int) (userID, tenantID int, ok bool) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.deps.JWTManager == nil || token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return 0, 0, false
	}
	claims, err := h.validateUserClaims(token)
	if err != nil || claims.ProjectID != projectID {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return 0, 0, false
	}
	return claims.UserID, claims.TenantID, true
}
