package data

import (
	"openfabrica/tools"
)

// manage_sql owns the raw SQL escape hatches: SELECT-only execute_sql,
// atomic multi-write transaction, CSV/JSON bulk import, CSV export. Per-row
// CRUD lives in manage_data; DDL in manage_schema.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_sql",
		Category:           "data",
		FullResultInEvents: true,
		OneLiner:           "Build-time only: raw SELECT, atomic multi-write transactions (<=100 ops), CSV/JSON bulk import (<=500 rows) and CSV export.",
		WhenToUse: `BUILD-TIME / admin tool only - it is NOT exposed as a runtime HTTP
route, so never use it to back a query endpoint a page calls. For runtime read-only
SQL (a SQL editor, joins/aggregations on demand) use manage_api enable_query; for a
view's rows use manage_api create over the view.

execute_sql (SELECT only) for joins/aggregations too complex for
manage_data query. transaction when two writes must succeed or fail together
(transfers, ledger entries, order+inventory); inner ops follow the manage_data
write shape (insert/update/upsert/delete/batch_delete) plus a required table_name
and action. Do NOT nest transactions or include DDL (CREATE/ALTER/DROP) - SQLite
DDL isn't transactional and is rejected. For loading rows you already have as
objects, manage_data insert (rows=[...], any count) is simpler; import_csv/import_json
fit data you hold as CSV or a JSON string. For a reusable upload/download endpoint use
manage_import_endpoint/manage_export_endpoint.`,
		Actions: []tools.Action{
			&SQLExecute{},
			&SQLTransaction{},
			&SQLExportCSV{},
			&SQLImportCSV{},
			&SQLImportJSON{},
		},
		// Action is always explicit (required enum at the tool schema level).
		// We deliberately do not infer it from which fields are populated:
		// inference (sql=>execute, operations=>transaction, csv_data=>import_csv,
		// json_data=>import_json) silently picked the wrong action when the LLM
		// filled more than one of those fields.
		Hints: func(action string, _ map[string]any, _ *tools.Result) string {
			switch action {
			case "execute_sql":
				return "SELECT only, no INSERT/UPDATE/DELETE. For complex joins and aggregations use this; for simple lookups prefer manage_data query."
			case "transaction":
				return "Atomic multi-write. Any failure rolls back every prior op. Up to 100 inner ops; allowed actions: insert / update / delete / upsert / batch_delete. Do NOT nest transactions or include schema DDL."
			}
			return ""
		},
		MaxResult: 12000,
		Summarize: MergedSummarize,
	})
}

// SQLExecute runs a read-only SQL query (SELECT only).
type SQLExecute struct {
	SQL   string `arg:"sql,required" desc:"Read-only SQL query. SELECT only; system tables (of_*) are blocked."`
	Limit int    `arg:"limit"        desc:"Max rows returned (default 100, capped at 500 - a larger value is rejected)." default:"100" max:"500"`
}

func (a *SQLExecute) ActionName() string { return "execute_sql" }
func (a *SQLExecute) Doc() string {
	return "Read-only SQL. SELECT only; system tables (of_*) are blocked."
}
func (a *SQLExecute) ReadOnly() bool { return true }
func (a *SQLExecute) Example() any {
	return &SQLExecute{SQL: "SELECT name FROM items LIMIT 5"}
}
func (a *SQLExecute) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataExecuteSQL(ctx, a)
}

// SQLTransaction is the atomic multi-write batch. Any inner failure rolls
// every prior op back. Allowed inner actions: insert/update/delete/upsert/
// batch_delete. No nesting, no schema DDL.
type SQLTransaction struct {
	Operations []any `arg:"operations,required" desc:"Ordered list of write ops, executed atomically. Each item: {\"action\": <\"insert\"|\"update\"|\"upsert\"|\"delete\"|\"batch_delete\">, \"table_name\": \"<table>\", plus the same fields manage_data takes for that action (data, rows, id, filters, conflict_columns, ...). Up to 100 ops. DDL (CREATE/ALTER/DROP) is rejected; use manage_schema outside the transaction."`
}

func (a *SQLTransaction) Doc() string {
	return "Atomic multi-write batch. ANY failure rolls every prior op back."
}
func (a *SQLTransaction) Example() any {
	return &SQLTransaction{Operations: []any{
		map[string]any{"action": "insert", "table_name": "ledger", "data": map[string]any{"amount": 100}},
		map[string]any{"action": "update", "table_name": "accounts", "id": 1, "data": map[string]any{"balance": 900}},
	}}
}
func (a *SQLTransaction) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataTransaction(ctx, a)
}

// SQLExportCSV streams a filtered table as CSV.
type SQLExportCSV struct {
	TableName string `arg:"table_name,required" desc:"Table to export."`
	Filters   []any  `arg:"filters"             desc:"Filter rows: [{\"column\":\"name\",\"op\":\"=\",\"value\":\"foo\"}]."`
	Columns   []any  `arg:"columns"             desc:"Columns to include (all columns if omitted)." items:"string"`
	Limit     int    `arg:"limit"               desc:"Max rows (default 500, capped at 5000 - a larger value is rejected)." default:"500" max:"5000"`
}

func (a *SQLExportCSV) ActionName() string { return "export_csv" }
func (a *SQLExportCSV) Doc() string        { return "Stream a filtered table as CSV." }
func (a *SQLExportCSV) ReadOnly() bool     { return true }
func (a *SQLExportCSV) Example() any {
	return &SQLExportCSV{TableName: "items"}
}
func (a *SQLExportCSV) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataExportCSV(ctx, a)
}

// SQLImportCSV bulk-inserts from a CSV string (first row is the header).
type SQLImportCSV struct {
	TableName  string `arg:"table_name,required" desc:"Target table."`
	CSVData    string `arg:"csv_data,required"   desc:"CSV string with header row."`
	OnConflict string `arg:"on_conflict"         desc:"A row that violates a UNIQUE key or the table's validation rules: error rolls the whole import back; skip ignores that row and imports the rest; update replaces the existing row (delete + insert, so columns absent from the file reset to their defaults). At most 500 rows per call." enum:"error,skip,update" default:"error"`
}

func (a *SQLImportCSV) ActionName() string { return "import_csv" }
func (a *SQLImportCSV) Doc() string        { return "Bulk insert from a CSV string (first row = header)." }
func (a *SQLImportCSV) Example() any {
	return &SQLImportCSV{TableName: "items", CSVData: "name,qty\nBread,5\nMilk,2"}
}
func (a *SQLImportCSV) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataImportCSV(ctx, a)
}

// SQLImportJSON bulk-inserts from a JSON array of objects.
type SQLImportJSON struct {
	TableName  string `arg:"table_name,required" desc:"Target table."`
	JSONData   string `arg:"json_data,required"  desc:"JSON array of objects."`
	OnConflict string `arg:"on_conflict"         desc:"A row that violates a UNIQUE key or the table's validation rules: error rolls the whole import back; skip ignores that row and imports the rest; update replaces the existing row (delete + insert, so columns absent from the file reset to their defaults). At most 500 rows per call." enum:"error,skip,update" default:"error"`
}

func (a *SQLImportJSON) ActionName() string { return "import_json" }
func (a *SQLImportJSON) Doc() string        { return "Bulk insert from a JSON array of objects." }
func (a *SQLImportJSON) Example() any {
	return &SQLImportJSON{TableName: "items", JSONData: `[{"name":"Bread"}]`}
}
func (a *SQLImportJSON) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataImportJSON(ctx, a)
}
