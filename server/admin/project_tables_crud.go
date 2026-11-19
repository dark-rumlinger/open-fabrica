package admin

import (
	"fmt"
	"net/http"

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

	secureCols, err := store.LoadSecureColumns(projectDB.Reader(), tableName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read secure-column projection for this table")
		return
	}

	var quotedCols []string
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
		quotedCols = append(quotedCols, security.QuoteIdent(col))

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

	id, err := store.InsertDynamicTableRow(projectDB, tableName, quotedCols, values)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to insert row")
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

	secureCols, err := store.LoadSecureColumns(projectDB.Reader(), tableName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read secure-column projection for this table")
		return
	}

	var setClauses []string
	var values []any
	changedCols := []string{}

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
			if kind == "hash" {
				if fmt.Sprintf("%v", val) == "" {
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

	revokeUser := tokenrevoke.AccountToRevoke(projectDB.Reader(), tableName, "id", rowID, changedCols)
	if err := store.UpdateDynamicTableRow(projectDB, tableName, rowID, setClauses, values); err != nil {
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

	if !store.DynamicTableExists(projectDB, tableName) {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}

	revokeUser := tokenrevoke.AccountToRevoke(projectDB.Reader(), tableName, "id", rowID, nil)
	if err := store.DeleteDynamicTableRow(projectDB, tableName, rowID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete row")
		return
	}
	h.revokeAccountSessions(projectDB, projectID, revokeUser, "account deleted by the admin")

	writeStatus(w, "deleted")
}

func (h *ProjectTablesHandler) revokeAccountSessions(projectDB *db.ProjectDB, projectID, userID int, why string) {
	if userID <= 0 {
		return
	}
	if err := tokenrevoke.Revoke(projectDB.Writer(), projectID, userID); err != nil {
		h.deps.Logger.Error("could not end the user's existing sessions", "project_id", projectID, "user_id", userID, "reason", why, "error", err)
	}
}
