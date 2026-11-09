package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/internal/endpoints"
	"openfabrica/internal/funcsandbox"
	"openfabrica/store"
	"openfabrica/tools"
)

// manage_function mounts a sandboxed, model-authored JS handler(req, ctx) at
// /api/<path>. It is the escape hatch for request logic the declarative tools
// cannot express: a computed response, an external API call with the key kept
// server-side and the result reshaped, multi-step validate-then-write, a
// derived/aggregated payload. The handler runs in internal/funcsandbox (goja,
// no require/fs/net) with the only I/O injected on ctx - ctx.query (read),
// ctx.insert/update/upsert (declared writes), ctx.fetch (SSRF-guarded), and
// ctx.secret(name) - so it is safe-by-construction, never an author-written
// backend. Validated at save with the same Compile the runtime uses.

// functionMethods is the closed set of HTTP verbs a function endpoint may answer.
var functionMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// timeoutCeilingMsg derives from the constant so the number quoted to the model
// cannot drift from the one enforced.
var timeoutCeilingMsg = fmt.Sprintf(
	"timeout_ms must be <= %d - the server stops writing the response at that point, so a longer budget only pins the handler on a request the client has already given up on. Move work that genuinely takes longer off the request: run the function on a schedule (manage_schedules create_schedule with a run_function native_action) or from a trigger.",
	maxFunctionTimeoutMS,
)

func init() {
	tools.Define(tools.Tool{
		Name: "manage_function",
		// Sized to the tool's own output: the 3000-byte default cut it mid-JSON.
		MaxResult: 16000,
		Category:  "api",
		OneLiner:  "Mount a sandboxed JS function at /api/<path> for custom request logic (compute, external API calls, multi-step writes).",
		WhenToUse: `Reach for this when a request needs COMPUTED server logic the declarative
tools can't express: a custom response shape, an external API call with the
API key kept server-side and the result reshaped, multi-step
validate-then-write, or a derived/aggregated payload. Prefer manage_api for
plain table CRUD (POST/GET/PUT/DELETE over one table) and a read-only view
endpoint for joins/aggregations; prefer manage_triggers for "when X happens do
Y" side-effects fired off a data/auth/payment event. The handler is sandboxed
JS - handler(req, ctx), synchronous (no async/await), no npm/fs/network beyond
ctx; the code arg and the server_functions guide carry the ctx surface. Grant
secrets explicitly via the secrets allowlist, and read the current source with
action=get before fixing it.`,
		Actions: []tools.Action{
			&FunctionCreate{},
			&FunctionUpdate{},
			&FunctionGet{},
			&FunctionList{},
			&FunctionDelete{},
		},
		Summarize: EpSummarize,
	})
}

// normalizeMethods validates a comma-separated method list and returns it
// uppercased and de-duplicated, or an error message. Empty defaults to POST.
func normalizeMethods(raw string) (string, string) {
	if strings.TrimSpace(raw) == "" {
		return "POST", ""
	}
	seen := map[string]bool{}
	out := make([]string, 0, 5)
	for _, m := range strings.Split(raw, ",") {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m == "" {
			continue
		}
		if !functionMethods[m] {
			return "", fmt.Sprintf("invalid HTTP method %q (allowed: GET, POST, PUT, PATCH, DELETE)", m)
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return "POST", ""
	}
	return strings.Join(out, ","), ""
}

// marshalSecrets validates the secret-allowlist names and returns the JSON to
// store. Each name must be a plain identifier so it can't smuggle anything odd
// through ctx.secret; the runtime resolves only names in this list.
func marshalSecrets(names []string) (string, string) {
	if len(names) == 0 {
		return "", ""
	}
	for _, n := range names {
		if !store.ValidColumnName.MatchString(n) {
			return "", fmt.Sprintf("invalid secret name %q (letters, digits, underscore; must start with a letter)", n)
		}
	}
	b, _ := json.Marshal(names)
	return string(b), ""
}

// --- create -----------------------------------------------------------------

type FunctionCreate struct {
	EndpointPath string   `arg:"endpoint_path,required" desc:"Literal path suffix (no /api/, no leading slash, no :param). The handler mounts at /api/<path>."`
	Code         string   `arg:"code,required"          desc:"Server-side handler JS. MUST define function handler(req, ctx) that RETURNS the response - either an envelope { status, body, headers } or a bare value (becomes 200 JSON; a string body is sent verbatim). req = { method, path, query, headers, body (parsed JSON), user:{id,role}|null }. ctx.query(sql, params) runs a read-only SELECT; ctx.insert(table,row) / ctx.update(table,patch,where) / ctx.upsert(table,row,[keyCol]) DECLARE writes applied after a successful return. Only ctx.query takes SQL - every write argument is a plain OBJECT the platform turns into a statement: where is equality matches ({id: 7} or {user_id: uid, post_id: pid}, keys ANDed), NOT a SQL string ('id = 7' throws a TypeError, and the response for a thrown handler is a generic 500 that will not name the type). patch supports {\"col\":{\"$increment\":n}} / {\"$decrement\":n} - prefer it over reading a value and writing back the sum, which loses concurrent updates. ctx.fetch(url,{method,headers,body}) is an SSRF-guarded outbound call; ctx.secret(name) reads a granted secret; ctx.llm(prompt,{system,max_tokens}) runs one completion on the project's ALREADY-configured LLM provider (no separate key) and returns the text - use it to summarize/classify/extract; ctx.checkout({amount, success_url, cancel_url, description, metadata}) opens a hosted payment checkout and returns {checkout_url, payment_id} (the payments guide); ctx.now() is the clock. Runs sandboxed and SYNCHRONOUS: no async/await, no require/import, no fs/network beyond ctx.fetch."`
	Methods      string   `arg:"methods"                desc:"Comma-separated HTTP methods to answer (GET,POST,PUT,PATCH,DELETE). Default POST."`
	RequiresAuth bool     `arg:"requires_auth"          desc:"When true, the request must carry a valid token; req.user is then {id, role}. When false req.user is null."`
	CorsOrigins  string   `arg:"cors_origins"           desc:"Comma-separated allowed CORS origins for cross-site browser calls. Omit for same-origin only."`
	RateLimit    int      `arg:"rate_limit"             desc:"Requests/minute per IP." default:"60"`
	TimeoutMS    int      `arg:"timeout_ms"             desc:"Per-invocation wall-clock cap in ms (0 = the platform default, 30000; max 120000). Raise for slow external calls."`
	Secrets      []string `arg:"secrets"                desc:"Allowlist of secret names (from manage_secrets) the handler may read via ctx.secret. A handler can read no secret not listed here."`
	Description  string   `arg:"description"            desc:"One line on what the function does and the JSON body it expects. A hosted agent that is allowed to call the function reads it to decide when to."`
}

func (a *FunctionCreate) Doc() string {
	return "Create (or replace) a sandboxed function endpoint at /api/<path> with a handler(req, ctx). Validated at save time."
}
func (a *FunctionCreate) Example() any {
	return &FunctionCreate{
		EndpointPath: "quote",
		Code:         "function handler(req, ctx) {\n  const rows = ctx.query('SELECT price FROM items WHERE id = ?', [req.body.id]);\n  if (!rows.length) return { status: 404, body: { error: 'not found' } };\n  return { total: rows[0].price * (req.body.qty || 1) };\n}",
	}
}
func (a *FunctionCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	path, errRes := tools.ValidateAndNormalizeEndpointPath(a.EndpointPath)
	if errRes != nil {
		return errRes, nil
	}
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	if strings.TrimSpace(a.Code) == "" {
		return tools.Fail("code is required"), nil
	}
	// Validate the handler with the SAME Compile the runtime uses, so a save that
	// would not load at runtime fails here with the reason.
	if _, err := funcsandbox.Compile(a.Code); err != nil {
		return tools.Fail("%s", "handler rejected (would not load): "+err.Error()), nil
	}
	methods, msg := normalizeMethods(a.Methods)
	if msg != "" {
		return tools.Fail("%s", msg), nil
	}
	secretsJSON, msg := marshalSecrets(a.Secrets)
	if msg != "" {
		return tools.Fail("%s", msg), nil
	}
	rateLimit := a.RateLimit
	if rateLimit < 0 {
		return tools.Fail("rate_limit must be >= 0"), nil
	}
	if rateLimit == 0 {
		rateLimit = defaultFunctionRateLimit
	}
	if a.TimeoutMS < 0 {
		return tools.Fail("timeout_ms must be >= 0"), nil
	}
	if a.TimeoutMS > maxFunctionTimeoutMS {
		return tools.Fail("%s", timeoutCeilingMsg), nil
	}

	// Claim the bare-path routes for the declared methods; a cross-kind collision
	// (e.g. a CRUD endpoint already answers GET /api/<path>) fails here.
	if err := ClaimEndpoint(ctx, endpoints.KindFunction, path, strings.Split(methods, ",")); err != nil {
		return tools.FailErr(err), nil
	}

	_, err := ctx.DB.Exec(
		`INSERT INTO of_function_endpoints (path, code, methods, requires_auth, cors_origins, rate_limit, timeout_ms, secrets, description)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   code = excluded.code,
		   methods = excluded.methods,
		   requires_auth = excluded.requires_auth,
		   cors_origins = excluded.cors_origins,
		   rate_limit = excluded.rate_limit,
		   timeout_ms = excluded.timeout_ms,
		   secrets = excluded.secrets,
		   description = excluded.description`,
		path, a.Code, methods, a.RequiresAuth, a.CorsOrigins, rateLimit, a.TimeoutMS, secretsJSON, strings.TrimSpace(a.Description),
	)
	if err != nil {
		return nil, fmt.Errorf("creating function endpoint: %w", err)
	}

	data := map[string]any{
		"endpoint_path": path,
		"route":         fmt.Sprintf("%s /api/%s", methods, path),
		"methods":       methods,
		"requires_auth": a.RequiresAuth,
		"usage":         functionUsage(path, methods, a.RequiresAuth),
	}
	if secretsJSON != "" {
		data["secrets"] = a.Secrets
	}
	return tools.Ok(data), nil
}

// functionUsage renders the client-facing call shape for a function endpoint. A GET
// carries its input in the query string (fetch throws on a GET with a body); the other
// verbs send a JSON body. An auth-gated function is called through the auth helper's
// apiFetch, which adds the Bearer header and JSON-encodes a plain-object body.
func functionUsage(path, methods string, requiresAuth bool) string {
	verb := "POST"
	if !strings.Contains(methods, "POST") {
		// Lead with the first configured verb when POST isn't offered.
		verb = strings.TrimSpace(strings.Split(methods, ",")[0])
	}
	var call string
	switch {
	case verb == "GET" && requiresAuth:
		call = fmt.Sprintf("import { apiFetch } from 'auth';\n  const data = await (await apiFetch('%s?' + new URLSearchParams({ ... }))).json();", path)
	case verb == "GET":
		call = fmt.Sprintf("const data = await (await fetch('/api/%s?' + new URLSearchParams({ ... }))).json();", path)
	case requiresAuth:
		call = fmt.Sprintf("import { apiFetch } from 'auth';\n  const data = await (await apiFetch('%s', { method: '%s', body: { ... } })).json();", path, verb)
	default:
		call = fmt.Sprintf("const data = await (await fetch('/api/%s', { method: '%s', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ... }) })).json();", path, verb)
	}
	return fmt.Sprintf(`CALL (from a page):
  %s
The handler reads req.query (GET) or req.body, and returns either { status, body, headers } or a bare value (200 JSON).
Server-side only: ctx.query/insert/update/upsert (DB), ctx.fetch (outbound), ctx.secret(name), ctx.llm(prompt), ctx.checkout({...}).`, call)
}

// --- update -----------------------------------------------------------------

type FunctionUpdate struct {
	EndpointPath string   `arg:"endpoint_path,required" desc:"Function endpoint path to update."`
	Code         string   `arg:"code"                   desc:"Replacement handler(req, ctx) source. Re-validated at save. Omit to leave the code unchanged."`
	Methods      string   `arg:"methods"                desc:"Replacement comma-separated method list."`
	RequiresAuth *bool    `arg:"requires_auth"          desc:"Toggle the auth requirement on/off. Omit to leave unchanged."`
	CorsOrigins  string   `arg:"cors_origins"           desc:"Replacement CORS origins."`
	RateLimit    int      `arg:"rate_limit"             desc:"Replacement rate limit (requests/minute per IP)."`
	TimeoutMS    int      `arg:"timeout_ms"             desc:"Replacement per-invocation timeout in ms (max 120000)."`
	Secrets      []string `arg:"secrets"                desc:"Replacement secret allowlist (replaces the whole list)."`
	Description  string   `arg:"description"            desc:"Replacement one-line description shown to a hosted agent that may call the function."`
}

func (a *FunctionUpdate) Doc() string {
	return "Update a function endpoint's handler code or options. Fix a bug by reading action=get, editing, then update with the new code."
}
func (a *FunctionUpdate) Example() any {
	rl := 120
	return &FunctionUpdate{EndpointPath: "quote", RateLimit: rl}
}
func (a *FunctionUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(a.EndpointPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	sets := []string{}
	vals := []any{}

	if a.Code != "" {
		if _, err := funcsandbox.Compile(a.Code); err != nil {
			return tools.Fail("%s", "handler rejected (would not load): "+err.Error()), nil
		}
		sets = append(sets, "code = ?")
		vals = append(vals, a.Code)
	}
	newMethods := ""
	if a.Methods != "" {
		methods, msg := normalizeMethods(a.Methods)
		if msg != "" {
			return tools.Fail("%s", msg), nil
		}
		newMethods = methods
		sets = append(sets, "methods = ?")
		vals = append(vals, methods)
	}
	if a.RequiresAuth != nil {
		sets = append(sets, "requires_auth = ?")
		vals = append(vals, *a.RequiresAuth)
	}
	if a.CorsOrigins != "" {
		sets = append(sets, "cors_origins = ?")
		vals = append(vals, a.CorsOrigins)
	}
	if a.RateLimit != 0 {
		if a.RateLimit < 0 {
			return tools.Fail("rate_limit must be >= 0"), nil
		}
		sets = append(sets, "rate_limit = ?")
		vals = append(vals, a.RateLimit)
	}
	if a.TimeoutMS != 0 {
		if a.TimeoutMS < 0 {
			return tools.Fail("timeout_ms must be >= 0"), nil
		}
		if a.TimeoutMS > maxFunctionTimeoutMS {
			return tools.Fail("%s", timeoutCeilingMsg), nil
		}
		sets = append(sets, "timeout_ms = ?")
		vals = append(vals, a.TimeoutMS)
	}
	if a.Secrets != nil {
		secretsJSON, msg := marshalSecrets(a.Secrets)
		if msg != "" {
			return tools.Fail("%s", msg), nil
		}
		sets = append(sets, "secrets = ?")
		vals = append(vals, secretsJSON)
	}
	if strings.TrimSpace(a.Description) != "" {
		sets = append(sets, "description = ?")
		vals = append(vals, strings.TrimSpace(a.Description))
	}
	if len(sets) == 0 {
		return tools.Fail("no fields to update"), nil
	}

	// A methods change moves the routes this endpoint answers, so re-claim the
	// new set before the config write - dispatch resolves against the spine, and
	// a method it does not carry is unroutable whatever the config row says.
	// Existence is checked first: claiming for a path with no config row would
	// leave the spine advertising routes nothing serves.
	if newMethods != "" {
		var exists int
		if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_function_endpoints WHERE path = ?", path).Scan(&exists); err != nil {
			return nil, fmt.Errorf("updating function endpoint: %w", err)
		}
		if exists == 0 {
			return tools.Fail("function endpoint %q not found - create it first", path), nil
		}
		if err := ClaimEndpoint(ctx, endpoints.KindFunction, path, strings.Split(newMethods, ",")); err != nil {
			return tools.FailErr(err), nil
		}
	}

	vals = append(vals, path)
	res, err := ctx.DB.Exec(fmt.Sprintf("UPDATE of_function_endpoints SET %s WHERE path = ?", strings.Join(sets, ", ")), vals...)
	if err != nil {
		return nil, fmt.Errorf("updating function endpoint: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tools.Fail("function endpoint %q not found - create it first", path), nil
	}
	return tools.Ok(map[string]any{"endpoint_path": path, "updated": true}), nil
}

// --- get --------------------------------------------------------------------

type FunctionGet struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Function endpoint to fetch, INCLUDING its handler source. Read the source here, fix the offending line, then update - don't blind-rewrite."`
}

func (a *FunctionGet) Doc() string {
	return "Fetch one function endpoint's full config plus its current handler source. Use this before fixing a handler."
}
func (a *FunctionGet) ReadOnly() bool { return true }
func (a *FunctionGet) Example() any   { return &FunctionGet{EndpointPath: "quote"} }
func (a *FunctionGet) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(a.EndpointPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	var (
		id, rateLimit, timeoutMS     int
		code, methods, cors, secrets string
		description                  string
		requiresAuth                 bool
	)
	err := ctx.DB.QueryRow(
		"SELECT id, code, methods, COALESCE(requires_auth,0), COALESCE(cors_origins,''), COALESCE(rate_limit,60), COALESCE(timeout_ms,0), COALESCE(secrets,''), COALESCE(description,'') FROM of_function_endpoints WHERE path = ?",
		path,
	).Scan(&id, &code, &methods, &requiresAuth, &cors, &rateLimit, &timeoutMS, &secrets, &description)
	if err == sql.ErrNoRows {
		return tools.Fail("function endpoint %q not found", path), nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting function endpoint: %w", err)
	}
	data := map[string]any{
		"endpoint_path": path, "id": id, "code": code, "methods": methods,
		"requires_auth": requiresAuth, "cors_origins": cors,
		"rate_limit": rateLimit, "timeout_ms": timeoutMS,
	}
	if description != "" {
		data["description"] = description
	}
	if secrets != "" {
		var names []string
		_ = json.Unmarshal([]byte(secrets), &names)
		data["secrets"] = names
	}
	return tools.Ok(data), nil
}

// --- list -------------------------------------------------------------------

type FunctionList struct{}

func (a *FunctionList) Doc() string {
	return "List every function endpoint (paths, methods, flags; not the source - use get for that)."
}
func (a *FunctionList) ReadOnly() bool { return true }
func (a *FunctionList) Example() any   { return &FunctionList{} }
func (a *FunctionList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query("SELECT id, path, methods, COALESCE(requires_auth,0), COALESCE(rate_limit,60), COALESCE(timeout_ms,0), created_at FROM of_function_endpoints ORDER BY path")
	if err != nil {
		return nil, fmt.Errorf("listing function endpoints: %w", err)
	}
	defer rows.Close()
	endpoints := []map[string]any{}
	for rows.Next() {
		var id, rateLimit, timeoutMS int
		var path, methods, createdAt string
		var requiresAuth bool
		if rows.Scan(&id, &path, &methods, &requiresAuth, &rateLimit, &timeoutMS, &createdAt) != nil {
			continue
		}
		endpoints = append(endpoints, map[string]any{
			"id": id, "endpoint_path": path, "methods": methods, "requires_auth": requiresAuth,
			"rate_limit": rateLimit, "timeout_ms": timeoutMS, "route": "/api/" + path, "created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing function endpoints: %w", err)
	}
	return tools.Ok(endpoints), nil
}

// --- delete -----------------------------------------------------------------

type FunctionDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Function endpoint path to delete."`
}

func (a *FunctionDelete) Doc() string  { return "Delete a function endpoint by path." }
func (a *FunctionDelete) Example() any { return &FunctionDelete{EndpointPath: "quote"} }
func (a *FunctionDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return DeleteEndpointByPath(ctx, "of_function_endpoints", "function endpoint", "manage_function", "delete", a.EndpointPath)
}
