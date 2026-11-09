package api

import (
	"fmt"

	"openfabrica/tools"
)

// manage_api is the REST CRUD surface: one create per table you want exposed
// at /api/<path>. Handler is auto-emitted by the dynamic API runtime. The
// Run methods here delegate to the existing endpoint-CRUD handlers in
// crud.go.
//
// One arg struct per action. Create and update share most of their shape but
// have different required-ness: create needs endpoint_path+table_name, update
// only needs endpoint_path. mergeEndpointOpts centralises the auth_model
// translation so both calls produce the same map shape for the underlying
// runtime.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_api",
		Category:           "api",
		FullResultInEvents: true,
		OneLiner:           "Define REST CRUD endpoints (GET/POST/PUT/DELETE) over a project table, GET over a view, or a read-only SQL query endpoint.",
		WhenToUse: `One create per table exposed at /api/<path>; the handler is auto-emitted. LIST and single-row/POST/PUT responses use different shapes: build to the api-response contract in your mental model.

create also accepts a SQL VIEW as table_name: the endpoint is read-only (GET list and single row, public_read), so a view's joined or aggregated rows are browsable like a table's. Use it for report views instead of materialising them in the browser.

enable_query adds a read-only raw-SELECT endpoint (POST /api/<path>, default "_query") returning {columns, rows (arrays), row_count, execution_ms, truncated}: the only runtime way to run free-form joins or aggregations (manage_sql is build-time only). It reads every non-auth table with NO owner or tenant scoping, so enable it only for a query tool, with requires_auth=true unless all tables are public. delete removes it.

auth_model (exactly one): public; authenticated (any logged-in user); role (required_role names it); owner (owner_column; per-user RLS, each user reads AND writes only their own rows); authored (owner_column names the AUTHOR: every logged-in user reads all rows, each writes only their own). Use authored, not owner, when rows are shared within a workspace, channel or room: owner hides everyone else's rows. public_read=true keeps GET open while writes need auth. public_write=true is the inverse (anyone POSTs, GET stays auth-gated), the public-submission shape (contact form, feedback): use authenticated + public_write=true, not public, which also exposes the reads. public_write is invalid with owner/authored.

On a writable, non-public endpoint with no owner_column, the platform scopes it to a detected owner-like column (user_id, owner_id) and reports owner_scope_applied; set owner_column explicitly, or public_read, to control it.

Single-row GET/PUT/DELETE /api/<path>/<value>: numeric matches id; for strings the column is (1) lookup_column, (2) a "uuid" or "slug" column, (3) the table's sole non-id UNIQUE TEXT column; set lookup_column when there are several. searchable_columns feeds FTS5 (manage_data action=search), not ?q=. The key goes in the URL path (a bare PUT/DELETE /api/<path> is refused), and a PUT on a non-id unique key UPSERTS, so an idempotent write by a natural key is one PUT /api/<path>/<value>.

LIST filters, sorting, ?q= search and GET /api/<path>/_stats (server-side aggregates, use it instead of totalling rows in the browser) are built in; the api_response_contract guide has the grammar. generate_docs writes an OpenAPI spec to /api/docs.json.`,
		Actions: []tools.Action{
			&APICreate{},
			&APIUpdate{},
			&APIList{},
			&APIDelete{},
			&APIGenerateDocs{},
			&APIEnableQuery{},
		},
		Hints: func(action string, args map[string]any, r *tools.Result) string {
			if action != "create" || r == nil || !r.Success {
				return ""
			}
			if m, ok := r.Data.(map[string]any); ok && m["already_existed"] == true {
				return "" // nothing was applied, so there is no new endpoint to describe
			}
			rawPath, _ := args["endpoint_path"].(string)
			path := tools.NormalizeEndpointPath(rawPath)
			if path == "" {
				return ""
			}
			// Only what is true of THIS endpoint. The list/single/status shapes are the
			// REST envelope law, transcluded into every prompt already; a paraphrase here
			// would be a second copy in the block that changes most.
			hint := fmt.Sprintf("CRUD /api/%s ready. Build the client against the REST envelope law already on your prompt rather than guessing its shapes; POST takes a flat {field:value} body. The `api_response_contract` guide has the full client contract.", path)
			if am, _ := args["auth_model"].(string); am != "" && am != "public" {
				hint += " This endpoint is auth-gated: call it through `apiFetch` (see the auth law)."
			}
			if owner, _ := args["owner_column"].(string); owner != "" {
				if am, _ := args["auth_model"].(string); am == "authored" {
					hint += fmt.Sprintf(" RLS active (authored): %s auto-set from JWT on POST, scoped on PUT/DELETE, but GET is shared - every logged-in user reads all rows.", owner)
				} else {
					hint += fmt.Sprintf(" RLS active: %s auto-set from JWT on POST, scoped on GET/PUT/DELETE.", owner)
				}
			}
			if lookup, _ := args["lookup_column"].(string); lookup != "" {
				hint += fmt.Sprintf(" /api/%s/<value> resolves against %s.", path, lookup)
			}
			return hint
		},
		MaxResult: 8000,
		Summarize: EpSummarize,
	})
}

// --- create -----------------------------------------------------------------

type APICreate struct {
	EndpointPath    string   `arg:"endpoint_path,required" desc:"REST endpoint path suffix; no /api/ prefix and no leading slash (e.g. \"items\", \"orders/recent\")."`
	TableName       string   `arg:"table_name,required"    desc:"Existing table backing the endpoint."`
	Methods         []string `arg:"methods"                desc:"Allowed HTTP methods (default GET/POST/PUT/DELETE). Set explicitly to lock down the surface."`
	PublicColumns   []string `arg:"public_columns"         desc:"Columns returned in responses. PASSWORD columns are always hidden. Default: every non-PASSWORD column. Also bounds which columns a client may filter, search, sort and aggregate on."`
	WritableColumns []string `arg:"writable_columns"     desc:"Columns a client may set on POST/PUT; anything else in the body is dropped. Default: every column, so set this whenever the table has privileged fields (status, role, paid, credits). Owner and tenant columns are stamped from the session regardless."`
	AuthModel       string   `arg:"auth_model"             desc:"Authentication model (default public). The tool's WhenToUse carries the enum semantics and when to pick each." enum:"public,authenticated,role,owner,authored"`
	RequiredRole    string   `arg:"required_role"          desc:"Role name required when auth_model=role (e.g. admin, editor)."`
	OwnerColumn     string   `arg:"owner_column"           desc:"Column holding the owning/authoring user id, required when auth_model=owner or authored. Auto-set from the JWT on POST. owner scopes it on GET/PUT/DELETE (private per user); authored scopes it on PUT/DELETE only, leaving GET shared."`
	ReadIsPublic    bool     `arg:"public_read"            desc:"With auth_model != public: allow GET without a token even though writes require auth."`
	WriteIsPublic   bool     `arg:"public_write"           desc:"With auth_model != public: allow writes (POST/PUT/DELETE) without a token while GET stays auth-gated. The public-submission shape (contact form/feedback: anyone posts, only an admin reads). Not valid with auth_model=owner/authored, nor with owner_column set: an anonymous writer has no identity to scope the row to."`
	RateLimit       int      `arg:"rate_limit"             desc:"Requests/minute per IP." default:"60"`
	CacheTTL        int      `arg:"cache_ttl"              desc:"GET response cache TTL in seconds (0 disables caching)."`
	LookupColumn    string   `arg:"lookup_column"          desc:"TEXT column resolved by GET/PUT/DELETE /api/<path>/<value>."`
	CORSOrigins     []string `arg:"cors_origins"           desc:"Allowed CORS origins. Use [\"*\"] for any. Default: same-origin only."`
	CORSMethods     []string `arg:"cors_methods"           desc:"Allowed CORS methods. Default mirrors methods."`
	CORSHeaders     []string `arg:"cors_headers"           desc:"Allowed CORS request headers."`
}

func (a *APICreate) Doc() string {
	return "Scaffold CRUD over a table. Create-only: if the path already exists this is a no-op that changes nothing (it does not re-apply options) - use action=update to change an existing endpoint."
}
func (a *APICreate) Example() any {
	return &APICreate{EndpointPath: "items", TableName: "items"}
}
func (a *APICreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epCreateAPI(ctx, a)
}

// --- update -----------------------------------------------------------------

type APIUpdate struct {
	EndpointPath    string   `arg:"endpoint_path,required" desc:"REST endpoint path suffix; no /api/ prefix and no leading slash (e.g. \"items\", \"orders/recent\")."`
	TableName       string   `arg:"table_name"             desc:"Existing table backing the endpoint."`
	Methods         []string `arg:"methods"                desc:"Allowed HTTP methods (default GET/POST/PUT/DELETE). Set explicitly to lock down the surface."`
	PublicColumns   []string `arg:"public_columns"         desc:"Columns returned in responses. PASSWORD columns are always hidden. Default: every non-PASSWORD column. Also bounds which columns a client may filter, search, sort and aggregate on."`
	WritableColumns []string `arg:"writable_columns"     desc:"Pass [\"*\"] to allow every column again."`
	AuthModel       string   `arg:"auth_model"             desc:"Replace the authentication model. REQUIRED to change any other auth setting: required_role, owner_column, public_read and public_write are read only when auth_model rides the same call, and are silently ignored without it. Re-send the endpoint's current model if you are only changing one of them. Switching between owner and authored also flips read sharing." enum:"public,authenticated,role,owner,authored"`
	RequiredRole    string   `arg:"required_role"          desc:"Role name required when auth_model=role (e.g. admin, editor)."`
	OwnerColumn     string   `arg:"owner_column"           desc:"Column holding the owning/authoring user id, required when auth_model=owner or authored. Auto-set from the JWT on POST. owner scopes it on GET/PUT/DELETE (private per user); authored scopes it on PUT/DELETE only, leaving GET shared."`
	ReadIsPublic    bool     `arg:"public_read"            desc:"With auth_model != public: allow GET without a token even though writes require auth."`
	WriteIsPublic   bool     `arg:"public_write"           desc:"With auth_model != public: allow writes (POST/PUT/DELETE) without a token while GET stays auth-gated. The public-submission shape (contact form/feedback: anyone posts, only an admin reads). Not valid with auth_model=owner/authored, nor with owner_column set: an anonymous writer has no identity to scope the row to."`
	RateLimit       int      `arg:"rate_limit"             desc:"Requests/minute per IP."`
	CacheTTL        int      `arg:"cache_ttl"              desc:"Pass -1 to disable caching; a bare 0 keeps the current TTL."`
	LookupColumn    string   `arg:"lookup_column"          desc:"TEXT column resolved by GET/PUT/DELETE /api/<path>/<value>."`
	CORSOrigins     []string `arg:"cors_origins"           desc:"Allowed CORS origins. Use [\"*\"] for any. Default: same-origin only."`
	CORSMethods     []string `arg:"cors_methods"           desc:"Allowed CORS methods. Default mirrors methods."`
	CORSHeaders     []string `arg:"cors_headers"           desc:"Allowed CORS request headers."`
}

func (a *APIUpdate) Doc() string {
	return "Update an existing API endpoint's options. An omitted arg keeps its stored value; a list arg (methods, columns, cors_*) replaces the stored list."
}
func (a *APIUpdate) Example() any {
	return &APIUpdate{EndpointPath: "items", RateLimit: 120}
}
func (a *APIUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epUpdateAPI(ctx, a)
}

// --- list -------------------------------------------------------------------

type APIList struct{}

func (a *APIList) Doc() string    { return "List API endpoints." }
func (a *APIList) ReadOnly() bool { return true }
func (a *APIList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epListAPI(ctx)
}

// --- delete -----------------------------------------------------------------

type APIDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Endpoint path to delete."`
}

func (a *APIDelete) Doc() string  { return "Delete an API endpoint." }
func (a *APIDelete) Example() any { return &APIDelete{EndpointPath: "items"} }
func (a *APIDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epDeleteAPI(ctx, a.EndpointPath)
}

// --- generate_docs ----------------------------------------------------------

type APIGenerateDocs struct{}

func (a *APIGenerateDocs) Doc() string {
	return "Write an OpenAPI 3.0 spec to the api-docs.json asset, served at /api/docs.json (import it into any OpenAPI viewer; no docs page is rendered). Covers the REST/CRUD, auth, upload, SSE stream, and websocket endpoints - a function, LLM, export, import, or query endpoint is not described there."
}
func (a *APIGenerateDocs) ReadOnly() bool { return false }
func (a *APIGenerateDocs) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epGenerateDocs(ctx)
}

// --- enable_query -----------------------------------------------------------

type APIEnableQuery struct {
	EndpointPath string `arg:"endpoint_path" desc:"Path for the query endpoint (default \"_query\"); POST /api/<path>."`
	MaxRows      int    `arg:"max_rows"      desc:"Hard cap on rows returned per query; max 1000, a larger value is rejected." default:"200" min:"1" max:"1000"`
	RequiresAuth bool   `arg:"requires_auth" desc:"Require a Bearer token (project key or user JWT) to run queries. Default false: anyone can read every non-auth table, so set true unless all of them are public."`
	RateLimit    int    `arg:"rate_limit"    desc:"Requests/minute per IP." default:"60"`
}

func (a *APIEnableQuery) Doc() string {
	return "Enable a read-only raw-SELECT endpoint (POST /api/<path>) for join/aggregation/view queries an explorer or report needs at runtime."
}
func (a *APIEnableQuery) Example() any { return &APIEnableQuery{EndpointPath: "_query"} }
func (a *APIEnableQuery) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epEnableQuery(ctx, a.EndpointPath, a.MaxRows, a.RateLimit, a.RequiresAuth)
}
