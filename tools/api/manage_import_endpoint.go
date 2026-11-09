package api

import "openfabrica/tools"

// manage_import_endpoint registers CSV bulk-insert HTTP endpoints at
// /api/<path>/import. Handlers live in exports.go alongside the related export
// ones. Named "_endpoint" to distinguish creating a reusable endpoint from
// manage_sql's one-off import_csv/import_json data transfer.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_import_endpoint",
		Category:           "api",
		FullResultInEvents: true,
		OneLiner:           "Define a CSV bulk-insert HTTP endpoint at /api/<path>/import (multipart CSV upload).",
		WhenToUse: `Use when admins need a reusable endpoint to upload spreadsheet data into a
table. The CSV header must match column names exactly. This creates an HTTP
endpoint; for a ONE-OFF data load at build time prefer manage_sql import_json
(programmatic JSON) or manage_sql import_csv (CSV-from-LLM).`,
		Actions: []tools.Action{
			&ImportCreate{},
			&ImportList{},
			&ImportDelete{},
		},
		Summarize: EpSummarize,
	})
}

type ImportCreate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Path suffix served at /api/<path>/import."`
	TableName    string `arg:"table_name,required"    desc:"Backing table; header row must match existing column names."`
	RequiresAuth bool   `arg:"requires_auth"          desc:"Bearer token required. Default true - an import writes rows, so it is auth-gated unless you set this false." default:"true"`
	// Named write_mode, not mode: `manage_api` and friends share this category, and a
	// bare `mode` collides with the descriptive `mode: signaling` the webrtc guide uses
	// (TestGuideArgsBelongToDeclaredTools catches exactly that ambiguity).
	WriteMode    string `arg:"write_mode"             desc:"insert (default) appends every row; upsert updates the existing row when unique_column matches and inserts otherwise - the pick for a re-runnable import that must not duplicate rows. upsert REQUIRES unique_column." enum:"insert,upsert"`
	UniqueColumn string `arg:"unique_column"          desc:"Column that identifies an existing row, for write_mode=upsert. Must be a real column on table_name and should carry a UNIQUE constraint."`
	MaxRows      int    `arg:"max_rows"               desc:"Reject an upload with more rows than this (default 10000)." default:"10000"`
}

func (a *ImportCreate) ActionName() string { return "create" }
func (a *ImportCreate) Doc() string {
	return "Create a CSV import endpoint (calling it again for the same path replaces it; omitted options reset to defaults). write_mode=upsert plus unique_column makes it idempotent, so re-uploading a corrected file updates rows instead of duplicating them."
}
func (a *ImportCreate) Example() any {
	return &ImportCreate{EndpointPath: "products", TableName: "products"}
}
func (a *ImportCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return apiCreateImport(ctx, a.EndpointPath, a.TableName, a.RequiresAuth, a.WriteMode, a.UniqueColumn, a.MaxRows)
}

type ImportList struct{}

func (a *ImportList) ActionName() string { return "list" }
func (a *ImportList) Doc() string        { return "List import endpoints." }
func (a *ImportList) ReadOnly() bool     { return true }
func (a *ImportList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return apiListImport(ctx)
}

type ImportDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Endpoint path to delete."`
}

func (a *ImportDelete) ActionName() string { return "delete" }
func (a *ImportDelete) Doc() string        { return "Delete an import endpoint." }
func (a *ImportDelete) Example() any       { return &ImportDelete{EndpointPath: "products"} }
func (a *ImportDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return apiDeleteImport(ctx, a.EndpointPath)
}
