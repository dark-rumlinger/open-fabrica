package public

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/funcexec"
	"openfabrica/internal/funcsandbox"
	"openfabrica/server/middleware"
)

// Custom function endpoints (manage_function).
//
// A row in of_function_endpoints mounts a sandboxed, model-authored handler(req, ctx)
// at /api/<path>. This file owns the HTTP concerns (routing, CORS, auth, rate limit,
// request/response marshalling); the sandbox execution and its guards live in
// internal/funcexec, shared with the trigger runner so the two paths can't drift.

const maxFunctionBodyBytes = 256 * 1024 // request body read cap

type functionEndpoint struct {
	Path         string
	Code         string
	Methods      string // comma-separated, uppercased at save time
	RequiresAuth bool
	CORSOrigins  string
	RateLimit    int
	TimeoutMS    int
	Secrets      string // JSON array of allowed secret names
	Description  string // one line shown to a hosted agent
}

// loadFunctionEndpoint returns the config for a function endpoint, or an error
// when no function is mounted at path.
func (h *Handler) loadFunctionEndpoint(reader *sql.DB, path string) (*functionEndpoint, error) {
	var ep functionEndpoint
	err := reader.QueryRow(
		"SELECT path, code, methods, COALESCE(requires_auth,0), COALESCE(cors_origins,''), COALESCE(rate_limit,60), COALESCE(timeout_ms,0), COALESCE(secrets,''), COALESCE(description,'') FROM of_function_endpoints WHERE path = ?",
		path,
	).Scan(&ep.Path, &ep.Code, &ep.Methods, &ep.RequiresAuth, &ep.CORSOrigins, &ep.RateLimit, &ep.TimeoutMS, &ep.Secrets, &ep.Description)
	if err != nil {
		return nil, err
	}
	if ep.Methods == "" {
		ep.Methods = "POST"
	}
	if ep.RateLimit <= 0 {
		ep.RateLimit = 60
	}
	return &ep, nil
}

// serveFunctionIfExists dispatches to a function endpoint that owns exactly this
// path, returning true when it handled the request. A miss returns false so the
// caller falls through to normal CRUD/sub-route dispatch.
//
// Method-aware co-location: a function owns the path only for the methods it declares.
// If it does not handle the request method but a REST/CRUD endpoint at the SAME path
// does, this returns false so dispatch falls through (e.g. a POST-only create function
// and a GET lookup endpoint sharing /api/files). Otherwise the function keeps the path
// and answers it (405 for an unhandled method).
func (h *Handler) serveFunctionIfExists(w http.ResponseWriter, r *http.Request, project *models.Project, projectDB *db.ProjectDB, path string) bool {
	ep, err := h.loadFunctionEndpoint(projectDB.Reader(), path)
	if err != nil {
		return false // not a function endpoint
	}
	if !functionMethodAllowed(ep.Methods, r.Method) {
		if crudEp, cerr := h.loadEndpoint(projectDB.Reader(), path); cerr == nil && methodAllowed(crudEp.Methods, r.Method) {
			return false // let the co-located CRUD endpoint serve this method
		}
	}
	h.apiFunction(w, r, project, projectDB, ep)
	return true
}

// apiFunction runs one function-endpoint request end to end.
func (h *Handler) apiFunction(w http.ResponseWriter, r *http.Request, project *models.Project, projectDB *db.ProjectDB, ep *functionEndpoint) {
	// Per-endpoint CORS on the actual response (mirrors the LLM endpoint): reflect
	// an allowed origin. The OPTIONS preflight is answered centrally upstream by
	// handleAPIPreflight, so this handler only ever runs for the real method.
	if ep.CORSOrigins != "" {
		applyEndpointCORS(w, ep.CORSOrigins, r.Header.Get("Origin"), ep.Methods+", OPTIONS", "Content-Type, Authorization")
	}

	if !functionMethodAllowed(ep.Methods, r.Method) {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Auth gate + best-effort identity for req.user (one call returns both the
	// gate result and, for a user JWT, the claims).
	var user map[string]any
	if ep.RequiresAuth {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		ok, claims := h.validateProjectTokenOrJWTWithClaims(project.ID, token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if claims != nil && claims.UserID > 0 {
			user = map[string]any{"id": claims.UserID, "role": claims.Role}
		}
	}

	// Rate limit by IP (per-minute window, same limiter the LLM/query endpoints use).
	rateLimitKey := fmt.Sprintf("fn:%d:%s:%s", project.ID, ep.Path, middleware.RateLimitIdentity(r))
	if !checkEndpointRateLimit(rateLimitKey, ep.RateLimit) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	reqObj := h.buildFunctionRequest(r, user)

	spec := funcexec.Spec{Code: ep.Code, SecretsJSON: ep.Secrets}
	if ep.TimeoutMS > 0 {
		spec.Timeout = time.Duration(ep.TimeoutMS) * time.Millisecond
	}
	rt := funcexec.NewRuntime(project.ID, projectDB.Reader(), projectDB.Writer(), h.deps.completeDeps())
	resp, err := funcexec.Execute(rt, spec, reqObj)
	if err != nil {
		var to *funcsandbox.Timeout
		if errors.As(err, &to) {
			writeError(w, http.StatusGatewayTimeout, "function timed out")
			return
		}
		// A thrown handler exception or a refused write - report a generic 500
		// (never the message, which could carry internal detail) and log the cause.
		// The message is also recorded against this endpoint so the build's own
		// probe can report WHY the call failed: over HTTP it sees only the generic
		// body, which cannot distinguish a bad argument from a refused write.
		slog.Warn("function endpoint errored", "project_id", project.ID, "path", ep.Path, "error", err)
		db.RecordRuntimeError(projectDB.ToolHandle(r.Context()), db.RuntimeErrorFunction, ep.Path, err.Error())
		writeError(w, http.StatusInternalServerError, "function error")
		return
	}

	writeFunctionResponse(w, resp)
}

// buildFunctionRequest assembles the req object: method, path, query, a
// sensitive-header-stripped header map, the parsed body, and the authed user.
func (h *Handler) buildFunctionRequest(r *http.Request, user map[string]any) map[string]any {
	query := map[string]any{}
	for k, vs := range r.URL.Query() {
		if len(vs) == 1 {
			query[k] = vs[0]
		} else {
			arr := make([]any, len(vs))
			for i, v := range vs {
				arr[i] = v
			}
			query[k] = arr
		}
	}

	headers := map[string]any{}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "cookie" {
			continue // never expose the caller's credentials to handler JS
		}
		if len(vs) > 0 {
			headers[k] = vs[0]
		}
	}

	var body any
	raw, _ := io.ReadAll(io.LimitReader(r.Body, maxFunctionBodyBytes))
	if len(raw) > 0 {
		if json.Unmarshal(raw, &body) != nil {
			body = string(raw) // non-JSON payload passes through as a string
		}
	}

	return map[string]any{
		"method":  r.Method,
		"path":    r.URL.Path,
		"query":   query,
		"headers": headers,
		"body":    body,
		"user":    user, // nil -> JS null
	}
}

// writeFunctionResponse writes the handler's Response. Content-Type defaults to
// application/json but a handler-set Content-Type header wins (so a handler can
// return text/html or plain text via a string body).
func writeFunctionResponse(w http.ResponseWriter, resp *funcsandbox.Response) {
	status := resp.Status
	switch {
	case status <= 0:
		// No status set: default to 200.
		status = http.StatusOK
	case status < 100 || status > 599:
		// An out-of-range code (net/http panics outside [100, 999]) means the
		// handler is misbehaving, so surface it as a server error.
		status = http.StatusInternalServerError
	}
	ctSet := false
	for k, v := range resp.Headers {
		if disallowedResponseHeader(k) {
			continue
		}
		if strings.EqualFold(k, "Content-Type") {
			ctSet = true
		}
		w.Header().Set(k, v)
	}
	if !ctSet {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
}

// disallowedResponseHeader blocks handler-set headers that would let a function
// mint credentials or steer the transport on the project's own origin. A handler
// returns application content, not session or connection control, so Set-Cookie
// and the hop-by-hop headers are dropped rather than forwarded.
func disallowedResponseHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "set-cookie", "set-cookie2",
		"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// functionMethodAllowed reports whether method is in the endpoint's
// comma-separated methods list (case-insensitive).
func functionMethodAllowed(methods, method string) bool {
	method = strings.ToUpper(method)
	for _, m := range strings.Split(methods, ",") {
		if strings.ToUpper(strings.TrimSpace(m)) == method {
			return true
		}
	}
	return false
}
