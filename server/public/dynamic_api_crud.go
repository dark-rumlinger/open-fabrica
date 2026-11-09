package public

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"openfabrica/events"
	"openfabrica/internal/sqliteretry"
	"openfabrica/internal/tokenrevoke"
	"openfabrica/security"
	"openfabrica/store"
)

// maxAPIRowBodyBytes bounds a single dynamic-API row write (insert/update). A
// row is structured JSON, not a file - large binary belongs on an upload
// endpoint, which has its own limit. This caps memory per write request.
const maxAPIRowBodyBytes = 8 << 20 // 8 MB

// apiList handles GET /api/{path}, list rows with pagination and filtering.
// Column filtering: ?column=value adds WHERE column = value.
// Sorting: ?sort=column&order=asc|desc (default: id DESC).
func (h *Handler) apiList(w http.ResponseWriter, r *http.Request, projectDB *sql.DB, physTable string, ep *apiEndpoint, rc *apiRequestContext) {
	limit := 50
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}

	cols := h.visibleColumns(ep)
	if cols == "" {
		cols = "*"
	}
	if fieldsCols := parseFieldsParam(r, ep); fieldsCols != "" {
		cols = fieldsCols
	}

	// Build WHERE clause from query parameters (column filters).
	// Supports: ?col=val (exact), ?col__like=term, ?col__has=token,
	// ?col__gt=5, ?col__gte=5, ?col__lt=10, ?col__lte=10, ?col__ne=val,
	// ?q=term (full-text across TEXT columns).
	var whereClauses []string
	var whereArgs []any
	for key, vals := range r.URL.Query() {
		if reservedParams[key] || len(vals) == 0 {
			continue
		}
		col, op, isLike, isToken, valid := parseFilterParam(key)
		if !valid {
			continue
		}
		if err := security.ValidateColumnName(col); err != nil {
			continue
		}
		// Skip unknown columns silently so unrelated params (e.g. order_dir=desc)
		// do not end up as a non-existent column in the WHERE clause.
		if !columnExists(projectDB, physTable, col) {
			continue
		}
		if _, ok := ep.SecureCols[col]; ok {
			continue
		}
		// Reject rather than skip: dropping the filter would widen the result.
		if !columnReadable(ep, col) {
			writeError(w, http.StatusBadRequest, "column not available: "+col)
			return
		}
		switch {
		case isToken:
			// Membership in a comma-separated TEXT column: match the value as a
			// whole token, not a substring (so ?tags__has=go matches "go,web"
			// but not "golang").
			token := strings.TrimSpace(vals[0])
			if token == "" {
				continue
			}
			whereClauses = append(whereClauses, tokenMembershipClause(security.QuoteIdent(col)))
			whereArgs = append(whereArgs, tokenMembershipPattern(token))
		case isLike:
			whereClauses = append(whereClauses, security.QuoteIdent(col)+" LIKE ? COLLATE NOCASE")
			whereArgs = append(whereArgs, "%"+vals[0]+"%")
		default:
			whereClauses = append(whereClauses, security.QuoteIdent(col)+" "+op+" ?")
			whereArgs = append(whereArgs, coerceFilterValue(columnType(projectDB, physTable, col), vals[0]))
		}
	}

	// ?q=term, search across all TEXT columns.
	if qTerm := r.URL.Query().Get("q"); qTerm != "" {
		textCols := getTextColumns(projectDB, physTable, ep)
		if len(textCols) > 0 {
			var orClauses []string
			for _, tc := range textCols {
				orClauses = append(orClauses, security.QuoteIdent(tc)+" LIKE ? COLLATE NOCASE")
				whereArgs = append(whereArgs, "%"+qTerm+"%")
			}
			whereClauses = append(whereClauses, "("+strings.Join(orClauses, " OR ")+")")
		}
	}

	// Row-level security: inject owner and tenant filters. The leading " AND "
	// is stripped because whereClauses are joined with AND below.
	if rc.OwnerFilter != "" {
		whereClauses = append(whereClauses, strings.TrimPrefix(rc.OwnerFilter, " AND "))
		whereArgs = append(whereArgs, rc.OwnerFilterVal)
	}
	if rc.TenantFilter != "" {
		whereClauses = append(whereClauses, strings.TrimPrefix(rc.TenantFilter, " AND "))
		whereArgs = append(whereArgs, rc.TenantFilterVal)
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = " WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Default newest-first by id. Views (read-only endpoints) may have no id
	// column, so fall back to no explicit ordering rather than emitting an
	// "ORDER BY id" that errors at the SQLite layer. Tables always have id, so
	// their behaviour is unchanged.
	orderSQL := ""
	if !ep.ReadOnly || columnExists(projectDB, physTable, "id") {
		orderSQL = `"id" DESC`
	}
	sortCol := r.URL.Query().Get("sort")
	if sortCol == "" {
		sortCol = r.URL.Query().Get("order_by")
	}
	if sortCol != "" {
		if security.ValidateColumnName(sortCol) == nil && columnExists(projectDB, physTable, sortCol) {
			if !columnReadable(ep, sortCol) {
				writeError(w, http.StatusBadRequest, "column not available: "+sortCol)
				return
			}
			dir := "ASC"
			orderDir := r.URL.Query().Get("order")
			if orderDir == "" {
				orderDir = r.URL.Query().Get("direction")
			}
			if orderDir == "" {
				orderDir = r.URL.Query().Get("order_direction")
			}
			if orderDir == "" {
				orderDir = r.URL.Query().Get("order_dir")
			}
			if strings.EqualFold(orderDir, "desc") {
				dir = "DESC"
			}
			orderSQL = security.QuoteIdent(sortCol) + " " + dir
		}
	}

	orderByClause := ""
	if orderSQL != "" {
		orderByClause = " ORDER BY " + orderSQL
	}
	query := fmt.Sprintf("SELECT %s FROM %s%s%s LIMIT ? OFFSET ?", quoteColumnList(cols), security.QuoteIdent(physTable), whereSQL, orderByClause)
	args := append(whereArgs, limit, offset)
	ctx, cancel := context.WithTimeout(r.Context(), dynamicAPIQueryTimeout)
	defer cancel()
	rows, err := projectDB.QueryContext(ctx, query, args...) // #nosec G701 -- identifiers are QuoteIdent'd/validated; every value is a ? placeholder
	if err != nil {
		slog.Error("public API list query failed", "table", physTable, "error", err)
		if strings.Contains(err.Error(), "no such column") {
			writeError(w, http.StatusBadRequest, "invalid filter")
		} else {
			writeError(w, http.StatusInternalServerError, "query error")
		}
		return
	}
	defer rows.Close()

	results := h.scanRowsToMaps(rows)
	stripSecureCols(results, ep.SecureCols)
	// The LIST array is `rows`. It is deliberately NOT also exposed as `data`:
	// two same-named `.data` fields (this envelope + a caller's own `{resp, data}`
	// wrapper) is exactly the collision that throws `x.map is not a function`.
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":   results,
		"count":  len(results),
		"limit":  limit,
		"offset": offset,
	})
}

// apiGetOne handles GET /api/{path}/{id}, get single row.
func (h *Handler) apiGetOne(w http.ResponseWriter, r *http.Request, projectDB *sql.DB, physTable, rowID string, ep *apiEndpoint, rc *apiRequestContext) {
	cols := h.visibleColumns(ep)
	if cols == "" {
		cols = "*"
	}
	if fieldsCols := parseFieldsParam(r, ep); fieldsCols != "" {
		cols = fieldsCols
	}

	lookupCol, lookupVal := rowIDColumn(projectDB, physTable, rowID, ep)
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s = ?", quoteColumnList(cols), security.QuoteIdent(physTable), security.QuoteIdent(lookupCol))
	args := []any{lookupVal}
	query, args = rc.applyRLS(query, args)
	ctx, cancel := context.WithTimeout(r.Context(), dynamicAPIQueryTimeout)
	defer cancel()
	rows, err := projectDB.QueryContext(ctx, query, args...) // #nosec G701 -- identifiers are QuoteIdent'd/validated; every value is a ? placeholder
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query error")
		return
	}
	defer rows.Close()

	results := h.scanRowsToMaps(rows)
	if len(results) == 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	stripSecureCols(results, ep.SecureCols)
	writeJSON(w, http.StatusOK, results[0])
}

// apiInsert handles POST /api/{path}, insert a row.
func (h *Handler) apiInsert(w http.ResponseWriter, r *http.Request, projectDB *sql.DB, physTable string, ep *apiEndpoint, projectID int, endpointPath string, rc *apiRequestContext) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIRowBodyBytes)
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	delete(body, "id")
	restrictWritable(ep, body)
	if !enforceRowValidation(w, projectDB, ep.TableName, body) {
		return
	}

	// Row-level security: auto-set owner column on insert.
	if ep.OwnerColumn != "" && rc.AuthUserID > 0 {
		body[ep.OwnerColumn] = rc.AuthUserID
	}
	// Tenant column is stamped from the JWT, never trust a body-supplied value.
	if ep.TenantColumn != "" && rc.AuthTenantID > 0 {
		body[ep.TenantColumn] = rc.AuthTenantID
	}

	for col, kind := range ep.SecureCols {
		if val, ok := body[col]; ok {
			processed, err := security.ProcessSecureValue(kind, val, h.deps.Encryptor)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("error processing %s: %s", col, err))
				return
			}
			body[col] = processed
		}
	}

	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "no data provided")
		return
	}

	// Validate that columns in the request body actually exist in the table.
	type colMeta struct {
		notNull bool
		hasDflt bool
		isPK    bool
	}
	var validCols map[string]colMeta
	if !validTableName.MatchString(physTable) {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}
	rows, err := projectDB.Query(fmt.Sprintf("PRAGMA table_info(%s)", physTable))
	if err == nil {
		validCols = make(map[string]colMeta)
		for rows.Next() {
			var cid int
			var name, colType string
			var notNull int
			var dfltValue sql.NullString
			var pk int
			if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err == nil {
				validCols[name] = colMeta{
					notNull: notNull != 0,
					hasDflt: dfltValue.Valid,
					isPK:    pk != 0,
				}
			}
		}
		rows.Close()
		if rows.Err() != nil {
			// Treat a failed iteration like a failed PRAGMA query: skip column
			// validation and let the INSERT surface any real problem.
			validCols = nil
		}
		if len(validCols) > 0 {
			for col := range body {
				if col == "id" || col == "created_at" {
					continue
				}
				if _, ok := validCols[col]; !ok {
					slog.Warn("public API insert: unknown column ignored", "table", physTable, "column", col)
					delete(body, col)
				}
			}
			if len(body) == 0 {
				writeError(w, http.StatusBadRequest, "no valid columns provided")
				return
			}
		}
	}

	// Auto-inject auth user ID for missing NOT NULL columns that FK-reference the auth table.
	if validCols != nil {
		var missingNotNull []string
		for col, meta := range validCols {
			if meta.notNull && !meta.hasDflt && !meta.isPK && col != "id" && col != "created_at" {
				if _, provided := body[col]; !provided {
					missingNotNull = append(missingNotNull, col)
				}
			}
		}
		if len(missingNotNull) > 0 && rc.AuthUserID > 0 {
			fkRef := make(map[string]string)
			if fkRows, fkErr := projectDB.Query(fmt.Sprintf("PRAGMA foreign_key_list(%s)", physTable)); fkErr == nil {
				for fkRows.Next() {
					var id, seq int
					var refTable, from, to, onUpdate, onDelete, match string
					if fkRows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match) == nil {
						fkRef[from] = refTable
					}
				}
				fkRows.Close()
				if fkRows.Err() != nil {
					// Treat like a failed PRAGMA query: no FK info.
					fkRef = map[string]string{}
				}
			}
			authTables := make(map[string]bool)
			if authRows, authErr := projectDB.Query("SELECT table_name FROM of_auth_endpoints"); authErr == nil {
				for authRows.Next() {
					var tbl string
					if authRows.Scan(&tbl) == nil {
						authTables[tbl] = true
					}
				}
				authRows.Close()
				if authRows.Err() != nil {
					// Treat like a failed query: no known auth tables.
					authTables = map[string]bool{}
				}
			}
			var stillMissing []string
			for _, col := range missingNotNull {
				if refTable, hasFk := fkRef[col]; hasFk && authTables[refTable] {
					body[col] = rc.AuthUserID
				} else {
					stillMissing = append(stillMissing, col)
				}
			}
			if len(stillMissing) > 0 {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("missing required fields: %s", strings.Join(stillMissing, ", ")))
				return
			}
		} else if len(missingNotNull) > 0 && rc.AuthUserID == 0 {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("missing required fields: %s", strings.Join(missingNotNull, ", ")))
			return
		}
	}

	columns := make([]string, 0, len(body))
	placeholders := make([]string, 0, len(body))
	values := make([]any, 0, len(body))
	for col, val := range body {
		if err := security.ValidateColumnName(col); err != nil {
			writeError(w, http.StatusBadRequest, "invalid field name: "+col)
			return
		}
		columns = append(columns, security.QuoteIdent(col))
		placeholders = append(placeholders, "?")
		values = append(values, val)
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", security.QuoteIdent(physTable), strings.Join(columns, ", "), strings.Join(placeholders, ", "))
	result, err := sqliteretry.Exec(projectDB, query, values...)
	if err != nil {
		slog.Error("public API insert failed", "table", physTable, "error", err)
		writeError(w, http.StatusBadRequest, "insert failed")
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		slog.Error("public API insert succeeded but reading new id failed", "table", physTable, "error", err)
		writeError(w, http.StatusInternalServerError, "insert failed")
		return
	}

	h.publishRowEvent(events.EventDataInsert, projectID, rowEventTarget{PhysTable: physTable, Endpoint: endpointPath}, id,
		eventFields(h.rowSnapshotForEvent(projectDB, ep, physTable, "id", id), body))

	if h.writeReadBackRow(w, projectDB, ep, physTable, "id", id, http.StatusCreated) {
		return
	}

	// The insert committed; the read-back failed. Answer with the row shape the
	// contract promises (its id), never a different envelope.
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// apiUpdate handles PUT /api/{path}/{id}, update a row.
func (h *Handler) apiUpdate(w http.ResponseWriter, r *http.Request, projectDB *sql.DB, physTable, rowID string, ep *apiEndpoint, projectID int, endpointPath string, rc *apiRequestContext) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIRowBodyBytes)
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	delete(body, "id")
	restrictWritable(ep, body)
	if !enforceRowValidation(w, projectDB, ep.TableName, body) {
		return
	}

	// Row-level security: never let a scoped client move a row to another owner
	// or tenant via PUT. Insert stamps these columns from the JWT (apiInsert);
	// update must not trust a body-supplied value, or a user could reassign their
	// own row's owner_id or inject it into another tenant's scope. Stripped only
	// when an identity is present, mirroring apiInsert's stamping condition so a
	// project API key (owner-level access) keeps full control.
	if ep.OwnerColumn != "" && rc.AuthUserID > 0 {
		delete(body, ep.OwnerColumn)
	}
	if ep.TenantColumn != "" && rc.AuthTenantID > 0 {
		delete(body, ep.TenantColumn)
	}

	for col, kind := range ep.SecureCols {
		if val, ok := body[col]; ok {
			processed, err := security.ProcessSecureValue(kind, val, h.deps.Encryptor)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("error processing %s: %s", col, err))
				return
			}
			body[col] = processed
		}
	}

	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "no data provided")
		return
	}

	setClauses := make([]string, 0, len(body))
	values := make([]any, 0, len(body)+1)
	for col, val := range body {
		if err := security.ValidateColumnName(col); err != nil {
			writeError(w, http.StatusBadRequest, "invalid field name: "+col)
			return
		}
		setClauses = append(setClauses, security.QuoteIdent(col)+" = ?")
		values = append(values, val)
	}
	lookupCol, lookupVal := rowIDColumn(projectDB, physTable, rowID, ep)
	values = append(values, lookupVal)

	// An account row whose password, role or standing is being changed: its existing
	// sessions end once the change lands (resolved first, in case the update moves
	// the lookup column itself).
	changedCols := make([]string, 0, len(body))
	for col := range body {
		changedCols = append(changedCols, col)
	}
	revokeUser := tokenrevoke.AccountToRevoke(projectDB, physTable, lookupCol, lookupVal, changedCols)

	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s = ?", security.QuoteIdent(physTable), strings.Join(setClauses, ", "), security.QuoteIdent(lookupCol))
	query, values = rc.applyRLS(query, values)
	result, err := sqliteretry.Exec(projectDB, query, values...)
	if err != nil {
		slog.Error("public API update failed", "table", physTable, "error", err)
		writeError(w, http.StatusBadRequest, "update failed")
		return
	}

	n, _ := result.RowsAffected()
	if n == 0 {
		// Upsert: a PUT addressed by a non-id unique lookup column (slug, uuid,
		// lookup_column, or an auto-detected single UNIQUE TEXT column) creates the
		// row when it does not yet exist, giving generated key-value / settings
		// tables clean idempotent "set by key" semantics. Without this the client
		// has to POST-then-PUT, and the POST logs a UNIQUE-constraint 400 on every
		// update of an existing key. Numeric-id PUTs still 404: you cannot
		// meaningfully create a row at a caller-chosen autoincrement id.
		if lookupCol != "id" {
			h.apiUpsertInsert(w, upsertInsertParams{
				ProjectDB:    projectDB,
				PhysTable:    physTable,
				LookupCol:    lookupCol,
				LookupVal:    lookupVal,
				Body:         body,
				Endpoint:     ep,
				ProjectID:    projectID,
				EndpointPath: endpointPath,
				RC:           rc,
			})
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	if revokeUser > 0 {
		h.revokeUserSessions(projectDB, projectID, revokeUser, "account updated")
	}

	h.publishRowEvent(events.EventDataUpdate, projectID, rowEventTarget{PhysTable: physTable, Endpoint: endpointPath}, rowID,
		eventFields(h.rowSnapshotForEvent(projectDB, ep, physTable, lookupCol, lookupVal), body))

	if h.writeReadBackRow(w, projectDB, ep, physTable, lookupCol, lookupVal, http.StatusOK) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": rowID})
}

// apiUpsertInsert creates the row a PUT targeted by lookup key when no existing row
// matched the (owner-scoped) UPDATE. It composes the new row from the PUT body, the URL
// key, and the caller's owner/tenant stamp - as apiCreate does - so an upserted row is
// owned by the caller. Because the prior UPDATE was owner-scoped, reaching here means
// the key is free or owned by someone else; in the latter case the INSERT hits the
// UNIQUE constraint and returns 404 rather than overwriting a row the caller cannot see.
// body already had its secure columns processed by apiUpdate.
//
// upsertInsertParams bundles the row/endpoint context, replacing a 10-parameter
// signature (w plus nine positional values, three of them adjacent strings).
type upsertInsertParams struct {
	ProjectDB    *sql.DB
	PhysTable    string
	LookupCol    string
	LookupVal    any
	Body         map[string]any
	Endpoint     *apiEndpoint
	ProjectID    int
	EndpointPath string
	RC           *apiRequestContext
}

func (h *Handler) apiUpsertInsert(w http.ResponseWriter, p upsertInsertParams) {
	projectDB := p.ProjectDB
	physTable := p.PhysTable
	lookupCol := p.LookupCol
	lookupVal := p.LookupVal
	body := p.Body
	ep := p.Endpoint
	projectID := p.ProjectID
	endpointPath := p.EndpointPath
	rc := p.RC

	insert := make(map[string]any, len(body)+3)
	for k, v := range body {
		insert[k] = v
	}
	insert[lookupCol] = lookupVal
	// Owner/tenant stamped from the JWT, never trusted from the body (mirrors apiCreate).
	if ep.OwnerColumn != "" && rc.AuthUserID > 0 {
		insert[ep.OwnerColumn] = rc.AuthUserID
	}
	if ep.TenantColumn != "" && rc.AuthTenantID > 0 {
		insert[ep.TenantColumn] = rc.AuthTenantID
	}

	columns := make([]string, 0, len(insert))
	placeholders := make([]string, 0, len(insert))
	values := make([]any, 0, len(insert))
	for col, val := range insert {
		if err := security.ValidateColumnName(col); err != nil {
			writeError(w, http.StatusBadRequest, "invalid field name: "+col)
			return
		}
		columns = append(columns, security.QuoteIdent(col))
		placeholders = append(placeholders, "?")
		values = append(values, val)
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", security.QuoteIdent(physTable), strings.Join(columns, ", "), strings.Join(placeholders, ", "))
	result, err := sqliteretry.Exec(projectDB, query, values...)
	if err != nil {
		// The key exists but belongs to another owner (the owner-scoped UPDATE
		// missed it): stay a 404 instead of overwriting an invisible row.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		slog.Error("public API upsert insert failed", "table", physTable, "error", err)
		writeError(w, http.StatusBadRequest, "update failed")
		return
	}

	id, _ := result.LastInsertId()
	h.publishRowEvent(events.EventDataInsert, projectID, rowEventTarget{PhysTable: physTable, Endpoint: endpointPath}, id,
		eventFields(h.rowSnapshotForEvent(projectDB, ep, physTable, lookupCol, lookupVal), insert))

	if h.writeReadBackRow(w, projectDB, ep, physTable, lookupCol, lookupVal, http.StatusOK) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// writeReadBackRow re-reads the row identified by lookupCol=lookupVal through the
// endpoint's visible-column projection, strips secure columns, and writes it as
// the response with status. Returns true when it wrote a row; on a read error or
// a missing row it writes nothing and returns false, so the caller falls back to
// a minimal ack.
func (h *Handler) writeReadBackRow(w http.ResponseWriter, projectDB *sql.DB, ep *apiEndpoint, physTable, lookupCol string, lookupVal any, status int) bool {
	cols := h.visibleColumns(ep)
	if cols == "" {
		cols = "*"
	}
	readBack, err := projectDB.Query(fmt.Sprintf("SELECT %s FROM %s WHERE %s = ?", quoteColumnList(cols), security.QuoteIdent(physTable), security.QuoteIdent(lookupCol)), lookupVal)
	if err != nil {
		return false
	}
	rows := h.scanRowsToMaps(readBack)
	if len(rows) == 0 {
		return false
	}
	stripSecureCols(rows, ep.SecureCols)
	writeJSON(w, status, rows[0])
	return true
}

// rowSnapshotForEvent reads the row back so a data.* event carries the row as it
// actually landed, not just the request body. The body is missing every
// server-generated value - column DEFAULTs, created_at, the owner/tenant stamp -
// and on a PUT it holds only the columns that changed, so a subscriber that
// rendered it straight (an SSE feed, a {{template}} in a trigger) got undefined
// fields or a half-blank row. Same visibility rules as the HTTP response, so a
// secure column never reaches the bus. Returns nil when the read-back fails; the
// caller then falls back to the request body rather than losing the event.
func (h *Handler) rowSnapshotForEvent(projectDB *sql.DB, ep *apiEndpoint, physTable, lookupCol string, lookupVal any) map[string]any {
	cols := h.visibleColumns(ep)
	if cols == "" {
		cols = "*"
	}
	readBack, err := projectDB.Query(fmt.Sprintf("SELECT %s FROM %s WHERE %s = ?", quoteColumnList(cols), security.QuoteIdent(physTable), security.QuoteIdent(lookupCol)), lookupVal)
	if err != nil {
		return nil
	}
	rows := h.scanRowsToMaps(readBack)
	if len(rows) == 0 {
		return nil
	}
	stripSecureCols(rows, ep.SecureCols)
	return rows[0]
}

// eventFields prefers the read-back snapshot and falls back to the request body.
func eventFields(snapshot, body map[string]any) map[string]any {
	if snapshot != nil {
		return snapshot
	}
	return body
}

// apiDelete handles DELETE /api/{path}/{id}, delete a row.
func (h *Handler) apiDelete(w http.ResponseWriter, projectDB *sql.DB, physTable, rowID string, ep *apiEndpoint, projectID int, endpointPath string, rc *apiRequestContext) {
	lookupCol, lookupVal := rowIDColumn(projectDB, physTable, rowID, ep)
	// A deleted account's sessions end with it; resolve who it is before the row goes.
	revokeUser := tokenrevoke.AccountToRevoke(projectDB, physTable, lookupCol, lookupVal, nil)
	query := fmt.Sprintf("DELETE FROM %s WHERE %s = ?", security.QuoteIdent(physTable), security.QuoteIdent(lookupCol))
	args := []any{lookupVal}
	query, args = rc.applyRLS(query, args)
	result, err := sqliteretry.Exec(projectDB, query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}

	n, _ := result.RowsAffected()
	if n == 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if revokeUser > 0 {
		h.revokeUserSessions(projectDB, projectID, revokeUser, "account deleted")
	}

	h.publishRowEvent(events.EventDataDelete, projectID, rowEventTarget{PhysTable: physTable, Endpoint: endpointPath}, rowID, nil)

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// rowEventTarget names the two identifiers a data.* event carries. Both are
// strings and an endpoint is routinely created at a path equal to its table, so
// adjacent positional args would transpose without any example noticing. Named
// fields make that transposition unrepresentable.
type rowEventTarget struct {
	PhysTable string // the row's physical table: what a trigger's event_filter {"table":...} matches
	Endpoint  string // the endpoint path that served the write
}

// publishRowEvent publishes a data.* change event for a row when an event bus is
// configured. The payload carries the target's table and endpoint plus the row id;
// any entries in fields (which may be nil) are merged in first, so a row column
// named `table`/`endpoint`/`id` cannot overwrite a reserved key and spoof a filter
// match.
func (h *Handler) publishRowEvent(evType events.EventType, projectID int, tgt rowEventTarget, id any, fields map[string]any) {
	if h.deps.Bus == nil {
		return
	}
	payload := map[string]any{}
	for k, v := range fields {
		payload[k] = v
	}
	payload["table"] = tgt.PhysTable
	payload["endpoint"] = tgt.Endpoint
	payload["id"] = eventRowID(id)
	h.deps.Bus.Publish(events.NewEvent(evType, projectID, payload))
}

// eventRowID types the id a row event carries the way the REST API returns it.
// Update and delete receive the id as the URL segment ("42"); a numeric segment
// always addresses the integer id column (see rowIDColumn), which GET and list
// answer as a number. Published as the string, every live client keying its rows
// by id (a Map filled from GET, updated from the stream) held the same order twice
// - 42 and "42" - and never saw an update or delete land. A non-numeric lookup
// value (slug, uuid) stays a string, as it is in the row.
func eventRowID(id any) any {
	s, ok := id.(string)
	if !ok {
		return id
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && strconv.FormatInt(n, 10) == s {
		return n
	}
	return id
}

// applyRLS appends the row-level-security owner/tenant filters, each already
// carrying its leading " AND " clause, to query and args, preserving arg order.
func (rc *apiRequestContext) applyRLS(query string, args []any) (string, []any) {
	if rc.OwnerFilter != "" {
		query += rc.OwnerFilter
		args = append(args, rc.OwnerFilterVal)
	}
	if rc.TenantFilter != "" {
		query += rc.TenantFilter
		args = append(args, rc.TenantFilterVal)
	}
	return query, args
}

// enforceRowValidation applies the table's set_validation rules to a client-written
// body, before secure columns are hashed or encrypted. A failure answers 400 with the
// rule messages and the per-field list a form can render beside its inputs, and
// reports false so the caller stops. A table with no rules always passes.
func enforceRowValidation(w http.ResponseWriter, projectDB *sql.DB, table string, body map[string]any) bool {
	rules := store.LoadValidationRules(projectDB, table)
	if len(rules) == 0 {
		return true
	}
	errs := store.ValidateRow(rules, body)
	if len(errs) == 0 {
		return true
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":  store.ValidationMessage(errs),
		"fields": errs,
	})
	return false
}
