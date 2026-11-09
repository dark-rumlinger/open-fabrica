package api

import "openfabrica/tools"

// manage_export_endpoint registers CSV-download HTTP endpoints served at
// /api/<path>/export. Handlers live in exports.go; this file declares the
// LLM schema. Named "_endpoint" to distinguish creating a reusable endpoint
// from manage_sql's one-off export_csv data transfer.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_export_endpoint",
		Category:           "api",
		FullResultInEvents: true,
		OneLiner:           "Define a CSV export HTTP endpoint at /api/<path>/export (table downloads as text/csv).",
		WhenToUse: `Use whenever the plan asks for a reusable endpoint to download table data as a
spreadsheet. Pair with manage_import_endpoint for round-trip CSV. owner_column
scopes the export so users only download their own rows. For a ONE-OFF export at
build time use manage_sql instead.`,
		Actions: []tools.Action{
			&ExportCreate{},
			&ExportList{},
			&ExportDelete{},
		},
		Summarize: EpSummarize,
	})
}

// ExportCreate creates a CSV export endpoint over a project table.
type ExportCreate struct {
	EndpointPath  string `arg:"endpoint_path,required" desc:"Path suffix served at /api/<path>/export."`
	TableName     string `arg:"table_name,required"    desc:"Backing table."`
	PublicColumns []any  `arg:"public_columns"         desc:"Columns to expose. PASSWORD always hidden." items:"string"`
	RequiresAuth  bool   `arg:"requires_auth"          desc:"Bearer token required. Default false - the export is public unless you set this true or scope it with owner_column."`
	OwnerColumn   string `arg:"owner_column"           desc:"Row scoping: each signed-in user downloads only their own rows (setting it requires login)."`
}

func (a *ExportCreate) ActionName() string { return "create" }
func (a *ExportCreate) Doc() string {
	return "Create a CSV export endpoint. Calling it again for the same path REPLACES it: omitted options reset to their defaults, so re-send requires_auth and owner_column. The export is public unless requires_auth=true or owner_column is set."
}
func (a *ExportCreate) Example() any {
	return &ExportCreate{EndpointPath: "orders", TableName: "orders", RequiresAuth: true}
}
func (a *ExportCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return apiCreateExport(ctx, a.EndpointPath, a.TableName, a.PublicColumns, a.RequiresAuth, a.OwnerColumn)
}

type ExportList struct{}

func (a *ExportList) ActionName() string { return "list" }
func (a *ExportList) Doc() string        { return "List export endpoints." }
func (a *ExportList) ReadOnly() bool     { return true }
func (a *ExportList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return apiListExport(ctx)
}

type ExportDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Endpoint path to delete."`
}

func (a *ExportDelete) ActionName() string { return "delete" }
func (a *ExportDelete) Doc() string        { return "Delete an export endpoint." }
func (a *ExportDelete) Example() any       { return &ExportDelete{EndpointPath: "orders"} }
func (a *ExportDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return apiDeleteExport(ctx, a.EndpointPath)
}
