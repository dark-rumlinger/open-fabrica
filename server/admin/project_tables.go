package admin

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"openfabrica/security"
	"openfabrica/store"
)

// ProjectTablesHandler handles dynamic table browsing for project detail views.
type ProjectTablesHandler struct {
	deps *Deps
}

// List returns all dynamic tables for a project with row counts.
func (h *ProjectTablesHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListDynamicTables(projectDB))
}

// Rows returns rows from a dynamic table with pagination.
// PASSWORD columns are stripped from the response. ENCRYPTED columns show a placeholder.
func (h *ProjectTablesHandler) Rows(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	tableName := chi.URLParam(r, "tableName")
	if err := security.ValidateTableName(tableName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	page, err := store.QueryDynamicTableRows(
		projectDB,
		tableName,
		r.URL.Query().Get("sort"),
		r.URL.Query().Get("dir"),
		r.URL.Query().Get("q"),
		limit,
		offset,
	)
	if errors.Is(err, store.ErrTableNotFound) {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query table")
		return
	}

	writeJSON(w, http.StatusOK, page)
}

// ExportCSV exports all rows of a table as CSV.
func (h *ProjectTablesHandler) ExportCSV(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	tableName := chi.URLParam(r, "tableName")
	if err := security.ValidateTableName(tableName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}

	header, records, err := store.ExportDynamicTableCSV(projectDB, tableName)
	if errors.Is(err, store.ErrTableNotFound) {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}
	if err != nil && len(header) == 0 {
		writeError(w, http.StatusInternalServerError, "failed to query table")
		return
	}

	startCSV(w, tableName)
	writer := csv.NewWriter(w)
	_ = writer.Write(header)
	for _, rec := range records {
		_ = writer.Write(rec)
	}
	if err != nil {
		h.deps.Logger.Error("table export: row iteration failed", "table", tableName, "error", err)
	}
	writer.Flush()
}

var validColName = security.IdentifierRegex
var allowedTypes = map[string]string{
	"TEXT": "TEXT", "INTEGER": "INTEGER", "REAL": "REAL", "BOOLEAN": "BOOLEAN",
	"PASSWORD": "TEXT", "ENCRYPTED": "TEXT",
}

type addColumnRequest struct {
	ColumnName string `json:"column_name"`
	ColumnType string `json:"column_type"`
	NotNull    bool   `json:"not_null"`
}

// AddColumn adds a new column to an existing dynamic table.
func (h *ProjectTablesHandler) AddColumn(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	tableName := chi.URLParam(r, "tableName")
	if err := security.ValidateTableName(tableName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}
	if store.IsSystemTable(tableName) {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}

	var req addColumnRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validColName.MatchString(req.ColumnName) {
		writeError(w, http.StatusBadRequest, "invalid column name")
		return
	}

	colType := strings.ToUpper(req.ColumnType)
	sqlType, ok := allowedTypes[colType]
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported column type: "+req.ColumnType)
		return
	}

	if err := store.AddDynamicTableColumn(projectDB, tableName, req.ColumnName, colType, sqlType, req.NotNull); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to add column")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"table": tableName, "column_added": req.ColumnName})
}

const utf8BOM = "\xEF\xBB\xBF"

func startCSV(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.csv", name))
	_, _ = w.Write([]byte(utf8BOM))
}
