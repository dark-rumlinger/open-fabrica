package data

import (
	"openfabrica/tools"
)

// manage_data is the row-level CRUD surface against project tables. DDL
// lives in manage_schema; raw SQL and bulk CSV/JSON live in manage_sql.
// Handlers live in data_*.go; this file is the schema declaration that
// builds args maps and delegates to the existing exported handler funcs.
//
// One arg struct per action - each exposes only the fields its handler reads, so
// the schema itself says which fields belong to which action. A shared union
// would force every field optional and carry no such hint.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_data",
		Category:           "data",
		FullResultInEvents: true,
		OneLiner:           "Query and write rows on a single project table - CRUD, count, aggregate, FTS search.",
		WhenToUse: `Use for reading and writing rows in tables you (or the plan) already created.
DDL belongs on manage_schema; raw SELECT or atomic multi-table writes belong on
manage_sql. action=search runs keyword FTS over the table's searchable_columns; reach
for manage_keyword_search to create and manage a dedicated FTS5 index for ranked
search, manage_vector_search for semantic similarity.`,
		Actions: []tools.Action{
			&DataActQuery{},
			&DataActInsert{},
			&DataActUpsert{},
			&DataActUpdate{},
			&DataActBatchUpdate{},
			&DataActDelete{},
			&DataActBatchDelete{},
			&DataActCount{},
			&DataActAggregate{},
			&DataActSearch{},
		},
		Hints: func(action string, _ map[string]any, _ *tools.Result) string {
			switch action {
			case "insert":
				return "Bulk insert: pass every row in one rows=[{},{}] call - over 100 rows auto-splits into 100-row batches, so never hand-split a seed. Give every row ALL its NOT NULL, no-default columns - a truncated row fails the whole batch with a per-row preflight error. Insert parent tables before child tables - foreign keys are enforced."
			case "update":
				return "Provide id OR filters (filters: [{column, op, value}]), never both. For many updates use batch_update (up to 100/call)."
			case "delete":
				return "delete removes exactly ONE row by id. To delete by criteria use batch_delete with filters ([{column, op, value}]) - delete does not accept filters."
			case "query":
				return "filters: [{column, op, value}] (ops: =, !=, <, >, <=, >=, LIKE, NOT LIKE, IS NULL, IS NOT NULL). Default limit 50; set explicitly when you need more or fewer."
			case "upsert":
				return "conflict_columns is required: it names the column(s) whose match triggers update instead of insert."
			case "search":
				return "FTS5 search runs against the table's searchable_columns. If the table has none the call fails: add the index after the fact with manage_keyword_search action=create_index (searchable_columns itself is a create_table arg, so manage_schema cannot add one to a table that already exists), or use a plain query with op=LIKE."
			}
			return ""
		},
		MaxResult: 12000,
		Summarize: MergedSummarize,
	})
}

// --- query ------------------------------------------------------------------

type DataActQuery struct {
	TableName string         `arg:"table_name,required" desc:"Project table name (snake_case)."`
	Filters   []any          `arg:"filters"             desc:"Filter array: each item {column, op, value}. Ops: =, !=, <, >, <=, >=, LIKE, NOT LIKE, IS NULL, IS NOT NULL."`
	OrderBy   map[string]any `arg:"order_by"            desc:"Sort spec: {column, direction} (direction=ASC|DESC)."`
	Limit     int            `arg:"limit"               desc:"Max rows returned. Default 50, capped at 500 - a larger value is rejected." default:"50" max:"500"`
}

func (a *DataActQuery) ActionName() string { return "query" }
func (a *DataActQuery) Doc() string {
	return "Filtered read with optional sort and row limit (no offset; filter to reach other rows)."
}
func (a *DataActQuery) ReadOnly() bool { return true }
func (a *DataActQuery) Example() any {
	return &DataActQuery{TableName: "items", Limit: 10}
}
func (a *DataActQuery) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataQuery(ctx, a)
}

// --- insert -----------------------------------------------------------------

type DataActInsert struct {
	TableName string         `arg:"table_name,required" desc:"Project table name (snake_case)."`
	Data      map[string]any `arg:"data"                desc:"Single-row insert: key->value column map. Pass either data or rows, not both."`
	Rows      []any          `arg:"rows"                desc:"Bulk insert: [{col: val}, ...]. Pass any number of rows in one call - over 100 auto-splits into 100-row batches, so do not hand-split a seed."`
}

func (a *DataActInsert) ActionName() string { return "insert" }
func (a *DataActInsert) Doc() string {
	return "Insert a single row (data) or many rows in bulk (rows). Prefer bulk for seeding. Do not seed derivable bulk data (e.g. a whole voxel world) into a table - generate it client-side. Column names in a row object are bare identifiers (bet_amount, not a quoted/escaped string)."
}
func (a *DataActInsert) Example() any {
	return &DataActInsert{TableName: "items", Data: map[string]any{"name": "Bread"}}
}
func (a *DataActInsert) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataInsert(ctx, a, nil)
}

// --- upsert -----------------------------------------------------------------

type DataActUpsert struct {
	TableName       string         `arg:"table_name,required"        desc:"Project table name (snake_case)."`
	Data            map[string]any `arg:"data,required"              desc:"Row to insert (or update on conflict): key->value column map."`
	ConflictColumns []any          `arg:"conflict_columns,required"  desc:"Column(s) whose match triggers UPDATE instead of INSERT." items:"string"`
}

func (a *DataActUpsert) ActionName() string { return "upsert" }
func (a *DataActUpsert) Doc() string {
	return "Insert or update on conflict. Requires conflict_columns, and the table must carry a matching UNIQUE or PRIMARY KEY constraint for them - an ON CONFLICT with no matching constraint errors; use insert when in doubt."
}
func (a *DataActUpsert) Example() any {
	return &DataActUpsert{
		TableName:       "users",
		Data:            map[string]any{"email": "a@b.com", "name": "A"},
		ConflictColumns: []any{"email"},
	}
}
func (a *DataActUpsert) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataUpsert(ctx, a, nil)
}

// --- update -----------------------------------------------------------------

type DataActUpdate struct {
	TableName string         `arg:"table_name,required" desc:"Project table name (snake_case)."`
	Data      map[string]any `arg:"data,required"       desc:"Column->value map to apply to matched rows."`
	ID        int            `arg:"id"                  desc:"Single-row update by row id. Mutually exclusive with filters - pass one, never both."`
	Filters   []any          `arg:"filters"             desc:"Filter array: each item {column, op, value}. Ops: =, !=, <, >, <=, >=, LIKE, NOT LIKE, IS NULL, IS NOT NULL. Mutually exclusive with id."`
}

func (a *DataActUpdate) ActionName() string { return "update" }
func (a *DataActUpdate) Doc() string        { return "Update rows by id or filters. Provide one, not both." }
func (a *DataActUpdate) Example() any {
	return &DataActUpdate{TableName: "items", ID: 1, Data: map[string]any{"name": "Sourdough"}}
}
func (a *DataActUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataUpdate(ctx, a, nil)
}

// --- batch_update -----------------------------------------------------------

type DataActBatchUpdate struct {
	TableName string `arg:"table_name,required" desc:"Project table name (snake_case)."`
	Updates   []any  `arg:"updates,required"    desc:"Array of {id|filters, data} entries. Cap 100 entries per call."`
}

func (a *DataActBatchUpdate) ActionName() string { return "batch_update" }
func (a *DataActBatchUpdate) Doc() string        { return "Apply many updates in one atomic transaction." }
func (a *DataActBatchUpdate) Example() any {
	return &DataActBatchUpdate{
		TableName: "items",
		Updates:   []any{map[string]any{"id": 1, "data": map[string]any{"qty": 5}}},
	}
}
func (a *DataActBatchUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataBatchUpdate(ctx, a)
}

// --- delete -----------------------------------------------------------------

type DataActDelete struct {
	TableName string `arg:"table_name,required" desc:"Project table name (snake_case)."`
	ID        int    `arg:"id,required"         desc:"Row id of the single row to delete. To delete rows by criteria use action=batch_delete with filters instead - delete is id-only."`
}

func (a *DataActDelete) ActionName() string { return "delete" }
func (a *DataActDelete) Doc() string {
	return "Delete a single row by id. For filter-based deletes use batch_delete."
}
func (a *DataActDelete) Example() any {
	return &DataActDelete{TableName: "items", ID: 1}
}
func (a *DataActDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataDelete(ctx, a, nil)
}

// --- batch_delete -----------------------------------------------------------

type DataActBatchDelete struct {
	TableName string `arg:"table_name,required" desc:"Project table name (snake_case)."`
	Filters   []any  `arg:"filters,required"    desc:"Filter array: each item {column, op, value}. Ops: =, !=, <, >, <=, >=, LIKE, NOT LIKE, IS NULL, IS NOT NULL."`
}

func (a *DataActBatchDelete) ActionName() string { return "batch_delete" }
func (a *DataActBatchDelete) Doc() string {
	return "Delete every row matching the filters in one call."
}
func (a *DataActBatchDelete) Example() any {
	return &DataActBatchDelete{
		TableName: "logs",
		Filters:   []any{map[string]any{"column": "level", "op": "=", "value": "debug"}},
	}
}
func (a *DataActBatchDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataBatchDelete(ctx, a, nil)
}

// --- count ------------------------------------------------------------------

type DataActCount struct {
	TableName string `arg:"table_name,required" desc:"Project table name (snake_case)."`
	Filters   []any  `arg:"filters"             desc:"Filter array: each item {column, op, value}. Ops: =, !=, <, >, <=, >=, LIKE, NOT LIKE, IS NULL, IS NOT NULL."`
}

func (a *DataActCount) ActionName() string { return "count" }
func (a *DataActCount) Doc() string        { return "Count rows matching filters." }
func (a *DataActCount) ReadOnly() bool     { return true }
func (a *DataActCount) Example() any       { return &DataActCount{TableName: "items"} }
func (a *DataActCount) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataCount(ctx, a)
}

// --- aggregate --------------------------------------------------------------

type DataActAggregate struct {
	TableName string `arg:"table_name,required" desc:"Project table name (snake_case)."`
	Function  string `arg:"function,required"   desc:"Aggregation function." enum:"count,sum,avg,min,max"`
	Column    string `arg:"column"              desc:"Target column; required for sum/avg/min/max, optional for count (counts non-NULL values of it)."`
	Filters   []any  `arg:"filters"             desc:"Filter array: each item {column, op, value}. Ops: =, !=, <, >, <=, >=, LIKE, NOT LIKE, IS NULL, IS NOT NULL."`
	GroupBy   []any  `arg:"group_by"            desc:"Optional GROUP BY columns." items:"string"`
}

func (a *DataActAggregate) ActionName() string { return "aggregate" }
func (a *DataActAggregate) Doc() string {
	return "Aggregate function (count/sum/avg/min/max) with optional GROUP BY."
}
func (a *DataActAggregate) ReadOnly() bool { return true }
func (a *DataActAggregate) Example() any {
	return &DataActAggregate{TableName: "orders", Function: "sum", Column: "total"}
}
func (a *DataActAggregate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataAggregate(ctx, a)
}

// --- search -----------------------------------------------------------------

type DataActSearch struct {
	TableName string `arg:"table_name,required" desc:"Project table name (snake_case)."`
	QueryText string `arg:"query_text,required" desc:"FTS5 query string. Requires an FTS index on the table: searchable_columns at manage_schema create_table, or manage_keyword_search action=create_index afterwards."`
	Limit     int    `arg:"limit"               desc:"Max rows returned. Default 50, capped at 200 - a larger value is rejected." default:"50" max:"200"`
}

func (a *DataActSearch) ActionName() string { return "search" }
func (a *DataActSearch) Doc() string        { return "FTS5 keyword search over searchable_columns." }
func (a *DataActSearch) ReadOnly() bool     { return true }
func (a *DataActSearch) Example() any {
	return &DataActSearch{TableName: "articles", QueryText: "ferment"}
}
func (a *DataActSearch) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DataSearch(ctx, a)
}
