package observability

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"openfabrica/internal/flowsnap"
	"openfabrica/security"
	"openfabrica/store"
	"openfabrica/tools"
)

// Shared infrastructure for the observability tools. The test_* actions on
// manage_diagnostics all dispatch HTTP requests back at the public server,
// optionally minting a JWT for auth-protected endpoints; the integrity
// action walks artifacts to surface form-sink mismatches. Everything in this
// file is local to the package - exported names are reserved for future
// external callers.

// testResolveDomain returns the project's configured vhost and the public
// port the server listens on. Tests target 127.0.0.1:port with Host set to
// the vhost so the dispatcher routes the request to this project.
func testResolveDomain(ctx *tools.ToolContext) (string, int, error) {
	port := ctx.PublicPortOrDefault()
	if ctx.GlobalDB == nil {
		return "", 0, fmt.Errorf("no global database, cannot resolve the project's domain to test")
	}
	var domain sql.NullString
	_ = ctx.GlobalDB.QueryRow("SELECT domain FROM projects WHERE id = ?", ctx.ProjectID).Scan(&domain)
	if !domain.Valid || domain.String == "" {
		return "", 0, fmt.Errorf("no domain configured for project, cannot test")
	}
	return domain.String, port, nil
}

// defaultProbeTimeout is the HTTP-client wait a test_* probe uses for an ordinary
// endpoint (CRUD, auth, URL). A function endpoint overrides it with its own
// wall-clock budget (see DiagTestEndpoint.Run) so a slow, valid handler is not
// reported as broken.
const defaultProbeTimeout = 10 * time.Second

// probeBodyCap bounds the response body a probe reads. It is large because callers
// parse the body (expected_fields, the auth flow's token): a cap below a valid
// response would cut the JSON mid-object and read as "not a JSON object". Previews
// shown to the model are sliced separately.
const probeBodyCap = 1 << 20

// testMakeRequest issues one HTTP call to the public server on this project's
// vhost, waiting at most `timeout` for the response. Limits the response body to
// 8 KiB so the LLM never sees a runaway payload.
func testMakeRequest(domain string, port int, method, urlPath string, body any, token string, timeout time.Duration) (int, []byte, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, urlPath)
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshaling body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	req.Host = domain
	req.Header.Set("Content-Type", "application/json")
	// The platform's own probe is not a visitor: the marker keeps its page views out of
	// the real-traffic check that decides whether a data snapshot may be taken.
	req.Header.Set("User-Agent", "OpenFabrica-Probe "+flowsnap.FlowBrowserMarker)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, probeBodyCap))
	if readErr != nil {
		// A stream that stalls or drops mid-body is a failed probe, not a short success.
		return resp.StatusCode, respBody, fmt.Errorf("reading response body: %w", readErr)
	}
	return resp.StatusCode, respBody, nil
}

// mintTestTokenIfNeeded mints a JWT for an existing user when the targeted
// endpoint requires authentication or has an owner_column. Lets test_endpoint
// behave like an authenticated browser without forcing test_auth_flow first.
//
// Returns ("", false) when no token is needed or anything goes wrong; the
// caller falls back to an unauthenticated request and surfaces what the
// server returns.
func mintTestTokenIfNeeded(ctx *tools.ToolContext, path, method string) (string, bool) {
	if ctx == nil || ctx.JWTManager == nil || ctx.DB == nil {
		return "", false
	}
	var requiresAuth, publicRead, publicWrite bool
	var ownerColumn, tableName string
	err := ctx.DB.QueryRow(
		`SELECT COALESCE(requires_auth,0), COALESCE(public_read,0), COALESCE(public_write,0),
		        COALESCE(owner_column,''), COALESCE(table_name,'')
		 FROM of_api_endpoints WHERE path = ?`,
		path,
	).Scan(&requiresAuth, &publicRead, &publicWrite, &ownerColumn, &tableName)
	if err != nil {
		return "", false
	}
	needsToken := requiresAuth || ownerColumn != ""
	if !needsToken {
		return "", false
	}
	if strings.ToUpper(method) == "GET" && publicRead && ownerColumn == "" {
		return "", false
	}
	// public_write: writes are open, so probe the anonymous write path (no token) -
	// that is exactly the surface public_write exists to make work.
	if strings.ToUpper(method) != "GET" && publicWrite && ownerColumn == "" {
		return "", false
	}

	return mintTestTokenFromAnyUser(ctx)
}

// mintTestTokenFromAnyUser generates a JWT for the first existing user in the auth
// table (role falling back to "admin"), for probing an auth-gated endpoint of ANY
// kind. Returns ("", false) when there is no user to mint from or the JWT manager is
// unavailable. Kind-agnostic: the caller decides WHEN a token is needed (a 401 the
// probe just saw, or a CRUD requires_auth flag); this only mints one.
func mintTestTokenFromAnyUser(ctx *tools.ToolContext) (string, bool) {
	if ctx == nil || ctx.JWTManager == nil || ctx.DB == nil {
		return "", false
	}
	var usersTable string
	_ = ctx.DB.QueryRow(`SELECT COALESCE(table_name,'') FROM of_auth_endpoints LIMIT 1`).Scan(&usersTable)
	if usersTable == "" {
		usersTable = "users"
	}
	var userID int
	var username, role sql.NullString
	q := fmt.Sprintf(
		`SELECT id, COALESCE(username, email, ''), COALESCE(role, '') FROM %s ORDER BY id LIMIT 1`,
		usersTable,
	)
	if err := ctx.DB.QueryRow(q).Scan(&userID, &username, &role); err != nil {
		if err2 := ctx.DB.QueryRow(fmt.Sprintf(`SELECT id FROM %s ORDER BY id LIMIT 1`, usersTable)).Scan(&userID); err2 != nil {
			return "", false
		}
	}
	uname := username.String
	if uname == "" {
		uname = "test_runner"
	}
	rname := role.String
	if rname == "" {
		rname = "admin"
	}
	tok, err := ctx.JWTManager.Generate(security.TokenRequest{ProjectID: ctx.ProjectID, UserID: userID, Username: uname, Role: rname})
	if err != nil {
		return "", false
	}
	return tok, true
}

// Patterns scanned by formSinkMismatches: an HTML <input>/<textarea>/<select>
// name= attribute on the page, and a fetch('/api/X', {method:'POST'}) pair
// inside the page or its linked JS assets. Two patterns to catch either
// ordering of `method:` relative to the fetch URL.
var (
	formInputNameRe      = regexp.MustCompile(`(?is)<(?:input|textarea|select)\b[^>]*\bname\s*=\s*["']([^"']+)["']`)
	apiPostRe            = regexp.MustCompile(`(?is)fetch\s*\(\s*["'\x60]/api/([A-Za-z0-9_\-]+)["'\x60][^)]*\)\s*[\s\S]{0,400}?method\s*:\s*["']POST["']`)
	apiPostMethodFirstRe = regexp.MustCompile(`(?is)method\s*:\s*["']POST["'][\s\S]{0,400}?fetch\s*\(\s*["'\x60]/api/([A-Za-z0-9_\-]+)["'\x60]`)
)

// formSinkMismatches scans a page (and its linked page-scope JS assets) for
// forms that POST to an /api/<table> path whose column set barely overlaps
// the form's input names. Such a mismatch silently inserts bogus rows into
// the target table when only a single field (e.g. `name`) overlaps between
// the form and the table.
func formSinkMismatches(ctx *tools.ToolContext, pagePath, body string) []string {
	if !strings.Contains(strings.ToLower(body), "<form") {
		return nil
	}
	fields := extractMatches(formInputNameRe, body)
	if len(fields) == 0 {
		return nil
	}
	combined := body
	if assets, err := store.PageAssets(ctx.DB, pagePath); err == nil {
		for _, a := range assets {
			if !strings.HasSuffix(strings.ToLower(a), ".js") {
				continue
			}
			var assetBody string
			_ = ctx.DB.QueryRow(
				"SELECT body FROM of_files WHERE path = ? AND kind = 'file' AND is_deleted = 0 LIMIT 1",
				a,
			).Scan(&assetBody)
			if assetBody != "" {
				combined += "\n" + assetBody
			}
		}
	}
	targets := append(extractMatches(apiPostRe, combined), extractMatches(apiPostMethodFirstRe, combined)...)
	if len(targets) == 0 {
		return nil
	}
	var issues []string
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t] {
			continue
		}
		seen[t] = true
		physical, err := store.SanitizedTableName(t)
		if err != nil {
			continue
		}
		cols, err := store.GetTableColumns(ctx.DB, physical)
		if err != nil || len(cols) == 0 {
			continue
		}
		var overlap, considered int
		for _, f := range fields {
			lf := strings.ToLower(f)
			if lf == "id" || lf == "created_at" {
				continue
			}
			considered++
			if cols[lf] {
				overlap++
			}
		}
		if considered == 0 {
			continue
		}
		if overlap*2 < considered {
			issues = append(issues, fmt.Sprintf(
				"Page %q POSTs to /api/%s but only %d of %d form fields match that table's columns. The form is likely wired to the wrong endpoint, add a dedicated table + endpoint for these submissions, or use manage_email.",
				pagePath, t, overlap, considered,
			))
		}
	}
	return issues
}

func extractMatches(re *regexp.Regexp, s string) []string {
	matches := re.FindAllStringSubmatch(s, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) > 1 {
			out = append(out, m[1])
		}
	}
	return out
}

// projectHasAuthSurface reports whether the project has a login endpoint, so a probe
// knows signing in as a seeded user is meaningful.
func projectHasAuthSurface(ctx *tools.ToolContext) bool {
	if ctx == nil || ctx.DB == nil {
		return false
	}
	var n int
	_ = ctx.DB.QueryRow(`SELECT COUNT(*) FROM of_auth_endpoints`).Scan(&n)
	return n > 0
}
