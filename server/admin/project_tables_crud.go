package admin

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	"openfabrica/internal/tokenrevoke"
	"openfabrica/security"
	"openfabrica/store"
)

type rowRequest struct {
	Data map[string]any `json:"data"`
}

// InsertRow inserts a new row into a dynamic table.
func (h *ProjectTablesHandler) InsertRow(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	tableName := chi.URLParam(r, "tableName")
	if err := security.ValidateTableName(tableName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}

	// Verify the table exists and load its declared column set. Unknown columns
	// must be rejected before they reach Exec: SQLite would error on those, but
	// without this check valid columns alongside an invalid one all get sent
	// through, and a malformed identifier could break the quoted interpolation.
	schemaCols, err := store.LoadTableSchema(projectDB.Reader(), tableName)
	if err != nil {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}

	var req rowRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Data) == 0 {
		writeError(w, http.StatusBadRequest, "data is required")
		return
	}

	// Fail closed: without the projection we can't tell which columns are secret,
	// and skipping the hash/encrypt step below would store the caller's value in a
	// PASSWORD/ENCRYPTED column verbatim.
	secureCols, err := store.LoadSecureColumns(projectDB.Reader(), tableName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read secure-column projection for this table")
		return
	}

	var colNames, placeholders []string
	var values []any

	for col, val := range req.Data {
		if !store.ValidColumnName.MatchString(col) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid column name: %s", col))
			return
		}
		if _, ok := schemaCols[col]; !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown column: %s", col))
			return
		}
		colNames = append(colNames, security.QuoteIdent(col))
		placeholders = append(placeholders, "?")

		if kind, ok := secureCols[col]; ok {
			processed, err := security.ProcessSecureValue(kind, val, h.deps.Encryptor)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("error processing %s: %v", col, err))
				return
			}
			val = processed
		}
		values = append(values, val)
	}

	query := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`,
		security.QuoteIdent(tableName), strings.Join(colNames, ", "), strings.Join(placeholders, ", "))

	res, err := projectDB.ExecWrite(query, values...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to insert row")
		return
	}

	id, err := res.LastInsertId()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get inserted row ID")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// UpdateRow updates a row in a dynamic table.
func (h *ProjectTablesHandler) UpdateRow(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	tableName := chi.URLParam(r, "tableName")
	if err := security.ValidateTableName(tableName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}
	rowID, ok := requireIntParam(w, r, "rowID", "invalid row ID")
	if !ok {
		return
	}

	// Verify the table exists and load its declared column set; see InsertRow.
	schemaCols, err := store.LoadTableSchema(projectDB.Reader(), tableName)
	if err != nil {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}

	var req rowRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Data) == 0 {
		writeError(w, http.StatusBadRequest, "data is required")
		return
	}

	// Fail closed: see InsertRow.
	secureCols, err := store.LoadSecureColumns(projectDB.Reader(), tableName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read secure-column projection for this table")
		return
	}

	var setClauses []string
	var values []any
	changedCols := []string{} // non-nil even when empty: nil means "a delete" to AccountToRevoke

	for col, val := range req.Data {
		if !store.ValidColumnName.MatchString(col) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid column name: %s", col))
			return
		}
		if _, ok := schemaCols[col]; !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown column: %s", col))
			return
		}
		if kind, ok := secureCols[col]; ok {
			// Skip empty password fields (means "no change").
			if kind == "hash" {
				strVal := fmt.Sprintf("%v", val)
				if strVal == "" {
					continue
				}
			}
			processed, err := security.ProcessSecureValue(kind, val, h.deps.Encryptor)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("error processing %s: %v", col, err))
				return
			}
			val = processed
		}
		setClauses = append(setClauses, security.QuoteIdent(col)+" = ?")
		values = append(values, val)
		changedCols = append(changedCols, col)
	}

	if len(setClauses) == 0 {
		writeStatus(w, "no changes")
		return
	}

	// Editing an app user's password, role or standing here ends their sessions, as
	// the same change through the app's own API would.
	revokeUser := tokenrevoke.AccountToRevoke(projectDB.Reader(), tableName, "id", rowID, changedCols)

	values = append(values, rowID)
	query := fmt.Sprintf(`UPDATE %s SET %s WHERE id = ?`,
		security.QuoteIdent(tableName), strings.Join(setClauses, ", "))

	_, err = projectDB.ExecWrite(query, values...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update row")
		return
	}
	h.revokeAccountSessions(projectDB, projectID, revokeUser, "account edited by the admin")

	writeStatus(w, "updated")
}

// DeleteRow deletes a row from a dynamic table.
func (h *ProjectTablesHandler) DeleteRow(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	tableName := chi.URLParam(r, "tableName")
	if err := security.ValidateTableName(tableName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}
	rowID, ok := requireIntParam(w, r, "rowID", "invalid row ID")
	if !ok {
		return
	}

	// Verify the table exists in this project's database.
	var count int
	err := projectDB.QueryRow(
		"SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?",
		tableName,
	).Scan(&count)
	if err != nil || count == 0 {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}

	// A deleted app user's sessions end with the account; resolve who it is first.
	revokeUser := tokenrevoke.AccountToRevoke(projectDB.Reader(), tableName, "id", rowID, nil)

	_, err = projectDB.ExecWrite(
		fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, security.QuoteIdent(tableName)),
		rowID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete row")
		return
	}
	h.revokeAccountSessions(projectDB, projectID, revokeUser, "account deleted by the admin")

	writeStatus(w, "deleted")
}

// revokeAccountSessions ends the sessions of an app user whose account row was just
// changed or deleted (userID 0 means the row was not an account). A failure to
// record it is logged, not returned: the edit itself succeeded.
func (h *ProjectTablesHandler) revokeAccountSessions(projectDB *db.ProjectDB, projectID, userID int, why string) {
	if userID <= 0 {
		return
	}
	if err := tokenrevoke.Revoke(projectDB.Writer(), projectID, userID); err != nil {
		h.deps.Logger.Error("could not end the user's existing sessions", "project_id", projectID, "user_id", userID, "reason", why, "error", err)
	}
}
