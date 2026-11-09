package api

import "openfabrica/tools"

// manage_uploads registers file-upload endpoints. Handlers live in uploads.go.
//
// One arg struct per action. Create and update share most of the option
// surface; the mergeUploadOpts helper centralises the args-map shape so
// both call paths produce identical inputs to the underlying handler.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_uploads",
		Category: "api",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult:          8000,
		FullResultInEvents: true,
		OneLiner:           "Define file upload endpoints served at POST /api/<path>/upload (multipart 'file' field; runtime adds the /upload suffix).",
		WhenToUse: `Use when the project takes user-uploaded files (images, PDFs, documents). Default
upload cap is 5 MB; raise via max_size_mb (hard cap 500 MB). Restrict acceptable
file types via allowed_types (audio/video are not in the default). Pair an optional table to auto-insert rows;
otherwise the file is stored under /files and the URL returned in the response.
For "each user sees only their own files", set owner_column on the linked table
rather than filtering in the browser - the runtime stamps the uploader in.`,
		Actions: []tools.Action{
			&UploadCreate{},
			&UploadUpdate{},
			&UploadList{},
			&UploadDelete{},
		},
		Summarize: EpSummarize,
	})
}

// --- create -----------------------------------------------------------------

type UploadCreate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Upload endpoint path suffix."`
	TableName    string `arg:"table_name"             desc:"Optional table to auto-insert into; must already exist and have filename, url, content_type, size columns. Bind it only when the row is nothing but file metadata: the insert fills those four (plus the endpoint's owner/tenant column, when multi-tenancy configured one) and nothing else, so a table with any other NOT NULL, no-default column is rejected. If the row also carries user-typed fields (a title, a caption), omit table_name (or pass \"none\") for disk-only storage and POST the row through the table's own create_api with the returned url and those fields in one insert. For server-generated fields (a share code, an expiry), declare the column with generate in manage_schema create_table."`
	RequiresAuth bool   `arg:"requires_auth"          desc:"Bearer token required for writes."`
	RateLimit    int    `arg:"rate_limit"             desc:"Req/min per IP." default:"60"`
	AllowedTypes []any  `arg:"allowed_types"          desc:"MIME globs, e.g. [\"image/*\",\"application/pdf\"]." items:"string"`
	MaxSizeMB    int    `arg:"max_size_mb"            desc:"Max upload size in MB (max 500; larger is rejected)." default:"5" max:"500"`
	OwnerColumn  string `arg:"owner_column"           desc:"Column on table_name holding the uploader's user id. Set it for a my-uploads-only endpoint: the runtime stamps the authenticated user into it on the linked-row insert, so each user sees only their own files. Requires table_name and requires_auth=true; both are enforced."`
}

func (a *UploadCreate) Doc() string {
	return "Create an upload endpoint. Create-only: if the path already exists this is a no-op that changes nothing - use action=update to change an existing endpoint."
}
func (a *UploadCreate) Example() any {
	return &UploadCreate{EndpointPath: "images", AllowedTypes: []any{"image/*"}}
}
func (a *UploadCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epCreateUpload(ctx, a.EndpointPath, a.TableName, a.RequiresAuth, a.RateLimit, a.AllowedTypes, a.MaxSizeMB, a.OwnerColumn)
}

// --- update -----------------------------------------------------------------

type UploadUpdate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Upload endpoint path to update."`
	TableName    string `arg:"table_name"             desc:"New auto-insert table (must already exist, and is rejected on the same unfillable-column rule as create). Pass \"none\" to detach the table and switch the endpoint to disk-only storage. Omit to leave unchanged."`
	RequiresAuth *bool  `arg:"requires_auth"          desc:"Toggle the bearer-token requirement on (true) or off (false). Omit to leave unchanged."`
	RateLimit    int    `arg:"rate_limit"             desc:"New req/min per IP cap."`
	AllowedTypes []any  `arg:"allowed_types"          desc:"Replace the MIME-glob allow-list." items:"string"`
	MaxSizeMB    int    `arg:"max_size_mb"            desc:"New max upload size in MB (maximum 500)." max:"500"`
	OwnerColumn  string `arg:"owner_column"           desc:"Column on the linked table holding the uploader's user id, for a my-uploads-only endpoint. Pass \"none\" to clear the scoping. Omit to leave unchanged."`
}

func (a *UploadUpdate) Doc() string { return "Update an upload endpoint's options." }
func (a *UploadUpdate) Example() any {
	return &UploadUpdate{EndpointPath: "images", MaxSizeMB: 10}
}
func (a *UploadUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epUpdateUpload(ctx, a.EndpointPath, a.TableName, a.RequiresAuth, a.RateLimit, a.AllowedTypes, a.MaxSizeMB, a.OwnerColumn)
}

// --- list -------------------------------------------------------------------

type UploadList struct{}

func (a *UploadList) Doc() string    { return "List upload endpoints." }
func (a *UploadList) ReadOnly() bool { return true }
func (a *UploadList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epListUpload(ctx)
}

// --- delete -----------------------------------------------------------------

type UploadDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Endpoint path to delete."`
}

func (a *UploadDelete) Doc() string  { return "Delete an upload endpoint." }
func (a *UploadDelete) Example() any { return &UploadDelete{EndpointPath: "images"} }
func (a *UploadDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epDeleteUpload(ctx, a.EndpointPath)
}
