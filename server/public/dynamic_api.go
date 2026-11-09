package public

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/endpoints"
	"openfabrica/server/middleware"
)

// Dynamic API Handler
//
// Files in this group:
//   - dynamic_api.go         entry point, sub-route dispatch, method switch
//   - dynamic_api_rls.go     buildAPIRequestContext, CORS helpers (security-critical)
//   - dynamic_api_crud.go    apiList / apiGetOne / apiInsert / apiUpdate / apiDelete
//   - dynamic_api_stats.go   apiStats aggregation handler
//   - dynamic_api_helpers.go loadEndpoint, validateProjectToken, scan/strip,
//                            parseFilterParam, parseFieldsParam, columnExists, ...

// apiRequestContext holds per-request context for row-level security.
type apiRequestContext struct {
	OwnerFilter    string // e.g. " AND user_id = ?" or ""
	OwnerFilterVal any    // the user ID value, nil if no filter
	AuthUserID     int    // authenticated user's ID (0 = none)
	// TenantFilter and TenantFilterVal mirror OwnerFilter for multi-tenant
	// isolation. When the endpoint has tenant_column set and the JWT carries
	// a TenantID, every CRUD query AND-appends " AND <tenant_column> = ?" so
	// rows in tenant B are invisible to a session bound to tenant A.
	TenantFilter    string
	TenantFilterVal any
	AuthTenantID    int // active tenant ID from the JWT (0 = none)
}

type apiEndpoint struct {
	ID            int
	Path          string
	TableName     string
	Methods       []string
	PublicColumns []string // nil means all non-secure
	// WritableColumns limits which body columns a client write may set; nil means
	// any column. Owner and tenant stamping is applied regardless.
	WritableColumns []string
	RequiresAuth    bool
	PublicRead      bool   // GET allowed without auth even when RequiresAuth=true
	PublicWrite     bool   // writes (POST/PUT/DELETE) allowed without auth even when RequiresAuth=true (public-submission shape; GET stays gated)
	RequiredRole    string // empty = any authenticated user, "admin" = admin only
	RateLimit       int
	CacheTTL        int               // response cache TTL in seconds (0 = no caching)
	SecureCols      map[string]string // column -> "hash" or "encrypt"
	CORSOrigins     string            // comma-separated allowed origins, "*" for any
	CORSMethods     string            // comma-separated allowed methods
	CORSHeaders     string            // comma-separated allowed headers
	OwnerColumn     string            // when set, queries are scoped to authenticated user's ID
	OwnerScope      string            // store.OwnerScopeAuthored = scope writes only (reads shared); "" = scope reads and writes
	TenantColumn    string            // when set, queries are scoped to claims.TenantID (stacks on top of OwnerColumn)
	LookupColumn    string            // when set, /api/<path>/<value> resolves against this column (else id/uuid/slug autodetect)
	ReadOnly        bool              // when set, the backing object is a view: GET only, writes rejected
}

// methodAuthExempt reports whether an HTTP method may proceed without a token,
// given the endpoint's per-method public flags. Reads (GET) are exempt when
// PublicRead; every other method (the writes) when PublicWrite. Symmetric across
// methods, so a public-submission endpoint can accept anonymous POSTs while
// keeping GET reads auth-gated. Only consulted when RequiresAuth is set.
func (ep *apiEndpoint) methodAuthExempt(method string) bool {
	if method == http.MethodGet {
		return ep.PublicRead
	}
	return ep.PublicWrite
}

func (h *Handler) DynamicAPI(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	projectDB := h.openProjectDB(w, project.ID)
	if projectDB == nil {
		return
	}

	// Extract the endpoint path: /api/{path...}
	fullPath := strings.TrimPrefix(r.URL.Path, "/api/")
	if fullPath == "" {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}

	// Serve OpenAPI spec JSON from of_files (kind=asset).
	if fullPath == "docs.json" && r.Method == http.MethodGet {
		h.serveDocsJSON(w, project, projectDB.Reader())
		return
	}

	// Split into endpoint path and optional sub-route (last segment).
	// e.g. "contacts"->("contacts",""), "contacts/5"->("contacts","5"),
	// "contacts/_stats"->("contacts","_stats"), "ws/lobby/ws"->("ws/lobby","ws").
	// Split at the LAST "/" so nested paths ("ws/lobby") keep their sub-route.
	var endpointPath, rowID string
	if idx := strings.LastIndex(fullPath, "/"); idx >= 0 {
		endpointPath = fullPath[:idx]
		rowID = fullPath[idx+1:]
	} else {
		endpointPath = fullPath
	}

	// CORS preflight. The router's coarse layer handed OPTIONS on /api/* down here
	// (middleware.CORSWithPreflightPassthrough) so a per-endpoint allowlist can
	// narrow it. Answer it now, before the method-gated spine dispatch would 404/405
	// on OPTIONS. Endpoints without a per-endpoint policy keep the coarse headers.
	if h.handleAPIPreflight(w, r, projectDB, fullPath, endpointPath, rowID) {
		return
	}

	// Resolve the owning kind from the of_endpoints spine and dispatch the
	// non-CRUD kinds (function, query, upload, sse, llm, ws, export, import).
	// The spine keys routes by (path, sub_segment, method), so co-located kinds
	// are separated by method here rather than by a hard-coded precedence ladder.
	// CRUD (bare + single-row), auth, and push are handled below.
	if h.dispatchViaSpine(w, r, project, projectDB, fullPath, endpointPath, rowID) {
		return
	}

	// Auth namespace: /api/<path>/{register,login,me,refresh,oauth,totp,magic,tenants,...}.
	if h.handleAuthRequest(w, r, project.ID, projectDB.Reader(), projectDB.Writer(), fullPath) {
		return
	}

	// Push subscribe: /api/<path>/push/subscribe.
	if strings.HasSuffix(endpointPath, "/push") && rowID == "subscribe" && r.Method == http.MethodPost {
		h.apiPushSubscribe(w, r, project.ID, projectDB, strings.TrimSuffix(endpointPath, "/push"))
		return
	}

	// CRUD table stats: /api/<path>/_stats (a CRUD feature, not a separate kind).
	if rowID == "_stats" && r.Method == http.MethodGet {
		h.apiStats(w, r, projectDB.Reader(), endpointPath)
		return
	}

	// Look up the endpoint config. The split at the last slash reads the last segment
	// as a row of the prefix, but an endpoint registered at the whole path
	// ("orders/new") owns that URL: it is tried first, whether or not the prefix is an
	// endpoint too. Read as row "new" of a role-gated "orders", a public submission
	// endpoint answered every anonymous POST with a 401 it never asked for.
	var ep *apiEndpoint
	var err error
	if rowID != "" {
		if whole, werr := h.loadEndpoint(projectDB.Reader(), fullPath); werr == nil {
			ep = whole
			endpointPath, rowID = fullPath, ""
		}
	}
	if ep == nil {
		ep, err = h.loadEndpoint(projectDB.Reader(), endpointPath)
	}
	if err != nil {
		// Nothing serves this (path, method). If some kind owns the route for a
		// DIFFERENT method, the path exists and only the method is wrong - say so
		// rather than 404, which reads as "create this endpoint" and sends the
		// caller into a collision against the endpoint that already owns it.
		if h.spinePathExists(projectDB.Reader(), fullPath, endpointPath, rowID) {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		// Drain a body-bearing request before the 404 so a POST/PUT to a route
		// that no longer resolves (e.g. an upload whose spine row was lost)
		// returns a clean 404 instead of resetting the connection mid-send.
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, int64(endpoints.MaxUploadSizeMB)*1024*1024+1024)
		}
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}

	// Per-endpoint CORS headers on the actual (non-preflight) response. OPTIONS was
	// already answered centrally by handleAPIPreflight above.
	applyCORSHeaders(w, r, ep)

	if !methodAllowed(ep.Methods, r.Method) {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Read-only (view-backed) endpoints serve GET only. Defense in depth: the
	// stored methods are already forced to GET at create time, but reject any
	// write here too so a stale or hand-edited row can never expose writes
	// against a view (which would fail at the SQLite layer anyway).
	if ep.ReadOnly && r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "endpoint is read-only")
		return
	}

	// Per-endpoint rate limit (requests/min per IP), configured via manage_api.
	// Applied uniformly to reads and writes; the OPTIONS preflight already
	// returned above so it is not counted. rate_limit=0 disables the cap.
	if !checkEndpointRateLimit("api:"+projectKeyOf(r)+":"+endpointPath+":"+middleware.RateLimitIdentity(r), ep.RateLimit) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	// Auth + row-level security context. On failure the helper has already
	// written the response body, so just return.
	rc, _, ok := h.buildAPIRequestContext(w, r, project, ep)
	if !ok {
		return
	}

	physTable := ep.TableName

	switch r.Method {
	case http.MethodGet:
		// Response caching for GET requests when cache_ttl is configured.
		if ep.CacheTTL > 0 && h.apiCache != nil {
			cacheKey := formatCacheKey(project.ID, ep.TableName, rc.OwnerFilterVal, rc.TenantFilterVal, r.URL.Path, r.URL.RawQuery)
			if body, status, ok := h.apiCache.get(cacheKey); ok {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(status)
				w.Write(body)
				return
			}
			rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
			if rowID != "" {
				h.apiGetOne(rec, r, projectDB.Reader(), physTable, rowID, ep, rc)
			} else {
				h.apiList(rec, r, projectDB.Reader(), physTable, ep, rc)
			}
			if rec.statusCode == http.StatusOK && len(rec.body) > 0 {
				h.apiCache.set(cacheKey, rec.body, rec.statusCode, time.Duration(ep.CacheTTL)*time.Second)
			}
			w.Header().Set("X-Cache", "MISS")
		} else {
			if rowID != "" {
				h.apiGetOne(w, r, projectDB.Reader(), physTable, rowID, ep, rc)
			} else {
				h.apiList(w, r, projectDB.Reader(), physTable, ep, rc)
			}
		}
	case http.MethodPost:
		h.apiInsert(w, r, projectDB.Writer(), physTable, ep, project.ID, endpointPath, rc)
		if h.apiCache != nil {
			h.apiCache.invalidateTable(project.ID, ep.TableName)
		}
	case http.MethodPut:
		if rowID == "" {
			writeError(w, http.StatusBadRequest, "ID required for PUT")
			return
		}
		h.apiUpdate(w, r, projectDB.Writer(), physTable, rowID, ep, project.ID, endpointPath, rc)
		if h.apiCache != nil {
			h.apiCache.invalidateTable(project.ID, ep.TableName)
		}
	case http.MethodDelete:
		if rowID == "" {
			writeError(w, http.StatusBadRequest, "ID required for DELETE")
			return
		}
		h.apiDelete(w, projectDB.Writer(), physTable, rowID, ep, project.ID, endpointPath, rc)
		if h.apiCache != nil {
			h.apiCache.invalidateTable(project.ID, ep.TableName)
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// resolveSpineKind returns the endpoint kind that owns (path, sub_segment) for
// the given method in the of_endpoints spine, or "" when none does. A stored
// method of '*' matches any request method (WebSocket upgrade, auth namespace).
func (h *Handler) resolveSpineKind(reader *sql.DB, path, subSegment, method string) string {
	var kind string
	err := reader.QueryRow(
		"SELECT kind FROM of_endpoints WHERE path = ? AND sub_segment = ? AND (method = ? OR method = '*') LIMIT 1",
		path, subSegment, strings.ToUpper(method),
	).Scan(&kind)
	if err != nil {
		return ""
	}
	return kind
}

// spinePathExists reports whether any kind owns a route at this URL for ANY
// method - the same (path, sub_segment) pairs resolveSpineKind tries, minus the
// method predicate. It separates "no such endpoint" (404) from "wrong method on
// an endpoint that exists" (405).
func (h *Handler) spinePathExists(reader *sql.DB, fullPath, endpointPath, rowID string) bool {
	var n int
	if rowID != "" {
		if err := reader.QueryRow(
			"SELECT COUNT(*) FROM of_endpoints WHERE path = ? AND sub_segment = ?",
			endpointPath, rowID,
		).Scan(&n); err == nil && n > 0 {
			return true
		}
	}
	if err := reader.QueryRow(
		"SELECT COUNT(*) FROM of_endpoints WHERE path = ? AND sub_segment = ''", fullPath,
	).Scan(&n); err == nil && n > 0 {
		return true
	}
	return false
}

// dispatchViaSpine resolves the request against the of_endpoints spine and
// serves the non-CRUD kinds. It returns true when it handled the request. CRUD
// (bare list/insert and single-row), auth, and push are intentionally NOT served
// here - the caller handles them after this returns false. A reserved sub-route
// (/api/<path>/<seg>) is tried first, then the bare-path owners (function,
// query); a CRUD bare owner returns false so the caller's CRUD block serves it.
func (h *Handler) dispatchViaSpine(w http.ResponseWriter, r *http.Request, project *models.Project, projectDB *db.ProjectDB, fullPath, endpointPath, rowID string) bool {
	reader := projectDB.Reader()
	if rowID != "" {
		switch h.resolveSpineKind(reader, endpointPath, rowID, r.Method) {
		case endpoints.KindUpload:
			h.apiUpload(w, r, project.ID, projectDB, endpointPath)
			return true
		case endpoints.KindSSE:
			h.apiStream(w, r, project.ID, reader)
			return true
		case endpoints.KindLLM:
			h.apiLLM(w, r, project, projectDB, endpointPath, rowID)
			return true
		case endpoints.KindAgent:
			if r.Method == http.MethodGet {
				h.apiAgentRunStatus(w, r, project, projectDB, endpointPath)
			} else {
				h.apiAgent(w, r, project, projectDB, endpointPath)
			}
			return true
		case endpoints.KindWS:
			if rowID == "rooms" {
				h.apiWSRooms(w, r, project.ID, projectDB, endpointPath)
			} else {
				h.apiWebSocket(w, r, project.ID, projectDB)
			}
			return true
		case endpoints.KindExport:
			h.apiCSVExport(w, r, project.ID, reader, endpointPath)
			return true
		case endpoints.KindImport:
			h.apiCSVImport(w, r, project.ID, projectDB, endpointPath)
			return true
		case endpoints.KindDocument:
			h.apiDocument(w, r, project, projectDB, endpointPath)
			return true
		}
	}
	switch h.resolveSpineKind(reader, fullPath, "", r.Method) {
	case endpoints.KindFunction:
		// serveFunctionIfExists loads the function and serves it (the method is
		// already known-good via the spine match).
		return h.serveFunctionIfExists(w, r, project, projectDB, fullPath)
	case endpoints.KindQuery:
		h.apiQuery(w, r, project, reader, fullPath)
		return true
	}
	return false
}
