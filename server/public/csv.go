package public

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"openfabrica/db"
	"openfabrica/internal/csvsafe"
	"openfabrica/security"
	"openfabrica/store"
)

// apiCSVExport streams a CSV of the bound table at GET /api/<path>/export.
func (h *Handler) apiCSVExport(w http.ResponseWriter, r *http.Request, projectID int, projectDB *sql.DB, path string) {
	ep, err := store.LoadExportEndpoint(projectDB, path)
	if err != nil {
		writeError(w, http.StatusNotFound, "export endpoint not found")
		return
	}
	table := ep.TableName
	publicColsJSON := ep.PublicCols
	requiresAuth := ep.RequiresAuth
	ownerCol := sql.NullString{String: ep.OwnerColumn, Valid: ep.OwnerColumn != ""}
	tenantCol := sql.NullString{String: ep.TenantColumn, Valid: ep.TenantColumn != ""}

	authUserID := 0
	authTenantID := 0
	var authUserRole, authTenantRole string
	if requiresAuth || ownerCol.String != "" || tenantCol.String != "" {
		claims, ok := h.requireJWTClaims(w, r, projectID)
		if !ok {
			return
		}
		authUserID = claims.UserID
		authTenantID = claims.TenantID
		authUserRole = claims.Role
		authTenantRole = claims.TenantRole
	}

	// A tenant-scoped export must run under an active tenant session; without
	// one the tenant filter below cannot bind and the export would stream every
	// tenant's rows. Mirrors the CRUD RLS tenancy gate and the import handler.
	if tenantCol.String != "" && authTenantID == 0 && authTenantRole != "platform_admin" {
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
		if info, err := store.GetTableColumnInfo(projectDB, table); err == nil {
			for _, c := range info {
				if !strings.EqualFold(c.Type, "PASSWORD") {
					publicCols = append(publicCols, c.Name)
				}
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

	args := []any{}
	var whereClauses []string
	if ownerCol.String != "" && authUserID > 0 && authUserRole != "admin" {
		clause, err := security.OwnerFilterClause(ownerCol.String)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid owner column")
			return
		}
		whereClauses = append(whereClauses, clause)
		args = append(args, authUserID)
	}
	// Tenant scope stacks on top of owner scope (matches the CRUD RLS path).
	// platform_admin bypasses tenant scoping.
	if tenantCol.String != "" && authTenantID > 0 && authTenantRole != "platform_admin" {
		clause, err := security.TenantFilterClause(tenantCol.String)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid tenant column")
			return
		}
		whereClauses = append(whereClauses, clause)
		args = append(args, authTenantID)
	}

	var cw *csv.Writer
	started := false
	err = store.StreamCSVExport(projectDB, store.CSVExportQuery{
		Table:        table,
		Columns:      publicCols,
		WhereClauses: whereClauses,
		WhereArgs:    args,
	}, func() {
		started = true
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(path)+`.csv"`)
		cw = csv.NewWriter(w)
		_ = cw.Write(publicCols) // streaming to the client; write errors are unactionable
	}, func(raw []string) error {
		record := make([]string, len(raw))
		for i, v := range raw {
			record[i] = csvsafe.Cell(v)
		}
		_ = cw.Write(record)
		return nil
	})
	if err != nil {
		if !started {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		// Headers and rows are already streamed; deliver what we have.
		slog.Warn("csv export: row iteration failed", "path", path, "error", err)
	}
	if cw != nil {
		cw.Flush()
	}
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
	imp, err := store.LoadImportEndpoint(projectDB.Reader(), path)
	if err != nil {
		writeError(w, http.StatusNotFound, "import endpoint not found")
		return
	}
	table, mode, uniqueCol, requiresAuth, maxRows := imp.TableName, imp.Mode, imp.UniqueColumn, imp.RequiresAuth, imp.MaxRows

	var authUserID, authTenantID int
	if requiresAuth {
		claims, ok := h.requireJWTClaims(w, r, projectID)
		if !ok {
			return
		}
		authUserID = claims.UserID
		authTenantID = claims.TenantID
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
	ownerCol, tenantCol, err := store.FindTableRLSScopeColumns(projectDB.Reader(), table)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "import endpoint misconfigured")
		return
	}
	for _, c := range []string{ownerCol, tenantCol} {
		if c != "" && !validTableName.MatchString(c) {
			writeError(w, http.StatusInternalServerError, "import endpoint misconfigured")
			return
		}
	}
	// A scoped table needs a caller to stamp; an anonymous import would write
	// rows owned by nobody.
	if (ownerCol != "" && authUserID == 0) || (tenantCol != "" && authTenantID == 0) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// PASSWORD and ENCRYPTED columns are hashed or encrypted like any other write.
	// Fail closed when the registry cannot be read.
	secureCols, err := store.LoadSecureColumns(projectDB.Reader(), table)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "import endpoint misconfigured")
		return
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

	if mode == "upsert" && uniqueCol != "" && !validTableName.MatchString(uniqueCol) {
		writeError(w, http.StatusBadRequest, "invalid unique column")
		return
	}

	imported, err := store.RunCSVImport(projectDB.Writer(), store.CSVImportSpec{
		Table:        table,
		Columns:      insertCols,
		Mode:         mode,
		UniqueColumn: uniqueCol,
		StampColumns: stampCols,
	}, func(rowNum int) ([]any, error) {
		if rowNum > maxRows {
			return nil, io.EOF
		}
		record, err := rdr.Read()
		if err != nil {
			return nil, err
		}
		if len(record) != len(header) {
			return nil, nil
		}
		args := make([]any, 0, len(insertCols))
		for _, idx := range dataIdx {
			val, serr := security.ProcessSecureValue(secureCols[header[idx]], record[idx], h.deps.Encryptor)
			if serr != nil {
				return nil, fmt.Errorf("secure: %w", serr)
			}
			args = append(args, val)
		}
		args = append(args, stampValues...)
		return args, nil
	})
	if err != nil {
		var impErr *store.CSVImportError
		if errors.As(err, &impErr) {
			switch impErr.Stage {
			case "begin":
				writeError(w, http.StatusInternalServerError, "begin tx")
			case "prepare":
				writeError(w, http.StatusBadRequest, "prepare: "+impErr.Wrapped.Error())
			case "caller":
				if strings.HasPrefix(impErr.Wrapped.Error(), "secure: ") {
					writeError(w, http.StatusInternalServerError, "import failed")
				} else {
					writeError(w, http.StatusBadRequest, "csv parse: "+impErr.Wrapped.Error())
				}
			case "row":
				writeError(w, http.StatusBadRequest, fmt.Sprintf("row %d: %v", impErr.RowNum, impErr.Wrapped))
			case "commit":
				writeError(w, http.StatusInternalServerError, "commit: "+impErr.Wrapped.Error())
			}
			return
		}
		writeError(w, http.StatusInternalServerError, "import failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success":  true,
		"imported": imported,
	})
}

// requireJWTClaims validates the request's Bearer token against this project and
// returns the caller's claims. On a missing or invalid token it writes the 401
// and returns ok=false; callers gate it behind their own "is auth required?"
// condition. This is a strict JWT-only check (no project token fallback),
// matching the CSV export/import handlers' access model.
func (h *Handler) requireJWTClaims(w http.ResponseWriter, r *http.Request, projectID int) (*security.Claims, bool) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.deps.JWTManager == nil || token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	claims, err := h.validateUserClaims(token)
	if err != nil || claims.ProjectID != projectID {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return nil, false
	}
	return claims, true
}
