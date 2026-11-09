package observability

// The active end-to-end probes (test_endpoint, test_url, test_auth_flow,
// test_page): HTTP requests back at this project's public server, registered
// by manage_testing (see manage_testing.go). Split from manage_diagnostics.go,
// which keeps the read-only inspection actions.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"openfabrica/analysis"
	"openfabrica/db"
	"openfabrica/internal/dbh"
	"openfabrica/internal/flowsnap"
	"openfabrica/store"
	"openfabrica/tools"
)

// endpointProbeTimeout is how long an endpoint probe (test_endpoint / test_url) waits
// for a response before giving up - ONE generous bound for every endpoint kind, not a
// per-kind budget. http.Client.Timeout is a ceiling, not a fixed wait, so a fast
// endpoint still returns immediately; a slow one (an LLM completion, a heavy query, an
// external call) gets the room it needs, and a genuinely stuck handler stays bounded.
// It sits under the public server's 120s request WriteTimeout - past that the server
// cuts the response anyway, so waiting longer proves nothing. Kind-agnostic by design:
// the probe reacts to whether the endpoint responds, never to what kind it is.
const endpointProbeTimeout = 60 * time.Second

// isTimeoutErr reports whether err is a client-side timeout (the probe gave up
// awaiting the response), as opposed to a connection refusal or other failure.
func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// timedOutProbeResult renders the de-misleading failure for an endpoint whose response
// the probe stopped waiting for - a slow endpoint is not the same as a broken one.
func timedOutProbeResult(waited time.Duration) *tools.Result {
	return tools.Fail(
		"request timed out after %s - the probe's wait limit, NOT proof the endpoint is broken. "+
			"Some endpoints do real work per request (an LLM completion, a heavy query, an external call) and can run longer than the probe waits yet still return a valid response. "+
			"Confirm it responds within the server's 120s request limit; if it genuinely needs longer, move the slow work to a background job so the endpoint returns immediately.",
		waited.Round(time.Second))
}

// expectedStatusOK reports whether an observed HTTP status satisfies the
// caller's expectation. expected == 0 means "unset": accept any 2xx, so a
// create that correctly returns 201 (or a 204) is not false-failed against an
// implicit 200 - the exact mismatch that read a healthy create endpoint as
// broken at the end of a build. A non-zero expected keeps an exact-match
// assertion for callers that deliberately pin a specific code.
func expectedStatusOK(status, expected int) bool {
	if expected == 0 {
		return status >= 200 && status < 300
	}
	return status == expected
}

// expectedStatusLabel renders the expectation for the result payload: the
// literal code when pinned, or "2xx" when left unset.
func expectedStatusLabel(expected int) any {
	if expected == 0 {
		return "2xx"
	}
	return expected
}

// expectedStatusError builds the failure message for a status mismatch,
// phrased to match whether the caller pinned a specific code or accepted 2xx.
func expectedStatusError(expected, status int) string {
	if expected == 0 {
		return fmt.Sprintf("expected a 2xx status, got %d", status)
	}
	return fmt.Sprintf("expected status %d, got %d", expected, status)
}

// DiagTestEndpoint probes a REST endpoint at /api/<path>. Auto-mints a JWT
// when the endpoint requires auth so callers don't have to chain
// test_auth_flow first.
type DiagTestEndpoint struct {
	EndpointPath   string         `arg:"endpoint_path,required" desc:"REST endpoint path WITHOUT the /api/ prefix (e.g. 'posts')."`
	Method         string         `arg:"method"                 desc:"HTTP method. CRUD endpoints answer GET/POST/PUT/DELETE; PATCH is only for a manage_function endpoint that declares it." enum:"GET,POST,PUT,PATCH,DELETE" default:"GET"`
	RequestBody    map[string]any `arg:"request_body"           desc:"Request body for POST/PUT/PATCH requests."`
	ExpectedStatus int            `arg:"expected_status"        desc:"Expected HTTP status code; omit to accept any 2xx (a create returns 201, not 200). For a deliberate negative test pass the intended status (e.g. 404) so it records as a pass."`
	ExpectedFields []string       `arg:"expected_fields"        desc:"Fields that must exist in the JSON response."`
	AuthToken      string         `arg:"auth_token"             desc:"USUALLY UNNECESSARY - the probe authenticates itself against an auth-gated endpoint (it mints a token for an existing user and retries once if a request 401s), so leave this empty. Pass a token ONLY to probe as a specific user or role."`
}

func (a *DiagTestEndpoint) ActionName() string { return "test_endpoint" }
func (a *DiagTestEndpoint) Doc() string {
	return "Probe a REST endpoint: checks the HTTP status and that the JSON response is an object carrying expected_fields (a 200 with the wrong shape is a contract break a page probe cannot see). It authenticates itself on an auth-gated path. A create tool's returned contract is already verified, so an endpoint you just created needs no probe."
}
func (a *DiagTestEndpoint) ReadOnly() bool { return true }
func (a *DiagTestEndpoint) Example() any {
	return &DiagTestEndpoint{EndpointPath: "posts", Method: "GET"}
}

// serverErrorCause returns the reason a sandboxed endpoint answered with a 5xx,
// or "" when the status is not a server error or nothing was recorded for that
// path. A function endpoint answers with a generic body on purpose, so the probe
// sees a 500 and no cause; the thrown message is recorded as the request fails
// and this is what reads it back. Without it, narrowing a 500 means rewriting the
// handler and probing again until the failure moves.
func serverErrorCause(h dbh.Handle, endpointPath string, status int) string {
	if status < 500 {
		return ""
	}
	re, found := db.LastRuntimeError(h, db.RuntimeErrorFunction, endpointPath)
	if !found {
		return ""
	}
	return re.Message
}

func (a *DiagTestEndpoint) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	switch strings.ToUpper(a.Method) {
	case "", "GET", "HEAD", "OPTIONS":
		return a.run(ctx)
	}
	// A write probe leaves its row behind: a "Test / Hack" order on the live board, a
	// probe account in the users table. Run it between a snapshot and its restore, the way
	// flow replays do. Where no snapshot can be taken (no opener here, or real visitors are
	// on the site) it runs as before.
	var res *tools.Result
	var err error
	ran := false
	_, restoreErr := flowsnap.Around(ctx.Context(), func() {
		ran = true
		res, err = a.run(ctx)
	})
	if !ran {
		return a.run(ctx)
	}
	if res != nil {
		if data, ok := res.Data.(map[string]any); ok {
			if restoreErr != nil {
				data["rollback_error"] = "the data this probe wrote could not be fully rolled back: " + restoreErr.Error()
			} else {
				data["rolled_back"] = "the probe's write was rolled back so test rows do not ship; to keep data, insert it with manage_data"
			}
		}
	}
	return res, err
}

func (a *DiagTestEndpoint) run(ctx *tools.ToolContext) (*tools.Result, error) {
	domain, port, err := testResolveDomain(ctx)
	if err != nil {
		return tools.FailErr(err), nil
	}
	method := strings.ToUpper(a.Method)
	if method == "" {
		method = "GET"
	}
	// Auth token: a caller-supplied token wins; "auto"/"mint" mean "mint one for me".
	// An auth-gated CRUD endpoint with no token pre-mints here (skips a wasted 401
	// round-trip); every other auth kind heals on the 401 below.
	token := a.AuthToken
	if token == "auto" || token == "mint" {
		token = ""
	}
	autoAuth := false
	// A caller probing the auth gate itself (expecting 401 or 403) must arrive without a
	// token, or the gate can never be seen to close.
	if token == "" && a.ExpectedStatus != 401 && a.ExpectedStatus != 403 {
		if minted, ok := mintTestTokenIfNeeded(ctx, a.EndpointPath, method); ok {
			token = minted
			autoAuth = true
		}
	}
	urlPath := "/api/" + a.EndpointPath
	status, respBody, err := testMakeRequest(domain, port, method, urlPath, a.RequestBody, token, endpointProbeTimeout)
	if err != nil {
		if isTimeoutErr(err) {
			return timedOutProbeResult(endpointProbeTimeout), nil
		}
		return tools.Fail("request failed: %v", err), nil
	}
	// Auth self-heal: an UNEXPECTED 401 means the request lacked a valid token - a
	// caller passed a stale/wrong-project one, or this endpoint's kind (function, llm,
	// upload, ...) is not one the pre-mint recognizes. Mint a fresh token from an
	// existing user and retry ONCE. Kind-agnostic: it reacts to the 401, so it covers
	// every auth endpoint without enumerating kinds. Skipped when the caller EXPECTED a
	// 401 (deliberately probing the auth gate), or when we already minted (a mint that
	// still 401s is a real problem, not a stale token).
	if status == 401 && !autoAuth && a.ExpectedStatus != 401 && a.ExpectedStatus != 403 {
		if minted, ok := mintTestTokenFromAnyUser(ctx); ok {
			token, autoAuth = minted, true
			if s2, b2, e2 := testMakeRequest(domain, port, method, urlPath, a.RequestBody, token, endpointProbeTimeout); e2 == nil {
				status, respBody = s2, b2
			}
		}
	}
	result := map[string]any{
		"url": urlPath, "method": method,
		"status": status, "expected_status": expectedStatusLabel(a.ExpectedStatus),
		"response_size": len(respBody),
	}
	if autoAuth {
		result["auto_auth"] = true
	}
	if !expectedStatusOK(status, a.ExpectedStatus) {
		result["response_preview"] = string(respBody[:min(len(respBody), 500)])
		probeErr := expectedStatusError(a.ExpectedStatus, status)
		if cause := serverErrorCause(ctx.DB, a.EndpointPath, status); cause != "" {
			result["server_error"] = cause
			probeErr = fmt.Sprintf("%s. The handler failed with: %s", probeErr, cause)
		}
		return &tools.Result{Success: false, Error: probeErr, Data: result}, nil
	}
	if len(a.ExpectedFields) > 0 {
		var parsed map[string]any
		if json.Unmarshal(respBody, &parsed) != nil {
			// Expected fields promise an object. A bare list, a scalar, or a non-JSON body
			// carries none of them - the silent shape break this check exists to catch.
			result["response_preview"] = string(respBody[:min(len(respBody), 500)])
			return &tools.Result{Success: false, Error: fmt.Sprintf("expected a JSON object carrying %s, but the response is not a JSON object (a bare list or non-JSON body)", strings.Join(a.ExpectedFields, ", ")), Data: result}, nil
		}
		var missing []string
		for _, f := range a.ExpectedFields {
			if _, exists := parsed[f]; !exists {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			result["missing_fields"] = missing
			return &tools.Result{Success: false, Error: fmt.Sprintf("missing fields: %s", strings.Join(missing, ", ")), Data: result}, nil
		}
	}
	result["passed"] = true
	return tools.Ok(result), nil
}

// DiagTestURL probes a verbatim URL path (no /api/ prefix). Use it for the page
// root, PWA files (/manifest.json), the service worker, and other dynamic
// non-/api paths. You do NOT need to probe a static file under /files/: a saved
// kind=file artifact is ALWAYS served at /files/<path>, so this action answers a
// /files/ path straight from the saved-file table (no HTTP) - a 404 there only
// means the path doesn't match a saved file (usually a typo), never a serving
// failure. Confirm a page references its assets correctly with test_page.
type DiagTestURL struct {
	URLPath        string         `arg:"url_path,required" desc:"Verbatim URL path (e.g. '/', '/manifest.json'). Static files under /files/ don't need probing - they always serve when saved."`
	Method         string         `arg:"method"            desc:"HTTP method." enum:"GET,POST,PUT,PATCH,DELETE" default:"GET"`
	RequestBody    map[string]any `arg:"request_body"      desc:"Request body for POST/PUT/PATCH."`
	ExpectedStatus int            `arg:"expected_status"   desc:"Expected HTTP status code; omit to accept any 2xx. For a deliberate negative test pass the intended status (e.g. 404 for an unknown route) so it records as a pass."`
	AuthToken      string         `arg:"auth_token"        desc:"Bearer token."`
}

func (a *DiagTestURL) ActionName() string { return "test_url" }
func (a *DiagTestURL) Doc() string {
	return "Probe a verbatim URL path (page roots, /manifest.json, dynamic non-/api paths). /files/ assets serve whenever saved. A WebSocket endpoint answers a plain GET with 426 and reports healthy, which only proves the route is mounted."
}
func (a *DiagTestURL) ReadOnly() bool { return true }
func (a *DiagTestURL) Example() any {
	return &DiagTestURL{URLPath: "/manifest.json"}
}

// isWebSocketUpgradeStatus reports whether an HTTP status from a plain GET probe
// indicates a WebSocket endpoint rather than a failure. A WebSocket route cannot
// be exercised with a normal request: the server answers a non-upgrade GET with
// 426 Upgrade Required. That status is positive evidence the endpoint is mounted
// and correctly demands a protocol upgrade, so a probe must read it as a pass,
// not a broken endpoint.
func isWebSocketUpgradeStatus(status int) bool {
	return status == 426
}
func (a *DiagTestURL) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	switch strings.ToUpper(a.Method) {
	case "", "GET", "HEAD", "OPTIONS":
		return a.run(ctx)
	}
	// A write probe leaves its row behind, exactly like a test_endpoint write: run it
	// between a snapshot and its restore.
	var res *tools.Result
	var err error
	ran := false
	_, restoreErr := flowsnap.Around(ctx.Context(), func() {
		ran = true
		res, err = a.run(ctx)
	})
	if !ran {
		return a.run(ctx)
	}
	if res != nil {
		if data, ok := res.Data.(map[string]any); ok {
			if restoreErr != nil {
				data["rollback_error"] = "the data this probe wrote could not be fully rolled back: " + restoreErr.Error()
			} else {
				data["rolled_back"] = "the probe's write was rolled back so test rows do not ship"
			}
		}
	}
	return res, err
}

func (a *DiagTestURL) run(ctx *tools.ToolContext) (*tools.Result, error) {
	// A cross-origin absolute URL is not a project-local path. The model reaches
	// here when it hand-probes a CDN library URL (the QR-lib flail); prepending "/"
	// below would just 404 on the app's own server as "/https://cdn..." with no hint
	// that a library must be registered. Route to add_library instead of probing or
	// hotlinking the CDN - the platform auto-injects a declared library.
	if analysis.CrossOriginURL(a.URLPath) {
		return tools.Fail(
			"%q is an external URL, not a project-local path - test_url probes paths served by your own app (e.g. '/', '/manifest.json', a dynamic non-/api route). Do NOT hotlink or probe a third-party library: declare it with `manage_libraries action=add_library` (library={name:\"<lib>\", url:%q}) and the platform auto-injects the <script>/<link> for the browser to load - you never fetch the CDN yourself.",
			a.URLPath, strings.TrimSpace(a.URLPath)), nil
	}
	urlPath := a.URLPath
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	// Static files under /files/ are served by a pure DB lookup (ServeFile):
	// a saved kind=file row is ALWAYS served at /files/<path>. So an HTTP probe
	// adds nothing - answer from the same table ServeFile reads, with a message
	// that names the real issue (a path/name that matches no saved file) instead
	// of a bare "404", and that steers off probing files at all.
	// Only the plain "does it serve" question short-circuits: a deliberate
	// expected_status (a negative test) or a non-GET still goes over HTTP.
	if rel := strings.TrimPrefix(urlPath, "/files/"); rel != urlPath && rel != "" && a.ExpectedStatus == 0 && (a.Method == "" || strings.EqualFold(a.Method, "GET")) {
		return diagCheckStaticFile(ctx, urlPath, rel), nil
	}
	domain, port, err := testResolveDomain(ctx)
	if err != nil {
		return tools.FailErr(err), nil
	}
	method := strings.ToUpper(a.Method)
	if method == "" {
		method = "GET"
	}
	// test_url can target a slow endpoint (an /api/<name> that does real work); give it
	// the same generous wait as test_endpoint so a slow response is not misread as broken.
	status, respBody, err := testMakeRequest(domain, port, method, urlPath, a.RequestBody, a.AuthToken, endpointProbeTimeout)
	if err != nil {
		if isTimeoutErr(err) {
			return timedOutProbeResult(endpointProbeTimeout), nil
		}
		return tools.Fail("request failed: %v", err), nil
	}
	result := map[string]any{
		"url": urlPath, "method": method,
		"status": status, "expected_status": expectedStatusLabel(a.ExpectedStatus),
		"response_size": len(respBody),
	}
	// A WebSocket endpoint answers a plain GET with 426 Upgrade Required, which
	// confirms it is mounted and correctly rejects a non-upgrade request - a pass,
	// not a failure. Without this, probing a WebSocket path reads as a broken
	// endpoint (and, when a probe outcome gates task completion, blocks it) for an
	// endpoint that is working as intended. test_url cannot drive the handshake, so
	// steer message-flow verification to client-side code on a page.
	// Only when no other status was asked for: a caller expecting 404 on the route must
	// not have a 426 pass for it.
	if isWebSocketUpgradeStatus(status) && (a.ExpectedStatus == 0 || a.ExpectedStatus == 426) {
		result["passed"] = true
		result["served"] = "websocket"
		result["note"] = "this path is a WebSocket endpoint: a 426 Upgrade Required confirms it is mounted and correctly rejects a plain HTTP request, but proves NOTHING about whether the relay actually works. Run manage_testing action=test_websocket endpoint_path=<path> to complete a real handshake and read the welcome frame (add two_peer=true to verify the room_gated join-token flow); that is the check that catches a relay that upgrades and then goes silent."
		return tools.Ok(result), nil
	}
	if !expectedStatusOK(status, a.ExpectedStatus) {
		preview := respBody
		if len(preview) > 500 {
			preview = preview[:500]
		}
		result["response_preview"] = string(preview)
		return &tools.Result{Success: false, Error: expectedStatusError(a.ExpectedStatus, status), Data: result}, nil
	}
	result["passed"] = true
	return tools.Ok(result), nil
}

// diagCheckStaticFile answers a /files/<rel> probe from the saved-file table
// instead of issuing an HTTP request. ServeFile resolves the same path the same
// way (SELECT ... WHERE kind='file' AND path = rel), so this is the identical
// pass/fail with no network cost. On a miss it names the real cause - a path
// that matches no saved file - and lists same-extension saved files so a typo
// (style.css vs styles.css) is obvious, rather than returning a bare 404.
func diagCheckStaticFile(ctx *tools.ToolContext, urlPath, rel string) *tools.Result {
	var n int
	_ = ctx.DB.QueryRow(
		`SELECT COUNT(*) FROM of_files WHERE kind='file' AND path = ? AND is_deleted = 0`, rel,
	).Scan(&n)
	if n > 0 {
		return tools.Ok(map[string]any{
			"url":    urlPath,
			"status": 200,
			"passed": true,
			"served": "static-file",
			"note":   "Static files are served automatically at /files/<path> whenever the artifact exists - so this one serves, and you don't need to probe files at all. To confirm a page actually loads its assets, use test_page.",
		})
	}
	data := map[string]any{"url": urlPath, "status": 404, "expected_status": 200}
	msg := fmt.Sprintf("no file artifact is saved at %q, so %s 404s. A saved file always serves at its path, so this is a wrong path/name - not a serving failure; you don't need to probe files.", rel, urlPath)
	if near := diagSameExtFiles(ctx, rel); len(near) > 0 {
		data["saved_files_with_same_extension"] = near
		msg += " Saved files with the same extension: " + strings.Join(near, ", ") + "."
	}
	return &tools.Result{Success: false, Error: msg, Data: data}
}

// diagSameExtFiles returns up to 8 saved kind=file paths sharing rel's extension,
// so a near-miss probe can point at the name the model probably meant.
func diagSameExtFiles(ctx *tools.ToolContext, rel string) []string {
	ext := filepath.Ext(rel)
	if ext == "" {
		return nil
	}
	rows, err := ctx.DB.Query(
		`SELECT path FROM of_files WHERE kind='file' AND is_deleted = 0 AND path LIKE ? ORDER BY path LIMIT 8`,
		"%"+ext,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	_ = rows.Err() // best-effort near-miss hints
	return out
}

// DiagTestAuthFlow exercises register -> login -> /me on an auth endpoint with
// a freshly minted test user. Walks every step so a partial failure surfaces
// the specific stage that broke.
type DiagTestAuthFlow struct {
	EndpointPath string `arg:"endpoint_path" desc:"Auth endpoint path." default:"auth"`
}

func (a *DiagTestAuthFlow) ActionName() string { return "test_auth_flow" }
func (a *DiagTestAuthFlow) Doc() string {
	return "Exercise the register/login flow of an auth endpoint."
}

// test_auth_flow is deliberately NOT ReadOnly: it registers a probe user in the auth
// table, a real write the build loop must count as one.
func (a *DiagTestAuthFlow) Example() any {
	return &DiagTestAuthFlow{EndpointPath: "auth"}
}
func (a *DiagTestAuthFlow) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	path := a.EndpointPath
	if path == "" {
		path = "auth"
	}
	domain, port, err := testResolveDomain(ctx)
	if err != nil {
		return tools.FailErr(err), nil
	}
	var tableName, usernameCol, passwordCol, roleCol string
	_ = ctx.DB.QueryRow("SELECT table_name, username_column, password_column, COALESCE(role_column, 'role') FROM of_auth_endpoints WHERE path = ?", path).Scan(&tableName, &usernameCol, &passwordCol, &roleCol)
	if usernameCol == "" {
		usernameCol = "email"
	}
	if passwordCol == "" {
		passwordCol = "password"
	}
	if roleCol == "" {
		roleCol = "role"
	}
	// Discover every required (NOT NULL, no-default) column so the synthetic
	// register body fills them all, not just the login key + password. A users
	// table commonly carries BOTH username and email NOT NULL while auth logs in
	// by only one of them; omitting the other 400s register on a NOT NULL
	// constraint - a false-positive probe failure for an auth flow that real
	// signup (which sends every field) uses fine. physTable == table_name
	// (server/public/auth.go), so this name feeds the PRAGMA helpers directly.
	var required map[string]bool
	var colInfo []store.ColumnInfo
	if tableName != "" {
		required, _ = store.RequiredColumns(ctx.DB, tableName)
		colInfo, _ = store.GetTableColumnInfo(ctx.DB, tableName)
	}
	suffix := time.Now().UnixNano() % 100000
	testUser := buildProbeUser(usernameCol, passwordCol, roleCol, required, colInfo, suffix)
	steps := []map[string]any{}

	regStatus, regBody, regErr := testMakeRequest(domain, port, "POST", "/api/"+path+"/register", testUser, "", defaultProbeTimeout)
	regStep := map[string]any{"step": "register", "status": regStatus}
	if regErr != nil {
		regStep["error"] = regErr.Error()
		steps = append(steps, regStep)
		return &tools.Result{Success: false, Error: "register failed: " + regErr.Error(), Data: steps}, nil
	}
	if regStatus != 200 && regStatus != 201 {
		regStep["response"] = string(regBody[:min(len(regBody), 300)])
		steps = append(steps, regStep)
		return &tools.Result{Success: false, Error: fmt.Sprintf("register returned %d", regStatus), Data: steps}, nil
	}
	regStep["passed"] = true
	steps = append(steps, regStep)

	loginStatus, loginBody, loginErr := testMakeRequest(domain, port, "POST", "/api/"+path+"/login", testUser, "", defaultProbeTimeout)
	loginStep := map[string]any{"step": "login", "status": loginStatus}
	if loginErr != nil {
		loginStep["error"] = loginErr.Error()
		steps = append(steps, loginStep)
		return &tools.Result{Success: false, Error: "login failed: " + loginErr.Error(), Data: steps}, nil
	}
	if loginStatus != 200 {
		loginStep["response"] = string(loginBody[:min(len(loginBody), 300)])
		steps = append(steps, loginStep)
		return &tools.Result{Success: false, Error: fmt.Sprintf("login returned %d", loginStatus), Data: steps}, nil
	}
	var loginResp map[string]any
	_ = json.Unmarshal(loginBody, &loginResp)
	token, _ := loginResp["token"].(string)
	if token == "" {
		loginStep["response"] = string(loginBody[:min(len(loginBody), 300)])
		steps = append(steps, loginStep)
		return &tools.Result{Success: false, Error: "login response missing 'token' field", Data: steps}, nil
	}
	loginStep["passed"] = true
	steps = append(steps, loginStep)

	meStatus, meBody, meErr := testMakeRequest(domain, port, "GET", "/api/"+path+"/me", nil, token, defaultProbeTimeout)
	meStep := map[string]any{"step": "me", "status": meStatus}
	if meErr != nil {
		meStep["error"] = meErr.Error()
		steps = append(steps, meStep)
		return &tools.Result{Success: false, Error: "/me failed: " + meErr.Error(), Data: steps}, nil
	}
	if meStatus != 200 {
		meStep["response"] = string(meBody[:min(len(meBody), 300)])
		steps = append(steps, meStep)
		return &tools.Result{Success: false, Error: fmt.Sprintf("/me returned %d", meStatus), Data: steps}, nil
	}
	// /me returning 200 is not enough: the platform's requireRole helper gates on
	// user.role from this body, so a /me that omits the role column makes every
	// role-gated page bounce the logged-in user home - a 200-status bug that
	// status-only checks miss. Assert the role column is actually surfaced.
	var meResp map[string]any
	_ = json.Unmarshal(meBody, &meResp)
	if _, hasRole := meResp[roleCol]; !hasRole {
		meStep["response"] = string(meBody[:min(len(meBody), 300)])
		meStep["missing_role_column"] = roleCol
		steps = append(steps, meStep)
		return &tools.Result{Success: false, Error: fmt.Sprintf(
			"/me succeeded (200) but its body omits the %q column - requireRole (from the 'auth' helper) reads user.%s, so every role-gated page will redirect the logged-in user home. /me always includes the endpoint's role column, so %q is not a column the user table has: point role_column at the real one (manage_auth update role_column=...).",
			roleCol, roleCol, roleCol), Data: steps}, nil
	}
	meStep["passed"] = true
	meStep["role"] = meResp[roleCol]
	steps = append(steps, meStep)

	return tools.Ok(map[string]any{"steps": steps, "token": token}), nil
}

// isEmailLikeColumn reports whether an auth login column expects an email address
// (so the probe should send an email-shaped value) rather than a plain handle.
// Keeps the synthetic register value in step with the column so a username column
// with a no-'@' rule is not tripped by an email-shaped probe value.
func isEmailLikeColumn(col string) bool {
	return strings.Contains(strings.ToLower(col), "mail") // matches email, e_mail, user_email, ...
}

// buildProbeUser assembles the synthetic register/login body for the auth-flow
// probe. It fills the login key, the password, AND every OTHER required column of
// the table, so a users table with a second NOT NULL column (the common
// username+email shape, where auth logs in by only one) does not 400 register on a
// NOT NULL constraint - a false-positive for an auth flow that real signup uses
// fine. Skips: the login key (already set), the password column (the server
// overwrites it with the hash from the plaintext), the role column (the server
// force-sets it to the default), and the auto id. `required`/`cols` may be empty
// (schema read failed) - the body then degrades to the bare login-key + password
// pair rather than erroring.
func buildProbeUser(usernameCol, passwordCol, roleCol string, required map[string]bool, cols []store.ColumnInfo, suffix int64) map[string]any {
	typeByCol := make(map[string]string, len(cols))
	for _, c := range cols {
		typeByCol[c.Name] = strings.ToUpper(c.Type)
	}
	// The synthetic login value must match the login column's shape. An `email`
	// column wants an email, but a plain `username`/`handle` column may carry a
	// CHECK constraint or app-side rule that rejects '@'/'.'. So derive the value
	// from the column: email-shaped only for an email column.
	loginValue := fmt.Sprintf("probe_user_%d", suffix)
	if isEmailLikeColumn(usernameCol) {
		loginValue = fmt.Sprintf("test_%d@test.local", suffix)
	}
	body := map[string]any{
		usernameCol: loginValue,
		// register/login read the plaintext under the literal "password" key
		// regardless of the column's name, then hash it into passwordCol.
		"password": "TestPass123!",
	}
	skip := map[string]bool{usernameCol: true, passwordCol: true, roleCol: true, "id": true}
	for col := range required {
		if skip[col] {
			continue
		}
		if _, set := body[col]; set {
			continue
		}
		body[col] = probeValueFor(col, typeByCol[col], suffix)
	}
	return body
}

// probeValueFor synthesizes a unique value for one extra required column, shaped
// to the column: an email-like name gets an email, a numeric affinity gets the
// numeric suffix (so a NOT NULL INTEGER does not get a string), everything else a
// plain handle. The suffix keeps values unique per run so repeated probes do not
// collide on a UNIQUE column.
func probeValueFor(col, sqlType string, suffix int64) any {
	if isEmailLikeColumn(col) {
		return fmt.Sprintf("test_%d_%s@test.local", suffix, col)
	}
	switch {
	case strings.Contains(sqlType, "INT"), strings.Contains(sqlType, "REAL"),
		strings.Contains(sqlType, "NUMERIC"), strings.Contains(sqlType, "FLOA"),
		strings.Contains(sqlType, "DOUB"):
		return suffix
	default:
		return fmt.Sprintf("probe_%s_%d", col, suffix)
	}
}

// DiagTestPage serves a page and then loads it in a real browser to verify it
// works: a 200 with a non-empty body, no uncaught error, and no failed
// same-origin script/stylesheet on load. It also measures how much visible
// content the page painted (visible_chars / media_count) and surfaces it for the
// agent to judge, but does NOT auto-fail a page that painted little: a "blank"
// verdict is a timing-coupled heuristic that false-positives a correct cold-load
// page and traps the agent in an in-session fix loop. Falls back to the
// status-only check when no browser is available.
type DiagTestPage struct {
	PagePath       string   `arg:"page_path"       desc:"Page path (e.g. '/about')." default:"/"`
	PagePaths      []string `arg:"page_paths"      desc:"Several page paths probed in one call (at most 8); replaces page_path and answers per page under pages."`
	ExpectedStatus int      `arg:"expected_status" desc:"Expected HTTP status code; omit to accept any 2xx."`
}

func (a *DiagTestPage) ActionName() string { return "test_page" }
func (a *DiagTestPage) Doc() string {
	return "Render a page (or several via page_paths) in a real browser, signed in as a seeded user when the project has a login: checks the expected status, no uncaught error, and that its scripts and styles load. Reports visible_chars/media_count and console_errors for you to judge; only an uncaught throw or a failed same-origin script or stylesheet hard-fails."
}
func (a *DiagTestPage) ReadOnly() bool { return true }
func (a *DiagTestPage) Example() any {
	return &DiagTestPage{PagePath: "/about"}
}

// maxProbePages bounds one multi-page test_page call: each page is a real browser render.
const maxProbePages = 8

func (a *DiagTestPage) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var paths []string
	seen := map[string]bool{}
	for _, p := range a.PagePaths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return a.probe(ctx, a.PagePath)
	}
	if len(paths) > maxProbePages {
		return tools.Fail("page_paths lists %d pages; probe at most %d per call", len(paths), maxProbePages), nil
	}
	pages := make([]map[string]any, 0, len(paths))
	var failed []string
	for _, p := range paths {
		res, err := a.probe(ctx, p)
		if err != nil {
			return nil, err
		}
		entry := map[string]any{"page_path": p}
		if data, ok := res.Data.(map[string]any); ok {
			for k, v := range data {
				entry[k] = v
			}
		}
		entry["passed"] = res.Success
		if !res.Success {
			entry["error"] = res.Error
			failed = append(failed, p)
		}
		pages = append(pages, entry)
	}
	out := map[string]any{"pages": pages, "passed": len(failed) == 0}
	if len(failed) > 0 {
		return &tools.Result{Success: false, Error: fmt.Sprintf("%d of %d pages failed: %s", len(failed), len(paths), strings.Join(failed, ", ")), Data: out}, nil
	}
	return tools.Ok(out), nil
}

func (a *DiagTestPage) probe(ctx *tools.ToolContext, path string) (*tools.Result, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	domain, port, err := testResolveDomain(ctx)
	if err != nil {
		return tools.FailErr(err), nil
	}
	status, body, err := testMakeRequest(domain, port, "GET", path, nil, "", defaultProbeTimeout)
	if err != nil {
		return tools.Fail("page request failed: %v", err), nil
	}
	result := map[string]any{
		"page_path": path, "status": status,
		"expected_status": expectedStatusLabel(a.ExpectedStatus), "response_size": len(body),
	}
	if !expectedStatusOK(status, a.ExpectedStatus) {
		result["response_preview"] = string(body[:min(len(body), 500)])
		return &tools.Result{Success: false, Error: expectedStatusError(a.ExpectedStatus, status), Data: result}, nil
	}
	if len(body) < 50 {
		return &tools.Result{Success: false, Error: "page response too short (< 50 bytes)", Data: result}, nil
	}

	// A 200 only proves the shell served. Load the page in a real browser to
	// confirm it executed without throwing and actually painted visible content -
	// catching pages that return 200 but render nothing (content hidden by an
	// unstyled / opacity:0 shell, a script that threw, or a missing asset), which
	// a status check can never see. A blank paint is surfaced as a "warning" below
	// (advisory, not a hard fail).
	// Load it the way the end-of-build gate does: signed in as a seeded user when the
	// project has a login surface, so a login-gated page runs its real data path instead
	// of bouncing to /login and passing on the redirect.
	renderToken := ""
	if projectHasAuthSurface(ctx) {
		if minted, ok := mintTestTokenFromAnyUser(ctx); ok {
			renderToken = minted
			result["signed_in"] = true
		}
	}
	render, rerr := verifyPageRender(ctx, domain, port, path, renderToken)
	if rerr != nil {
		// No browser, or an infra error driving it: degrade to the status-only
		// result rather than fail a healthy page on a browser-less host or a flake.
		if errors.Is(rerr, store.ErrBrowserUnavailable) {
			result["render_check"] = "skipped: no browser available to verify rendering"
		} else {
			result["render_check"] = "skipped: " + rerr.Error()
		}
		result["passed"] = true
		return tools.Ok(result), nil
	}
	result["visible_chars"] = render.visibleChars
	result["media_count"] = render.mediaCount
	// For a canvas-app page (a game, a 3D world, a WebGL tool) visible_chars is a
	// false health signal - the page legitimately paints almost no text. Surface the
	// canvas + render-loop facts instead: raf_frames is the honest "did the loop run"
	// signal (see stuckBootWarning). Only when a canvas is actually present, to avoid
	// noise on ordinary text pages.
	if render.canvasPresent {
		result["canvas_present"] = true
		result["canvas_dominant"] = render.canvasDominant
		result["canvas_sized"] = render.canvasSized
		result["raf_frames"] = render.rafFrames
	}
	if render.hiddenChars > 0 {
		result["hidden_chars"] = render.hiddenChars
	}
	if len(render.exceptions) > 0 {
		result["exceptions"] = render.exceptions
	}
	if render.firstPrjFrame != "" {
		result["first_project_frame"] = render.firstPrjFrame
	}
	if len(render.failedAssets) > 0 {
		result["failed_assets"] = render.failedAssets
	}
	if len(render.failedURLs) > 0 {
		result["failed_asset_urls"] = render.failedURLs
	}
	if len(render.bootFailures) > 0 {
		result["boot_failures"] = render.bootFailures
	}
	// Surface any console.error/assert the page logged so the agent can judge them.
	// The platform does NOT decide whether a console error is fatal - that is the
	// model's call: a library misuse error (e.g. "THREE.Object3D.add: ... not an
	// instance of") that does not throw still leaves a feature silently broken,
	// while some console errors are genuinely benign. Surfacing is the platform's
	// job; judgement is the model's.
	if len(render.consoleErrors) > 0 {
		result["console_errors"] = render.consoleErrors
	}
	if reason, failed := render.failureReason(); failed {
		// Even on a hard failure, keep the console errors visible - they often
		// explain the failure (or a second, separate defect).
		return &tools.Result{Success: false, Error: reason, Data: result}, nil
	}
	result["passed"] = true
	if len(render.consoleErrors) > 0 {
		result["note"] = "the page rendered, but it logged the console error(s) listed in console_errors. Decide whether they indicate broken behaviour (a library-misuse error such as \"not an instance of\" / \"is not a function\" usually means a feature is silently dead) - if so, fix the cause and re-run test_page before completing. If you judge them harmless, you may proceed."
	}
	// A 2xx page that threw nothing but painted nothing a user can see is the exact
	// failure this probe exists to catch (a stuck opacity:0 reveal, a render that
	// never ran, an unstyled empty state). It is not a hard fail - blank verdicts
	// are timing-coupled and would trap the agent in a fix loop - but it must not
	// pass silently, so surface it as an advisory warning the agent must judge.
	if warn := render.blankRenderWarning(); warn != "" {
		result["warning"] = warn
	}
	// A 2xx page that painted visible "NaN"/"undefined"/stringified-object text threw
	// nothing and is not blank, so neither hard-fail nor blankRenderWarning catches it -
	// yet it is the fingerprint of a math/data bug (and a NaN in a transform also blacks
	// a WebGL canvas silently). Surface it as an advisory the agent must judge; keep any
	// blank warning already set.
	if junk := render.junkTextWarning(); junk != "" {
		result["visible_glitches"] = render.junkText
		if existing, _ := result["warning"].(string); existing != "" {
			result["warning"] = existing + " " + junk
		} else {
			result["warning"] = junk
		}
	}
	// A canvas-app page that mounted a full-viewport <canvas> but never ran a render
	// frame passed every check above (it served 200, threw nothing, and painted its
	// loading overlay so it is not blank) - yet the game never started. visible_chars
	// cannot see this; raf_frames can. Advisory, not a hard fail (a one-shot static
	// canvas legitimately runs no loop), but it must not pass silently.
	if stuck := render.stuckBootWarning(); stuck != "" {
		if existing, _ := result["warning"].(string); existing != "" {
			result["warning"] = existing + " " + stuck
		} else {
			result["warning"] = stuck
		}
	}
	return tools.Ok(result), nil
}
