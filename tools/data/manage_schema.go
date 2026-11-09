package data

import (
	"openfabrica/tools"
)

// manage_schema is the DDL surface against project tables: create/alter/
// describe/drop, indexes, and validation rules. Row CRUD lives in
// manage_data; raw SQL lives in manage_sql.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_schema",
		Category:           "data",
		FullResultInEvents: true,
		OneLiner:           "Define table DDL - create/alter/drop, indexes, validation. PASSWORD/ENCRYPTED columns auto-hash/auto-encrypt.",
		WhenToUse: `Create or modify project tables. To change one you already created,
prefer alter_table - it adds or drops columns, and cannot rename or retype one. A
wrong column on a table holding rows: add the right one under a new name, copy values
with manage_data / manage_sql, drop the old. On an EMPTY table drop_table and recreate
is fine (no approval needed). drop_table is destructive: confirm=true, plus owner
approval once the table holds rows. Row CRUD is manage_data; raw SQL is manage_sql. Reserved
table names start with of_; use descriptive prefixes for your own.`,
		Actions: []tools.Action{
			&SchemaActCreateTable{},
			&SchemaActAlterTable{},
			&SchemaActDescribeTable{},
			&SchemaActListTables{},
			&SchemaActDropTable{},
			&SchemaActAddIndex{},
			&SchemaActSetValidation{},
			&SchemaActCreateView{},
			&SchemaActDropView{},
		},
		// The LLM passes `action` explicitly (the framework adds it as a
		// required enum at the tool schema level). We deliberately do not infer
		// it from which fields are populated: inference (column_defs=>create_table,
		// add_columns=>alter_table, index_columns=>add_index) silently routed
		// mixed-arg calls to the wrong action.
		Hints: func(action string, _ map[string]any, r *tools.Result) string {
			ok := r != nil && r.Success
			switch action {
			case "create_table":
				if !ok {
					return ""
				}
				if m, isMap := r.Data.(map[string]any); isMap && m["already_existed"] == true {
					return ""
				}
				// Post-call: the column-type/constraint catalogue lives on the
				// column_defs arg (pre-call, where it's needed), not re-taught here.
				return "Table created (id and created_at auto-added). Index its filter/lookup/FK columns now with add_index, in this same turn while the shape is fresh. PASSWORD/ENCRYPTED columns are omitted from every read and cannot be queried by value."
			case "alter_table":
				return "add_columns: same format as create_table.column_defs. drop_columns is destructive - requires confirm=true, plus owner approval when the table holds rows."
			case "describe_table":
				if !ok {
					return ""
				}
				return "Column `type` reflects the semantic type: PASSWORD and ENCRYPTED columns report as `PASSWORD`/`ENCRYPTED` (not the underlying TEXT/BLOB affinity). Those columns are omitted from row reads."
			case "drop_table":
				return "Pass confirm=true to acknowledge this is destructive. A table holding rows also needs owner approval at run time; an empty one is waived."
			case "create_view":
				if !ok {
					return ""
				}
				// Post-call: the SELECT constraints live on the select arg (pre-call).
				return "View created. Query it with manage_sql execute_sql, expose it read-only at runtime via manage_api create (table_name=<view>), or inspect its columns with describe_table."
			case "drop_view":
				return "Removes a view only - holds no data, so no confirm/approval needed. To remove a table use drop_table."
			}
			return ""
		},
		MaxResult: 8000,
		Summarize: MergedSummarize,
	})
}

// SchemaActCreateTable creates a new project table.
type SchemaActCreateTable struct {
	TableName         string         `arg:"table_name,required" desc:"New table name (snake_case). Reserved prefix of_ is rejected - use a descriptive project-specific prefix."`
	ColumnDefs        map[string]any `arg:"column_defs,required" desc:"Map of column-names-only -> type-or-constraint-object. Bare-type form: {\"title\":\"TEXT\",\"qty\":\"INTEGER\"}. Constraint-object form: {\"email\":{\"type\":\"TEXT\",\"unique\":true,\"not_null\":true}}. Types: TEXT, INTEGER, REAL, BOOLEAN, BLOB (raw bytes; prefer manage_uploads for files), PASSWORD (auto-hashed with bcrypt on write - store user passwords as PASSWORD, never TEXT), ENCRYPTED (AES-encrypted at rest). Constraint keys: type, not_null, unique, default, check (SQL expression), references (\"table(col)\"), generate. generate puts a SERVER-GENERATED value on the row at insert time - one of: code (short share slug), token, uuid, expires_in_days:N, expires_in_hours:N. It is filled automatically by an upload or CRUD auto-insert, so a share code / expiry needs NO server function: e.g. {\"slug\":{\"type\":\"TEXT\",\"unique\":true,\"not_null\":true,\"generate\":\"code\"},\"expires_at\":{\"type\":\"TEXT\",\"generate\":\"expires_in_days:7\"}}. (generate works only at create_table, not alter_table add_columns.) DO NOT include sibling args here: table_name and searchable_columns are top-level args; id and created_at are added automatically. Wrong: {\"table_name\":\"items\",\"title\":\"TEXT\"} - table_name goes outside column_defs."`
	SearchableColumns []any          `arg:"searchable_columns" desc:"TEXT column names to index for FTS5 keyword search via manage_data action=search. Must reference columns declared in column_defs." items:"string"`
}

func (a *SchemaActCreateTable) ActionName() string { return "create_table" }
func (a *SchemaActCreateTable) Doc() string {
	return "Create a new project table. Declare every column the table needs up front - especially the FK/owner columns a join table is FOR (a channel_members table MUST carry channel_id, not just user_id) - then add_index its filter/lookup/FK columns in that SAME turn, while the shape is fresh: splitting create and index across turns is how you end up indexing a column the table never got (`no such column`)."
}
func (a *SchemaActCreateTable) Example() any {
	// Note: column_defs keys are *column names only* - table_name is a
	// sibling arg, not a key inside this map. Using `title` here (not `name`)
	// to avoid the column-name-vs-metadata-field conflation the model has
	// repeatedly fallen into.
	return &SchemaActCreateTable{
		TableName:  "items",
		ColumnDefs: map[string]any{"title": "TEXT", "qty": "INTEGER"},
	}
}
func (a *SchemaActCreateTable) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return SchemaCreate(ctx, a.TableName, a.ColumnDefs, a.SearchableColumns)
}

// SchemaActAlterTable adds or drops columns on an existing table.
type SchemaActAlterTable struct {
	TableName   string         `arg:"table_name,required" desc:"Existing table name."`
	AddColumns  map[string]any `arg:"add_columns"         desc:"Columns to add. Same shape as create_table.column_defs (bare-type or constraint-object form)."`
	DropColumns []any          `arg:"drop_columns"        desc:"Column names to drop. Destructive - requires confirm=true, plus owner approval when the table holds rows (an empty table has nothing to lose, so it is waived)." items:"string"`
	Confirm     bool           `arg:"confirm"             desc:"Required when drop_columns is set. Acknowledges that dropping columns deletes data permanently."`
}

func (a *SchemaActAlterTable) ActionName() string { return "alter_table" }
func (a *SchemaActAlterTable) Doc() string {
	return "Add or drop columns on an existing table (no rename or retype)."
}
func (a *SchemaActAlterTable) Example() any {
	return &SchemaActAlterTable{TableName: "items", AddColumns: map[string]any{"sku": "TEXT"}}
}
func (a *SchemaActAlterTable) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if len(a.DropColumns) > 0 && !a.Confirm {
		return tools.FailCode(tools.CodeMissingArg, "drop_columns requires confirm=true (dropping a column deletes its data). Set confirm=true and re-try; a table holding rows also needs owner approval at run time."), nil
	}
	return SchemaAlter(ctx, a.TableName, a.AddColumns, a.DropColumns)
}

// SchemaActDescribeTable returns column list, types, indexes, and validation.
type SchemaActDescribeTable struct {
	TableName string `arg:"table_name,required" desc:"Existing table name."`
}

func (a *SchemaActDescribeTable) ActionName() string { return "describe_table" }
func (a *SchemaActDescribeTable) Doc() string {
	return "Return column list, types, indexes, and validation for one table."
}
func (a *SchemaActDescribeTable) ReadOnly() bool { return true }
func (a *SchemaActDescribeTable) Example() any {
	return &SchemaActDescribeTable{TableName: "items"}
}
func (a *SchemaActDescribeTable) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return SchemaDescribe(ctx, a.TableName)
}

// SchemaActListTables returns every project table.
type SchemaActListTables struct{}

func (a *SchemaActListTables) ActionName() string { return "list_tables" }
func (a *SchemaActListTables) Doc() string        { return "List every project table." }
func (a *SchemaActListTables) ReadOnly() bool     { return true }
func (a *SchemaActListTables) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return SchemaList(ctx)
}

// SchemaActDropTable removes a table.
type SchemaActDropTable struct {
	TableName string `arg:"table_name,required" desc:"Existing table name."`
	Confirm   bool   `arg:"confirm,required"    desc:"Must be true. Acknowledges that drop_table deletes every row and is irreversible without a backup. Owner approval is also enforced at run time."`
}

func (a *SchemaActDropTable) ActionName() string { return "drop_table" }
func (a *SchemaActDropTable) Doc() string {
	return "Drop a table, its indexes, and the API and auth endpoints built on it. Destructive; requires confirm=true, plus owner approval when the table holds rows. alter_table cannot rename or retype a column, so on an empty table with a wrong column drop and recreate it; on a table holding rows migrate through a new column instead."
}
func (a *SchemaActDropTable) Example() any {
	return &SchemaActDropTable{TableName: "temp_scratch", Confirm: true}
}
func (a *SchemaActDropTable) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if !a.Confirm {
		return tools.FailCode(tools.CodeMissingArg, "drop_table requires confirm=true (dropping a table deletes every row). Set confirm=true and re-try; a table holding rows also needs owner approval at run time."), nil
	}
	return SchemaDrop(ctx, a.TableName)
}

// SchemaActAddIndex creates a B-tree index over one or more columns.
type SchemaActAddIndex struct {
	TableName    string `arg:"table_name,required"    desc:"Existing table name."`
	IndexColumns []any  `arg:"index_columns,required" desc:"Column names to include in the index - bare names (\"channel_id\"), never table-qualified (\"channel_members.channel_id\")." items:"string"`
	Unique       bool   `arg:"unique"                 desc:"Create a UNIQUE index."`
}

func (a *SchemaActAddIndex) ActionName() string { return "add_index" }
func (a *SchemaActAddIndex) Doc() string        { return "Create a B-tree index over one or more columns." }
func (a *SchemaActAddIndex) Example() any {
	return &SchemaActAddIndex{TableName: "users", IndexColumns: []any{"email"}, Unique: true}
}
func (a *SchemaActAddIndex) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return SchemaAddIndex(ctx, a.TableName, a.IndexColumns, a.Unique)
}

// SchemaActSetValidation attaches per-column validation rules.
type SchemaActSetValidation struct {
	TableName  string         `arg:"table_name,required" desc:"Existing table name."`
	Validation map[string]any `arg:"validation,required" desc:"Map of column name -> rule object. Supported rules: min_length, max_length, min, max (numeric), pattern (regex), format (email|url|phone|uuid|date|time), enum (array of allowed values), custom_message. Example: {\"email\":{\"format\":\"email\"},\"age\":{\"min\":0,\"max\":120}}."`
}

func (a *SchemaActSetValidation) ActionName() string { return "set_validation" }
func (a *SchemaActSetValidation) Doc() string {
	return "Attach per-column validation rules (merged into any already set). Enforced on every write: the app's CRUD API answers 400 {error, fields} for a violating POST/PUT, and manage_data / manage_sql imports reject the row."
}
func (a *SchemaActSetValidation) Example() any {
	return &SchemaActSetValidation{
		TableName: "users",
		Validation: map[string]any{
			"email": map[string]any{"format": "email"},
			"age":   map[string]any{"min": 0, "max": 120},
		},
	}
}
func (a *SchemaActSetValidation) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return SchemaSetValidation(ctx, a.TableName, a.Validation)
}

// SchemaActCreateView defines a read-only view: a named SELECT over project
// tables. SQLite resolves it transparently, so it is queryable via manage_sql
// execute_sql and inspectable via describe_table.
type SchemaActCreateView struct {
	ViewName string `arg:"view_name,required" desc:"New view name (snake_case). Reserved prefix of_ is rejected; must not collide with an existing table."`
	Select   string `arg:"select,required"    desc:"A single read-only SELECT defining the view, e.g. \"SELECT u.id, u.name, COUNT(o.id) AS order_count FROM users u LEFT JOIN orders o ON o.user_id = u.id GROUP BY u.id\". SELECT only - no INSERT/UPDATE/DELETE, no ';' (single statement), and no of_ system tables. Re-running with the same view_name replaces the definition."`
}

func (a *SchemaActCreateView) ActionName() string { return "create_view" }
func (a *SchemaActCreateView) Doc() string {
	return "Create (or replace) a read-only view over project tables. Serve it to the app with manage_api create (table_name=<view>); manage_sql execute_sql reads it at build time."
}
func (a *SchemaActCreateView) Example() any {
	return &SchemaActCreateView{
		ViewName: "active_users",
		Select:   "SELECT id, email FROM users WHERE status = 'active'",
	}
}
func (a *SchemaActCreateView) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return SchemaCreateView(ctx, a.ViewName, a.Select)
}

// SchemaActDropView removes a view. A view holds no data, so unlike drop_table
// this needs no confirm flag or owner approval.
type SchemaActDropView struct {
	ViewName string `arg:"view_name,required" desc:"View to drop. Holds no data; this only removes the saved query. To drop a table use drop_table."`
}

func (a *SchemaActDropView) ActionName() string { return "drop_view" }
func (a *SchemaActDropView) Doc() string {
	return "Drop a view. Non-destructive - a view stores no rows."
}
func (a *SchemaActDropView) Example() any {
	return &SchemaActDropView{ViewName: "active_users"}
}
func (a *SchemaActDropView) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return SchemaDropView(ctx, a.ViewName)
}
