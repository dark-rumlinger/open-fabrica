package public

import (
	"database/sql"
	"log/slog"
	"net/http"
	"strings"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/security"
	"openfabrica/store"
)

// buildAPIRequestContext authenticates the request and assembles the row-level
// security predicates (owner_column and tenant_column) for an endpoint call.
// Returns (rc, isProjectKey, ok); when ok is false a response has already been
// written and the caller must return immediately.
//
// This is the security-critical block of the dynamic API: every CRUD path reads
// rc.OwnerFilter / rc.TenantFilter to AND-append the predicate that keeps user A's or
// tenant A's rows invisible to a session bound to user B or tenant B. Project API keys
// bypass the predicate (owner-level access), and the platform_admin tenant role
// bypasses tenancy.
func (h *Handler) buildAPIRequestContext(w http.ResponseWriter, r *http.Request, project *models.Project, ep *apiEndpoint) (rc *apiRequestContext, isProjectKey bool, ok bool) {
	var authUserID int
	var authUserRole string
	var authTenantID int
	var authTenantRole string

	if ep.RequiresAuth && !ep.methodAuthExempt(r.Method) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if h.validateProjectToken(project.ID, token) {
			isProjectKey = true
		} else {
			if h.deps.JWTManager == nil {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return nil, false, false
			}
			claims, err := h.validateUserClaims(token)
			if err != nil || claims.ProjectID != project.ID {
				writeError(w, http.StatusUnauthorized, "invalid or expired token")
				return nil, false, false
			}
			// admin satisfies every role gate, as it bypasses owner scoping below.
			if ep.RequiredRole != "" && claims.Role != ep.RequiredRole && claims.Role != "admin" {
				writeError(w, http.StatusForbidden, "insufficient permissions")
				return nil, false, false
			}
			authUserID = claims.UserID
			authUserRole = claims.Role
			authTenantID = claims.TenantID
			authTenantRole = claims.TenantRole
		}
	} else if ep.OwnerColumn != "" || ep.TenantColumn != "" {
		// Owner- or tenant-scoped endpoints: extract claims from an optional
		// token for GET with public_read so reads still scope when a token
		// happens to be present.
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != "" && h.deps.JWTManager != nil {
			if claims, err := h.validateUserClaims(token); err == nil && claims.ProjectID == project.ID {
				authUserID = claims.UserID
				authUserRole = claims.Role
				authTenantID = claims.TenantID
				authTenantRole = claims.TenantRole
			}
		}
	}

	// Tenancy gate: a tenanted endpoint that requires auth must not accept a
	// user session without an active tenant. Project API keys bypass tenancy
	// the same way they bypass owner_column.
	if ep.TenantColumn != "" && ep.RequiresAuth && !isProjectKey && !(r.Method == http.MethodGet && ep.PublicRead) {
		if authTenantID == 0 && authTenantRole != "platform_admin" {
			writeError(w, http.StatusForbidden, "no active tenant; call /tenant/switch first")
			return nil, false, false
		}
	}

	// Defense in depth against a scoped-but-anonymous leak. The owner/tenant
	// filters below only apply once an identity is known (authUserID/authTenantID
	// > 0). An owner- or tenant-scoped endpoint that is public (requires_auth=
	// false) but NOT marked public_read would otherwise fall through with no
	// filter and return every owner's/tenant's rows to an anonymous caller.
	// public_read endpoints intentionally expose unscoped reads (e.g. a public
	// blog whose writes are owner-scoped), so they are exempt; project API keys
	// (owner-level access) and the admin / platform_admin roles keep their bypass.
	if !isProjectKey && !ep.PublicRead {
		if ep.OwnerColumn != "" && authUserRole != "admin" && authUserID == 0 {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return nil, false, false
		}
		if ep.TenantColumn != "" && authTenantRole != "platform_admin" && authTenantID == 0 {
			writeError(w, http.StatusForbidden, "no active tenant")
			return nil, false, false
		}
	}

	// Owner filter: admin role and project API keys bypass.
	//
	// authored owner_scope marks the row's author rather than gating who may read it:
	// the filter scopes writes (a caller updates and deletes only their own rows) and
	// the insert stamp still attributes the author, but authenticated reads are shared,
	// so a GET carries no owner predicate. Any other scope keeps the owner filter on
	// reads too (private-per-user rows). A tenant_column, when set, still scopes reads
	// to the caller's tenant, so authored composes with tenant isolation.
	var ownerFilter string
	var ownerFilterVal any
	sharedRead := ep.OwnerScope == store.OwnerScopeAuthored && r.Method == http.MethodGet
	if ep.OwnerColumn != "" && !isProjectKey && authUserRole != "admin" && authUserID > 0 && !sharedRead {
		clause, err := security.OwnerFilterClause(ep.OwnerColumn)
		if err != nil {
			slog.Error("invalid owner_column at request dispatch", "path", ep.Path, "owner_column", ep.OwnerColumn, "error", err)
			writeError(w, http.StatusInternalServerError, "endpoint misconfigured")
			return nil, false, false
		}
		ownerFilter = " AND " + clause
		ownerFilterVal = authUserID
	}

	// Tenant filter: stacks on top of the owner filter. platform_admin bypasses.
	var tenantFilter string
	var tenantFilterVal any
	if ep.TenantColumn != "" && !isProjectKey && authTenantRole != "platform_admin" && authTenantID > 0 {
		clause, err := security.TenantFilterClause(ep.TenantColumn)
		if err != nil {
			slog.Error("invalid tenant_column at request dispatch", "path", ep.Path, "tenant_column", ep.TenantColumn, "error", err)
			writeError(w, http.StatusInternalServerError, "endpoint misconfigured")
			return nil, false, false
		}
		tenantFilter = " AND " + clause
		tenantFilterVal = authTenantID
	}

	return &apiRequestContext{
		OwnerFilter:     ownerFilter,
		OwnerFilterVal:  ownerFilterVal,
		AuthUserID:      authUserID,
		TenantFilter:    tenantFilter,
		TenantFilterVal: tenantFilterVal,
		AuthTenantID:    authTenantID,
	}, isProjectKey, true
}

// applyCORSHeaders writes the per-endpoint CORS response headers for the actual
// (non-preflight) response. The OPTIONS preflight is answered earlier and
// centrally by handleAPIPreflight, so this is only ever reached for real methods.
// A no-op when the endpoint sets no per-endpoint policy - the coarse instance-wide
// headers the router applied then stand.
func applyCORSHeaders(w http.ResponseWriter, r *http.Request, ep *apiEndpoint) {
	if ep.CORSOrigins == "" {
		return
	}
	setEndpointCORSOrigin(w, ep.CORSOrigins, r.Header.Get("Origin"))
	allowMethods := ep.CORSMethods
	if allowMethods == "" {
		allowMethods = strings.Join(ep.Methods, ", ")
	}
	w.Header().Set("Access-Control-Allow-Methods", allowMethods)
	if ep.CORSHeaders != "" {
		w.Header().Set("Access-Control-Allow-Headers", ep.CORSHeaders)
	} else {
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	}
}

// endpointCORSSpec is a resolved per-endpoint CORS policy used to answer a
// preflight. origins is the endpoint's stored allowlist (comma-separated, or "*");
// an empty origins means the endpoint declares no per-endpoint narrowing, so the
// coarse instance-wide policy stands.
type endpointCORSSpec struct {
	origins string
	methods string // Access-Control-Allow-Methods value
	headers string // Access-Control-Allow-Headers value
}

// handleAPIPreflight answers a CORS preflight (OPTIONS) for a dynamic /api/*
// endpoint and reports whether it did (callers return immediately when true).
//
// The router's coarse CORS layer set instance-wide headers but, for /api/*,
// deliberately did NOT answer the preflight (CORSWithPreflightPassthrough) so this
// per-endpoint layer can narrow it. When the endpoint declares its own allowlist,
// setEndpointCORSOrigin drops the coarse Allow-Origin/Credentials so a disallowed
// origin reflects nothing (not the permissive "*") and a matched origin comes back
// with credentials; an endpoint with no policy keeps the coarse headers. Either way
// OPTIONS is answered 204 so a preflight never falls through to the 405 path.
func (h *Handler) handleAPIPreflight(w http.ResponseWriter, r *http.Request, projectDB *db.ProjectDB, fullPath, endpointPath, rowID string) bool {
	if r.Method != http.MethodOptions {
		return false
	}
	if spec, ok := h.lookupEndpointCORS(projectDB.Reader(), fullPath, endpointPath, rowID); ok && spec.origins != "" {
		applyEndpointCORS(w, spec.origins, r.Header.Get("Origin"), spec.methods, spec.headers)
	}
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
	return true
}

// lookupEndpointCORS resolves the per-endpoint CORS policy for a dynamic /api/*
// route without regard to HTTP method (a preflight arrives as OPTIONS, which no
// endpoint stores). It mirrors DynamicAPI's dispatch resolution so the preflight
// policy matches the endpoint that serves the real request: an LLM endpoint answers
// /api/<path>/{chat,complete}; a function owns the bare full path; a CRUD endpoint
// owns its path and /api/<path>/<id>. These are the only kinds carrying a per-endpoint
// allowlist - every other kind relies on the coarse policy, so a miss returns ok=false
// and the caller keeps the router's headers. The Allow-Methods/Headers returned match
// each kind's actual-response CORS block, so preflight and real response agree.
func (h *Handler) lookupEndpointCORS(reader *sql.DB, fullPath, endpointPath, rowID string) (endpointCORSSpec, bool) {
	// LLM: /api/<endpointPath>/{chat,complete}. Gated on the sub-route so a CRUD
	// row whose id happens to be "chat" does not probe the LLM table for a hit.
	if rowID == "chat" || rowID == "complete" {
		if ep, err := h.loadLLMEndpoint(reader, endpointPath); err == nil {
			return endpointCORSSpec{origins: ep.CORSOrigins, methods: "POST, OPTIONS", headers: "Content-Type, Authorization"}, true
		}
	}
	// Function: owns the bare full path.
	if ep, err := h.loadFunctionEndpoint(reader, fullPath); err == nil {
		return endpointCORSSpec{origins: ep.CORSOrigins, methods: ep.Methods + ", OPTIONS", headers: "Content-Type, Authorization"}, true
	}
	// CRUD: owns its path (bare list and single-row).
	if ep, err := h.loadEndpoint(reader, endpointPath); err == nil {
		methods := ep.CORSMethods
		if methods == "" {
			methods = strings.Join(ep.Methods, ", ")
		}
		headers := ep.CORSHeaders
		if headers == "" {
			headers = "Content-Type, Authorization"
		}
		return endpointCORSSpec{origins: ep.CORSOrigins, methods: methods, headers: headers}, true
	}
	return endpointCORSSpec{}, false
}
