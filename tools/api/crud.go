package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"openfabrica/internal/endpoints"
	"openfabrica/store"
	"openfabrica/tools"
)

// REST CRUD + OpenAPI docs handlers for manage_api. Schema lives in manage_api.go.

// API endpoint actions

// resolveOwnerColumn settles the endpoint's row-level-security owner column: an
// explicit owner_column (validated), else a single FK to the auth table, else an
// owner-like column by heuristic. Any adopted scope forces requires_auth on, so
// the possibly-updated flag is returned. autoScoped reports that the heuristic
// (not an explicit arg or an FK) picked it, which the caller surfaces as a nudge.
// Skipped for read-only views and public endpoints, which have no scope to apply.
func resolveOwnerColumn(ctx *tools.ToolContext, ownerColumnStr, tableName string, readOnly, requiresAuth, publicRead, publicWrite bool) (*string, bool, bool, error) {
	// Row-level security: when owner_column is set, queries are scoped to the
	// authenticated user's ID. Requires requires_auth to be effective. The
	// value is interpolated into RLS WHERE clauses, so it must pass the
	// canonical identifier validator before being persisted.
	var ownerColumn *string
	if ownerColumnStr != "" {
		oc := ownerColumnStr
		if err := validateColumnRef("owner_column", oc); err != nil {
			return nil, requiresAuth, false, err
		}
		ownerColumn = &oc
		requiresAuth = true // owner scoping implies auth required
	}

	// Auto-detect owner_column from FK references to the auth table when not explicitly set.
	// Skipped for read-only view endpoints: views have no RLS surface to scope.
	// Also skipped for public_write endpoints: an anonymous writer has no identity
	// to own the row, and adopting a scope would re-gate the writes we just opened.
	// Gated on the same preconditions as the owner-like guard below - an endpoint
	// the caller declared public (auth_model=public, or public_read) is never
	// silently narrowed: a scope adopted there also forces auth on, and the
	// runtime applies the owner filter whenever an optional token is present, so
	// the endpoint would show anonymous callers every row and the signed-in owner
	// only their own.
	autoScopeEligible := !readOnly && requiresAuth && !publicRead && !publicWrite
	if ownerColumn == nil && autoScopeEligible {
		authTables := make(map[string]bool)
		if authRows, err := ctx.DB.Query("SELECT table_name FROM of_auth_endpoints"); err == nil {
			for authRows.Next() {
				var tbl string
				if authRows.Scan(&tbl) == nil {
					authTables[tbl] = true
				}
			}
			_ = authRows.Err() // best-effort detection; tolerated like the Query error
			authRows.Close()
		}
		if len(authTables) > 0 {
			var candidates []string
			if fkRows, err := ctx.DB.Query(fmt.Sprintf("PRAGMA foreign_key_list(%s)", tableName)); err == nil {
				for fkRows.Next() {
					var id, seq int
					var refTable, from, to, onUpdate, onDelete, match string
					if fkRows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match) == nil {
						if authTables[refTable] {
							candidates = append(candidates, from)
						}
					}
				}
				_ = fkRows.Err() // best-effort detection; tolerated like the Query error
				fkRows.Close()
			}
			if len(candidates) == 1 {
				ownerColumn = &candidates[0]
			}
		}
	}

	// Source-side RLS guard (prevent-at-source for the rls_scope defect class):
	// an authenticated, non-public endpoint over a row-owned but unscoped table is
	// a cross-user leak the build agent routinely finishes past the advisory below.
	// When the FK auto-detect found nothing but the table carries an owner-like
	// column (user_id/owner_id/created_by/... - a per-user reference without a
	// formal FK, exactly how such leaks slip through), adopt it as
	// owner_column so the endpoint ships scoped by default. Uses
	// store.OwnerLikeColumn - the same heuristic the build-end rls_scope validator
	// (engine/validator_rls.go) uses - so guard and gate never drift. Applied only
	// to writable, auth-required, non-public-read endpoints: an intentionally
	// public table (public_read) is never silently narrowed (the validator keeps
	// those a speculative warning, not an error).
	autoScoped := false
	if ownerColumn == nil && autoScopeEligible {
		if col := ownerLikeColumnForTable(ctx, tableName); col != "" {
			ownerColumn = &col
			autoScoped = true
		}
	}

	return ownerColumn, requiresAuth, autoScoped, nil
}

// epCreateAPI takes typed args straight from APICreate. authModel is the raw
// enum (public/authenticated/role/owner); it is translated here into the
// requires_auth / required_role / owner_column shape the row stores. Empty/zero
// optional fields fall back to the same defaults the prior map path used.
func epCreateAPI(ctx *tools.ToolContext, a *APICreate) (*tools.Result, error) {
	tableName := a.TableName
	methodsArg := a.Methods
	publicColumnsArg := a.PublicColumns
	authModel := a.AuthModel
	readIsPublic := a.ReadIsPublic
	writeIsPublic := a.WriteIsPublic
	rateLimitArg := a.RateLimit
	cacheTTLArg := a.CacheTTL
	lookupColumnArg := a.LookupColumn

	path, errRes := tools.ValidateAndNormalizeEndpointPath(a.EndpointPath)
	if errRes != nil {
		return errRes, nil
	}
	if path == "" || tableName == "" {
		return tools.Fail("endpoint_path and table_name are required"), nil
	}
	// public_write is the "anyone can POST, only an admin reads" shape. It only
	// makes sense on an auth-gated endpoint (with auth_model=public everything is
	// already open) and is incompatible with owner_column: an anonymous writer has
	// no identity to scope the row to. Reject the contradictions at create-time.
	if msg := validatePublicWrite(writeIsPublic, authModel, a.OwnerColumn); msg != "" {
		return tools.Fail("%s", msg), nil
	}
	if msg := validateAuthModelArgs(authModel, a.RequiredRole, a.OwnerColumn); msg != "" {
		return tools.FailCode(tools.CodeBadArg, "%s", msg), nil
	}
	// authored names the row's author column, so it cannot be inferred - require it.
	if authModel == "authored" && a.OwnerColumn == "" {
		return tools.Fail("auth_model=authored requires owner_column (the column naming each row's author): reads stay shared across users while writes are scoped to that column"), nil
	}

	// Translate auth_model into the persisted requires_auth / required_role /
	// owner_column shape. owner_scope carries whether owner_column scopes reads too
	// (empty) or writes only (authored).
	requiresAuthFromModel, requiredRoleStr, ownerColumnStr := translateAuthModel(authModel, a.RequiredRole, a.OwnerColumn)
	ownerScopeStr := ownerScopeForModel(authModel)
	// CORS arrays become comma-separated strings, matching the prior shape.
	corsOrigins := strings.Join(a.CORSOrigins, ",")
	corsMethods := strings.Join(a.CORSMethods, ",")
	corsHeaders := strings.Join(a.CORSHeaders, ",")

	if msg := validateMethods(methodsArg); msg != "" {
		return tools.FailCode(tools.CodeBadArg, "%s", msg), nil
	}

	// Verify the backing object exists. A registered dynamic table backs full
	// CRUD; a SQL VIEW (created via manage_schema create_view) backs a read-only
	// GET endpoint - views carry no CRUD metadata and cannot be written, so they
	// are served list/single-row only. Anything else is an error.
	var tableExists int
	err := ctx.DB.QueryRow(
		"SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?",
		tableName,
	).Scan(&tableExists)
	if err != nil {
		return nil, fmt.Errorf("checking dynamic table: %w", err)
	}
	readOnly := false
	if tableExists == 0 {
		var viewExists int
		if err := ctx.DB.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'view' AND name = ?",
			tableName,
		).Scan(&viewExists); err != nil {
			return nil, fmt.Errorf("checking view: %w", err)
		}
		if viewExists == 0 {
			return tools.Fail("'%s' is neither a dynamic table nor a view; create the table first with manage_schema create_table (or the view with manage_schema create_view)", tableName), nil
		}
		if authModel != "" && authModel != "public" || requiredRoleStr != "" || ownerColumnStr != "" {
			return tools.FailCode(tools.CodeBadArg, "'%s' is a view: a view-backed endpoint is read-only and world-readable (a view has no owner or role column to scope by), so auth_model=%s would be silently dropped. To gate the data, serve it from a table endpoint with an auth_model, or a function endpoint with requires_auth", tableName, modelOrPublic(authModel)), nil
		}
		// View-backed endpoint: read-only. Force GET-only and public read, and
		// drop any auth/RLS model (views have no owner/tenant columns and the
		// table-only auto-detection below is skipped for them).
		readOnly = true
		methodsArg = []string{"GET"}
		readIsPublic = true
		writeIsPublic = false
		requiresAuthFromModel = false
		requiredRoleStr = ""
		ownerColumnStr = ""
	}

	// Parse methods, default to full CRUD since endpoints are table-bound.
	methods := []string{"GET", "POST", "PUT", "DELETE"}
	if len(methodsArg) > 0 {
		methods = append([]string(nil), methodsArg...)
	}
	methodsJSON, _ := json.Marshal(methods)

	// Skip if endpoint already exists (prevents duplicate creation during BUILD).
	var existingMethodsJSON string
	if err := ctx.DB.QueryRow("SELECT COALESCE(methods,'') FROM of_api_endpoints WHERE path = ?", path).Scan(&existingMethodsJSON); err == nil {
		// Still (re-)claim the spine: ClaimEndpoint is idempotent, and a claim
		// that failed on the original create would otherwise never heal - this
		// branch returns before the claim below. Claim the STORED methods, not
		// the requested ones: this path skips the config write, so the stored set
		// is what dispatch has to match.
		stored := methods
		if existingMethodsJSON != "" {
			var parsed []string
			if json.Unmarshal([]byte(existingMethodsJSON), &parsed) == nil && len(parsed) > 0 {
				stored = parsed
			}
		}
		if err := ClaimEndpoint(ctx, endpoints.KindCRUD, path, stored); err != nil {
			return tools.FailErr(err), nil
		}
		return tools.Ok(map[string]any{
			"endpoint_path":   path,
			"already_existed": true,
			"message":         "Endpoint /api/" + path + " already exists, so nothing was created and NO option in this call was applied. Change its config with manage_api action=update.",
		}), nil
	}

	writableColsJSON, errRes, err := writableColumnsJSON(ctx, tableName, a.WritableColumns)
	if err != nil {
		return nil, err
	}
	if errRes != nil {
		return errRes, nil
	}

	// Parse public_columns.
	var publicColsJSON *string
	if len(publicColumnsArg) > 0 {
		j, _ := json.Marshal(publicColumnsArg)
		s := string(j)
		publicColsJSON = &s
	}

	requiresAuth := requiresAuthFromModel

	publicRead := readIsPublic
	publicWrite := writeIsPublic

	// Role-based access control: if required_role is set, implies requires_auth.
	var requiredRole *string
	if requiredRoleStr != "" {
		rr := requiredRoleStr
		requiredRole = &rr
		requiresAuth = true
	}

	rateLimit := defaultAPIRateLimit
	if rateLimitArg > 0 {
		rateLimit = rateLimitArg
	}

	ownerColumn, requiresAuth, autoScopedOwner, err := resolveOwnerColumn(ctx, ownerColumnStr, tableName, readOnly, requiresAuth, publicRead, publicWrite)
	if err != nil {
		return nil, err
	}

	// Response caching: cache_ttl in seconds (0 = no caching).
	cacheTTL := 0
	if cacheTTLArg > 0 {
		cacheTTL = cacheTTLArg
	}

	// lookup_column: column that resolves /api/<path>/<value>. When unset the
	// runtime auto-detects (see rowIDColumn in server/public). Validated here
	// so a bad identifier fails at create-time rather than silently downgrading
	// to fallback resolution at request time.
	var lookupColumn *string
	if lookupColumnArg != "" {
		lc := lookupColumnArg
		if err := validateColumnRef("lookup_column", lc); err != nil {
			return nil, err
		}
		lookupColumn = &lc
	}

	// Claim the routes in the endpoint spine first: a cross-kind collision (e.g.
	// a function already owns POST /api/<path>) fails here before any config row
	// is written.
	if err := ClaimEndpoint(ctx, endpoints.KindCRUD, path, methods); err != nil {
		return tools.FailErr(err), nil
	}

	_, err = ctx.DB.Exec(
		`INSERT INTO of_api_endpoints (path, table_name, methods, public_columns, writable_columns, requires_auth, public_read, public_write, required_role, rate_limit, cors_origins, cors_methods, cors_headers, owner_column, owner_scope, lookup_column, cache_ttl, read_only)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   table_name = excluded.table_name,
		   methods = excluded.methods,
		   public_columns = excluded.public_columns,
		   writable_columns = excluded.writable_columns,
		   requires_auth = excluded.requires_auth,
		   public_read = excluded.public_read,
		   public_write = excluded.public_write,
		   required_role = excluded.required_role,
		   rate_limit = excluded.rate_limit,
		   cors_origins = excluded.cors_origins,
		   cors_methods = excluded.cors_methods,
		   cors_headers = excluded.cors_headers,
		   owner_column = excluded.owner_column,
		   owner_scope = excluded.owner_scope,
		   lookup_column = excluded.lookup_column,
		   cache_ttl = excluded.cache_ttl,
		   read_only = excluded.read_only`,
		path, tableName, string(methodsJSON), publicColsJSON, writableColsJSON, requiresAuth, publicRead, publicWrite, requiredRole, rateLimit, corsOrigins, corsMethods, corsHeaders, ownerColumn, ownerScopeStr, lookupColumn, cacheTTL, readOnly,
	)
	if err != nil {
		return nil, fmt.Errorf("creating API endpoint: %w", err)
	}

	data := map[string]any{
		"endpoint_path": path,
		"table_name":    tableName,
		"methods":       methods,
		"requires_auth": requiresAuth,
		"public_read":   publicRead,
		"rate_limit":    rateLimit,
	}
	if publicWrite {
		data["public_write"] = true
	}
	if readOnly {
		data["read_only"] = true
		data["backing"] = "view"
	}
	if requiredRole != nil {
		data["required_role"] = *requiredRole
	}
	if ownerColumn != nil {
		data["owner_column"] = *ownerColumn
	}
	if ownerScopeStr == store.OwnerScopeAuthored {
		data["owner_scope"] = ownerScopeStr
		data["owner_scope_note"] = fmt.Sprintf("authored: every authenticated user reads all rows, but %s is stamped on insert and a caller can only PUT/DELETE rows they authored. Use this for shared content each user writes their own of (chat messages, board cards, comments).", *ownerColumn)
	}
	if autoScopedOwner {
		data["owner_scope_applied"] = fmt.Sprintf("This endpoint was auto-scoped to owner_column=%q (each user reads AND writes only their own rows) because %s is a row-owned table exposed to authenticated users: without it every signed-in user could read and write every other user's rows. If these rows are SHARED content that members co-read while each edits their own (chat messages, comments, a shared board), switch to `manage_api action=update endpoint_path=%s auth_model=authored owner_column=%s` so reads stay shared and only the author can edit - do NOT reach for public_read, which shares reads with anonymous callers but still hides other users' rows from signed-in ones. For hard multi-tenant isolation, use manage_tenancy action=add_tenant_column. If the rows are private per user, no action needed.", *ownerColumn, tableName, path, *ownerColumn)
	}
	if lookupColumn != nil {
		data["lookup_column"] = *lookupColumn
	}
	if writableColsJSON != nil {
		data["writable_columns"] = a.WritableColumns
	} else if !readOnly && (methodAllowed(methods, "POST") || methodAllowed(methods, "PUT")) {
		if w := writableColumnsWarning(ctx, tableName, path); w != "" {
			data["write_warning"] = w
		}
	}
	// Single-row addressing sanity: /api/<path>/<value> resolves exactly ONE row by
	// lookup_column. If that column carries no UNIQUE constraint, GET/PUT/DELETE by
	// value hit an arbitrary matching row and PUT cannot act as a deterministic
	// upsert key (e.g. a non-unique position column makes PUT fail). Non-fatal nudge; the endpoint is created either way.
	if !readOnly && lookupColumn != nil && singleRowRouteEnabled(methods) && !lookupColumnIsUnique(ctx, tableName, *lookupColumn) {
		data["key_warning"] = fmt.Sprintf("lookup_column %q on %q has no UNIQUE constraint, so GET/PUT/DELETE /api/%s/<value> resolves an arbitrary matching row and PUT cannot upsert deterministically by it. Add a UNIQUE index on %s (manage_schema), address rows by the integer id instead, or mutate via LIST ?%s= filters + POST.", *lookupColumn, tableName, path, *lookupColumn, *lookupColumn)
	}
	// Safety net: nudge the builder when an endpoint exposes a table that has an
	// owner/tenant-like column but is left unscoped, so every caller can read
	// every row. Non-fatal - the endpoint is created either way; the warning
	// surfaces in the tool result so a per-user table doesn't ship world-readable.
	// Views have no RLS surface, so the check is table-only.
	if !readOnly {
		oc := ""
		if ownerColumn != nil {
			oc = *ownerColumn
		}
		if w := rlsScopeWarningForTable(ctx, tableName, oc, "", publicRead); w != "" {
			data["rls_warning"] = w
		}
	}
	// Safety net: an endpoint that requires a logged-in user, in a project with no
	// login surface at all, 401s every write from the browser. Non-fatal nudge
	// here; the build-end auth_gate validator (engine) is the hard gate. Both decide
	// "auth missing?" via store.ProjectHasAuth, so save and build cannot drift.
	if requiresAuthFromModel && !store.ProjectHasAuth(ctx.DB) {
		scope := "writes"
		if !readIsPublic && !publicWrite {
			scope = "reads and writes"
		} else if publicWrite {
			scope = "reads"
		}
		data["auth_warning"] = fmt.Sprintf("Endpoint /api/%s requires a logged-in user (auth_model=%s) but this project has no login (no create_auth/create_oauth), so %s will 401 from the browser. Either add an auth endpoint, use auth_model=public (with public_read for open reads), or public_write=true for a public-submission endpoint (contact form) whose reads stay gated.", path, authModel, scope)
	}
	// Hand back the real, live response contract so the caller never has to
	// discover it by probing: the LIST envelope vs the bare-row shape, the
	// single-row sub-route, and the actual column names/types read back from
	// the table. Promotes the "LIST is the envelope" fact out of the tool hints
	// (which a probe-happy agent ignores) and into the create result itself.
	lookup := "id"
	if lookupColumn != nil {
		lookup = *lookupColumn
	}
	data["response_contract"] = apiResponseContract(ctx, path, tableName, methods, lookup)
	return tools.Ok(data), nil
}

// apiResponseContract describes what each method of a freshly created CRUD
// endpoint returns, derived deterministically from the backing table's live
// schema. The LIST route answers the envelope; single-row GET/POST/PUT answer
// the bare row. Column info is read back via PRAGMA so the shape is verified,
// not echoed from the create args.
func apiResponseContract(ctx *tools.ToolContext, path, tableName string, methods []string, lookup string) map[string]any {
	enabled := map[string]bool{}
	for _, m := range methods {
		enabled[strings.ToUpper(m)] = true
	}
	var cols []store.ColumnInfo
	if physical, err := store.SanitizedTableName(tableName); err == nil {
		cols, _ = store.GetTableColumnInfo(ctx.DB, physical)
	}
	contract := map[string]any{}
	if enabled["GET"] {
		contract["list"] = map[string]any{
			"route": "GET /api/" + path,
			"shape": "{ rows: [<row>], count, limit, offset }",
			"note":  "LIST returns the envelope - the array is under `rows`, not at the top level and never a second `.data`. Paginate with ?limit=&offset=.",
		}
		contract["row"] = map[string]any{
			"route":   "GET /api/" + path + "/<" + lookup + ">",
			"shape":   "the bare row object (no envelope)",
			"columns": cols,
		}
	}
	var writes []string
	if enabled["POST"] {
		writes = append(writes, "POST /api/"+path+" -> created row (201)")
	}
	if enabled["PUT"] {
		writes = append(writes, "PUT /api/"+path+"/<"+lookup+"> -> updated row")
	}
	if enabled["DELETE"] {
		writes = append(writes, "DELETE /api/"+path+"/<"+lookup+"> -> { success: true } (200)")
	}
	if len(writes) > 0 {
		contract["writes"] = writes
	}
	return contract
}

// singleRowRouteEnabled reports whether any method uses the single-row
// /api/<path>/<value> route (GET-by-value, PUT, or DELETE). LIST-only (bare GET
// with no writes) still counts because GET enables the single-row read.
func singleRowRouteEnabled(methods []string) bool {
	for _, m := range methods {
		switch strings.ToUpper(m) {
		case "GET", "PUT", "DELETE":
			return true
		}
	}
	return false
}

// lookupColumnIsUnique reports whether column uniquely identifies a row in
// tableName: it is a primary-key column, or is covered by a single-column UNIQUE
// index. The single-row route resolves one row by this column, so a non-unique
// lookup_column addresses an arbitrary match and can't be a deterministic upsert
// key. Fail-closed: any PRAGMA/scan hiccup returns false (surface the nudge rather
// than suppress it).
func lookupColumnIsUnique(ctx *tools.ToolContext, tableName, column string) bool {
	physical, err := store.SanitizedTableName(tableName)
	if err != nil {
		return false
	}
	// A primary-key column is uniquely addressable.
	if infoRows, err := ctx.DB.Query(fmt.Sprintf("PRAGMA table_info(%s)", physical)); err == nil {
		for infoRows.Next() {
			var cid, notnull, pk int
			var name, ctype string
			var dflt any
			if infoRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk) == nil && name == column && pk > 0 {
				infoRows.Close()
				return true
			}
		}
		err = infoRows.Err()
		infoRows.Close()
		if err != nil {
			return false
		}
	}
	// A single-column UNIQUE index covering exactly this column.
	idxRows, err := ctx.DB.Query(fmt.Sprintf("PRAGMA index_list(%s)", physical))
	if err != nil {
		return false
	}
	var uniqueIdx []string
	for idxRows.Next() {
		var seq, unique, partial int
		var name, origin string
		if idxRows.Scan(&seq, &name, &unique, &origin, &partial) == nil && unique == 1 {
			uniqueIdx = append(uniqueIdx, name)
		}
	}
	err = idxRows.Err()
	idxRows.Close()
	if err != nil {
		return false
	}
	for _, name := range uniqueIdx {
		ciRows, err := ctx.DB.Query(fmt.Sprintf("PRAGMA index_info(%s)", name))
		if err != nil {
			continue
		}
		var cols []string
		for ciRows.Next() {
			var seqno, cid int
			var cname string
			if ciRows.Scan(&seqno, &cid, &cname) == nil {
				cols = append(cols, cname)
			}
		}
		err = ciRows.Err()
		ciRows.Close()
		if err != nil {
			continue
		}
		if len(cols) == 1 && cols[0] == column {
			return true
		}
	}
	return false
}

// epUpdateAPI takes typed args straight from APIUpdate. authModel is translated
// into the requires_auth / required_role shape (only the non-public side, as
// the prior merge path never emitted requires_auth=false). Each field is
// patched only when set, matching that map shape.
//
// Fix (arg-forwarding drift): APIUpdate declares table_name but the prior
// handler never read it, so re-pointing an endpoint at a new table silently
// no-oped. table_name is now patched when non-empty.
func epUpdateAPI(ctx *tools.ToolContext, a *APIUpdate) (*tools.Result, error) {
	tableName := a.TableName
	methodsArg := a.Methods
	publicColumnsArg := a.PublicColumns
	authModel := a.AuthModel
	requiredRoleArg := a.RequiredRole
	ownerColumnArg := a.OwnerColumn
	readIsPublic := a.ReadIsPublic
	writeIsPublic := a.WriteIsPublic
	rateLimitArg := a.RateLimit
	cacheTTLArg := a.CacheTTL
	lookupColumnArg := a.LookupColumn
	corsOriginsArg := a.CORSOrigins
	corsMethodsArg := a.CORSMethods
	corsHeadersArg := a.CORSHeaders

	path := tools.NormalizeEndpointPath(a.EndpointPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}

	setClauses := []string{}
	var vals []any

	if tableName != "" {
		var n int
		_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&n)
		if n == 0 {
			return tools.FailCode(tools.CodeNotFound, "table %q not found - an endpoint can only move to an existing table (manage_schema list_tables)", tableName), nil
		}
		setClauses = append(setClauses, "table_name = ?")
		vals = append(vals, tableName)
	}
	if msg := validateMethods(methodsArg); msg != "" {
		return tools.FailCode(tools.CodeBadArg, "%s", msg), nil
	}
	if len(methodsArg) > 0 {
		methodsJSON, _ := json.Marshal(methodsArg)
		setClauses = append(setClauses, "methods = ?")
		vals = append(vals, string(methodsJSON))
	}
	if len(publicColumnsArg) > 0 {
		b, _ := json.Marshal(publicColumnsArg)
		setClauses = append(setClauses, "public_columns = ?")
		vals = append(vals, string(b))
	}
	if len(a.WritableColumns) > 0 {
		table := tableName
		if table == "" {
			_ = ctx.DB.QueryRow("SELECT table_name FROM of_api_endpoints WHERE path = ?", path).Scan(&table)
		}
		wj, errRes, err := writableColumnsJSON(ctx, table, a.WritableColumns)
		if err != nil {
			return nil, err
		}
		if errRes != nil {
			return errRes, nil
		}
		setClauses = append(setClauses, "writable_columns = ?")
		if wj == nil {
			vals = append(vals, nil)
		} else {
			vals = append(vals, *wj)
		}
	}
	// auth_model, when supplied, is the single source of truth for the whole auth
	// shape, so rewrite EVERY auth column to its computed value - including back to
	// false/empty. The prior merge path only ever set flags to true, so relaxing an
	// endpoint (e.g. authenticated -> public, or turning on public_read/public_write)
	// silently no-op'd and forced a delete+recreate to clear the stale value. Gated
	// on authModel != "" so a metadata-only update (rate_limit, cors, ...) leaves
	// auth untouched.
	if authModel == "" && (readIsPublic || writeIsPublic || requiredRoleArg != "" || ownerColumnArg != "") {
		return tools.FailCode(tools.CodeBadArg, "public_read, public_write, required_role and owner_column take effect only when auth_model rides the same call; without it they are ignored. Re-send the endpoint's current auth_model (manage_api list shows it) together with the change"), nil
	}
	if authModel != "" {
		if msg := validatePublicWrite(writeIsPublic, authModel, ownerColumnArg); msg != "" {
			return tools.Fail("%s", msg), nil
		}
		if msg := validateAuthModelArgs(authModel, requiredRoleArg, ownerColumnArg); msg != "" {
			return tools.FailCode(tools.CodeBadArg, "%s", msg), nil
		}
		if authModel == "authored" && ownerColumnArg == "" {
			return tools.Fail("auth_model=authored requires owner_column (the column naming each row's author): reads stay shared across users while writes are scoped to that column"), nil
		}
		// owner scopes by a column. Re-sending the model alone (as the arg description
		// says to) must keep the stored column, and an endpoint with none cannot become
		// owner-scoped by naming the model: that would silently leave it unscoped.
		if authModel == "owner" && ownerColumnArg == "" {
			var stored sql.NullString
			_ = ctx.DB.QueryRow("SELECT owner_column FROM of_api_endpoints WHERE path = ?", path).Scan(&stored)
			if strings.TrimSpace(stored.String) == "" {
				return tools.FailCode(tools.CodeBadArg, "auth_model=owner requires owner_column (the column holding each row's owner id): this endpoint has none stored, so owner scoping cannot be applied without it"), nil
			}
			ownerColumnArg = stored.String
		}
		reqAuth, role, owner := translateAuthModel(authModel, requiredRoleArg, ownerColumnArg)
		if owner != "" {
			if err := validateColumnRef("owner_column", owner); err != nil {
				return nil, err
			}
		}
		// Empty role/owner/scope persist as ""; loadEndpoint COALESCEs and treats ""
		// as "no role"/"no owner"/"scope reads and writes", identical to NULL, so
		// switching away from authored back to owner or authenticated clears the scope.
		setClauses = append(setClauses,
			"requires_auth = ?", "public_read = ?", "public_write = ?",
			"required_role = ?", "owner_column = ?", "owner_scope = ?")
		vals = append(vals, reqAuth, readIsPublic, writeIsPublic, role, owner, ownerScopeForModel(authModel))
	}
	if rateLimitArg != 0 {
		setClauses = append(setClauses, "rate_limit = ?")
		vals = append(vals, rateLimitArg)
	}
	// cache_ttl on update: a positive value sets the TTL; -1 explicitly DISABLES
	// caching (writes 0). A bare 0 stays "unset" (leave the existing TTL untouched)
	// because the reflected int cannot tell an omitted arg from an explicit 0 - so
	// -1 is how the model turns caching off without a delete+recreate. Mirrors the
	// cache_ttl arg desc.
	if cacheTTLArg != 0 {
		ttl := cacheTTLArg
		if ttl < 0 {
			ttl = 0
		}
		setClauses = append(setClauses, "cache_ttl = ?")
		vals = append(vals, ttl)
	}
	if len(corsOriginsArg) > 0 {
		setClauses = append(setClauses, "cors_origins = ?")
		vals = append(vals, strings.Join(corsOriginsArg, ","))
	}
	if len(corsMethodsArg) > 0 {
		setClauses = append(setClauses, "cors_methods = ?")
		vals = append(vals, strings.Join(corsMethodsArg, ","))
	}
	if len(corsHeadersArg) > 0 {
		setClauses = append(setClauses, "cors_headers = ?")
		vals = append(vals, strings.Join(corsHeadersArg, ","))
	}
	if lookupColumnArg != "" {
		if err := validateColumnRef("lookup_column", lookupColumnArg); err != nil {
			return nil, err
		}
		setClauses = append(setClauses, "lookup_column = ?")
		vals = append(vals, lookupColumnArg)
	}

	if len(setClauses) == 0 {
		return tools.Fail("no fields to update"), nil
	}

	// A methods change moves the routes this endpoint answers, so re-claim the
	// new set before the config write - dispatch resolves against the spine, and
	// a method it does not carry is unroutable whatever the config row says.
	// Existence is checked first: claiming for a path with no config row would
	// leave the spine advertising routes nothing serves.
	if len(methodsArg) > 0 {
		var exists int
		if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_api_endpoints WHERE path = ?", path).Scan(&exists); err != nil {
			return nil, fmt.Errorf("updating endpoint: %w", err)
		}
		if exists == 0 {
			return tools.Fail("endpoint '%s' not found", path), nil
		}
		if err := ClaimEndpoint(ctx, endpoints.KindCRUD, path, methodsArg); err != nil {
			return tools.FailErr(err), nil
		}
	}

	vals = append(vals, path)
	query := fmt.Sprintf("UPDATE of_api_endpoints SET %s WHERE path = ?", strings.Join(setClauses, ", "))
	res, err := ctx.DB.Exec(query, vals...)
	if err != nil {
		return nil, fmt.Errorf("updating endpoint: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tools.Fail("endpoint '%s' not found", path), nil
	}
	data := map[string]any{"endpoint_path": path, "updated": true}
	// Same auth-gate nudge as create: tightening an endpoint to require a logged-in
	// user in a project with no login 401s its writes from the browser. A
	// public_write endpoint keeps its writes open, so only the gated reads 401.
	if authModelRequiresAuth(authModel) && !store.ProjectHasAuth(ctx.DB) {
		scope := "writes"
		if !readIsPublic && !writeIsPublic {
			scope = "reads and writes"
		} else if writeIsPublic {
			scope = "reads"
		}
		data["auth_warning"] = fmt.Sprintf("Endpoint /api/%s now requires a logged-in user (auth_model=%s) but this project has no login (no create_auth/create_oauth), so %s will 401 from the browser. Add an auth endpoint, use auth_model=public (with public_read for open reads), or public_write=true for a public-submission endpoint whose reads stay gated.", path, authModel, scope)
	}
	return tools.Ok(data), nil
}

// epEnableQuery opts the project into the read-only raw-SELECT endpoint at
// /api/<path> (default "_query"). It grants read of every non-system table, so
// it is never created implicitly - only here, on an explicit request. The
// runtime handler enforces SELECT-only / no-system-table and the row cap.
func epEnableQuery(ctx *tools.ToolContext, rawPath string, maxRows, rateLimit int, requiresAuth bool) (*tools.Result, error) {
	path, errRes := tools.ValidateAndNormalizeEndpointPath(rawPath)
	if errRes != nil {
		return errRes, nil
	}
	if path == "" {
		path = "_query"
	}
	// Mirror the schema's min/max clamp for direct (non-reflected) callers:
	// unset falls back to the default, over-cap clamps to the documented max
	// rather than silently resetting to the default.
	if maxRows <= 0 {
		maxRows = 200
	}
	if maxRows > 1000 {
		maxRows = 1000
	}
	if rateLimit <= 0 {
		rateLimit = defaultAPIRateLimit
	}
	if err := ClaimEndpoint(ctx, endpoints.KindQuery, path, nil); err != nil {
		return tools.FailErr(err), nil
	}
	_, err := ctx.DB.Exec(
		`INSERT INTO of_query_endpoints (path, max_rows, requires_auth, rate_limit)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   max_rows = excluded.max_rows,
		   requires_auth = excluded.requires_auth,
		   rate_limit = excluded.rate_limit`,
		path, maxRows, requiresAuth, rateLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("enabling query endpoint: %w", err)
	}
	data := map[string]any{
		"endpoint_path": path,
		"max_rows":      maxRows,
		"requires_auth": requiresAuth,
	}
	if !requiresAuth {
		data["warning"] = "This endpoint needs no login and reads EVERY table except the user/auth tables, payments and password columns, owner- and tenant-scoped tables included (it applies no row scoping). Re-run enable_query with requires_auth=true unless every readable table is meant to be public."
	}
	data["hint"] = fmt.Sprintf("Read-only SQL endpoint ready. Frontend: POST /api/%s with {\"sql\":\"SELECT ...\"} -> {columns:[...], rows:[[...]], row_count, execution_ms}; an invalid or non-SELECT query returns 400 {error}. SELECT only (no writes), system of_ tables blocked, max %d rows/query. Use this for the free-form query editor and any join/aggregation; use view-backed GET endpoints or CRUD for everything else. Turn it off with manage_api action=delete endpoint_path=%s.", path, maxRows, path)
	return tools.Ok(data), nil
}

func epListAPI(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		`SELECT id, path, table_name, methods, public_columns, requires_auth, public_read,
		        required_role, rate_limit, cors_origins, cors_methods, cors_headers,
		        owner_column, COALESCE(owner_scope,''), tenant_column, lookup_column, cache_ttl, created_at,
		        COALESCE(public_write,0), COALESCE(writable_columns,''), COALESCE(read_only,0)
		   FROM of_api_endpoints
		   ORDER BY path`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing API endpoints: %w", err)
	}
	defer rows.Close()

	// Non-nil so an empty project serialises as [] ("confirmed none"), never a
	// bare null that a weaker model reads as "broken / unknown" and reacts to by
	// re-creating things that already exist.
	endpointList := []map[string]any{}
	for rows.Next() {
		var id, rateLimit, cacheTTL int
		var path, tableName, methods, corsOrigins, corsMethods, corsHeaders, ownerScope string
		var publicCols, requiredRole, ownerCol, tenantCol, lookupCol sql.NullString
		var requiresAuth, publicRead, publicWrite, readOnly bool
		var writableCols string
		var createdAt time.Time
		if err := rows.Scan(&id, &path, &tableName, &methods, &publicCols, &requiresAuth, &publicRead,
			&requiredRole, &rateLimit, &corsOrigins, &corsMethods, &corsHeaders,
			&ownerCol, &ownerScope, &tenantCol, &lookupCol, &cacheTTL, &createdAt,
			&publicWrite, &writableCols, &readOnly); err != nil {
			continue
		}
		ep := map[string]any{
			"id":             id,
			"endpoint_path":  path,
			"table_name":     tableName,
			"methods":        methods,
			"public_columns": publicCols.String,
			"requires_auth":  requiresAuth,
			"auth_model":     endpoints.AuthModel(requiresAuth, publicRead, requiredRole.String, ownerCol.String, ownerScope),
			"public_read":    publicRead,
			"public_write":   publicWrite,
			"required_role":  requiredRole.String,
			"rate_limit":     rateLimit,
			"cors_origins":   corsOrigins,
			"cors_methods":   corsMethods,
			"cors_headers":   corsHeaders,
			"owner_column":   ownerCol.String,
			"tenant_column":  tenantCol.String,
			"lookup_column":  lookupCol.String,
			"cache_ttl":      cacheTTL,
			"created_at":     createdAt,
		}
		// Only the collaborative endpoints carry a scope; omit it elsewhere to keep
		// the listing lean rather than tagging every row with an empty field.
		if ownerScope != "" {
			ep["owner_scope"] = ownerScope
		}
		if writableCols != "" {
			ep["writable_columns"] = writableCols
		}
		if readOnly {
			ep["read_only"] = true
		}
		endpointList = append(endpointList, ep)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing API endpoints: %w", err)
	}

	data := map[string]any{
		"endpoints": endpointList,
		"count":     len(endpointList),
	}
	// LLM chat/completion endpoints live in a separate table (of_llm_endpoints)
	// and are managed by a separate tool, but the project_state snapshot lists
	// them alongside REST endpoints with an [llm] tag. Without this pointer an
	// empty REST list flatly contradicts the snapshot, which is exactly what
	// sends the model into a re-create-then-"already exists" loop. Surface the
	// count and the right tool so the two views reconcile.
	var llmCount int
	if err := ctx.DB.QueryRow(`SELECT COUNT(*) FROM of_llm_endpoints`).Scan(&llmCount); err == nil && llmCount > 0 {
		data["llm_endpoints_note"] = fmt.Sprintf(
			"%d LLM endpoint(s) exist and are NOT shown here (they live in a separate table); list them with manage_llm_proxy action=list.",
			llmCount,
		)
	}
	return tools.Ok(data), nil
}

func epDeleteAPI(ctx *tools.ToolContext, endpointPath string) (*tools.Result, error) {
	// enable_query records its endpoint in its own table, so a plain CRUD delete of it
	// found nothing and the free-form SELECT endpoint could never be turned off.
	var crud int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_api_endpoints WHERE path = ?", tools.NormalizeEndpointPath(endpointPath)).Scan(&crud)
	if crud == 0 {
		if res, err := DeleteEndpointByPath(ctx, "of_query_endpoints", "endpoint", "manage_api", "delete", endpointPath); err == nil && res != nil && res.Success {
			return res, nil
		}
	}
	return DeleteEndpointByPath(ctx, "of_api_endpoints", "endpoint", "manage_api", "delete", endpointPath)
}

// rlsScopeWarning returns a non-empty nudge when an endpoint exposes a table
// that has an owner/tenant-like column but is left unscoped (no owner_column,
// no tenant_column) - so every caller can read every row. Returns "" when the
// endpoint is already scoped or the table has no owner-like column.
//
// The owner-like decision is store.OwnerLikeColumn, the same heuristic the
// build-end rls_scope validator (engine/validator_rls.go) consumes, so this
// save-time advisory and that hard gate can never drift. The advisory is
// best-effort here (the table may not exist yet at create time); the build gate
// is the strict backstop.
func rlsScopeWarning(columns []string, ownerColumn, tenantColumn string, publicRead bool) string {
	if ownerColumn != "" || tenantColumn != "" {
		return ""
	}
	hit := store.OwnerLikeColumn(columns)
	if hit == "" {
		return ""
	}
	exposure := "every authenticated user can read and write every row"
	if publicRead {
		exposure = "anyone can read every row without authenticating"
	}
	// A container reference alongside the owner-like column marks likely-shared
	// collaborative content: lead with authored (shared reads, owner-scoped writes)
	// so the advice does not steer a chat/board table into per-user owner scoping,
	// which would hide every other member's rows.
	if container := store.ContainerColumn(columns); container != "" {
		return fmt.Sprintf("table has author column %q grouped by %q but the endpoint sets no owner_column/tenant_column, so %s. These rows look SHARED within %q; to let members read all rows but edit only their own, use `manage_api action=update auth_model=authored owner_column=%s` (auth_model must ride the SAME call). If rows are PRIVATE per user, use auth_model=owner instead; for hard isolation between %q values, manage_tenancy action=add_tenant_column.", hit, container, exposure, container, hit, container)
	}
	return fmt.Sprintf("table has column %q but the endpoint sets no owner_column/tenant_column, so %s. If rows are per-user, scope access with `manage_api action=update auth_model=owner owner_column=%s` (auth_model must ride the SAME call - owner_column alone is ignored). For tenant-wide isolation use manage_tenancy action=add_tenant_column instead; manage_api has no tenant_column arg.", hit, exposure, hit)
}

// ownerLikeColumnForTable returns the table's first owner/tenant-like column
// (store.OwnerLikeColumn over the live schema) or "" on none / introspection
// failure. Reads columns through store.TableColumnNames so the create-time
// auto-scope guard in epCreateAPI and the advisory here agree on which column
// is owner-like (and on definition order, which decides the "first" pick).
func ownerLikeColumnForTable(ctx *tools.ToolContext, tableName string) string {
	return store.OwnerLikeColumn(store.TableColumnNames(ctx.DB, tableName))
}

// rlsScopeWarningForTable reads the table's columns and applies rlsScopeWarning.
// Best-effort: a column-introspection failure yields no warning rather than a
// spurious one (store.TableColumnNames returns nil on failure).
func rlsScopeWarningForTable(ctx *tools.ToolContext, tableName, ownerColumn, tenantColumn string, publicRead bool) string {
	if ownerColumn != "" || tenantColumn != "" {
		return ""
	}
	return rlsScopeWarning(store.TableColumnNames(ctx.DB, tableName), ownerColumn, tenantColumn, publicRead)
}
