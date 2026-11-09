package public

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/funcsandbox"
	"openfabrica/security"
)

// newFunctionTestDB spins up a real project DB (migrations applied, so
// of_function_endpoints/of_secrets/of_dynamic_tables all exist) and runs the
// extra seed statements.
func newFunctionTestDB(t *testing.T, stmts ...string) *db.ProjectDB {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	for _, s := range stmts {
		if _, err := pdb.Writer().Exec(s); err != nil {
			t.Fatalf("seed failed: %v\n%s", err, s)
		}
	}
	return pdb
}

// createFunction inserts a function endpoint with a rate limit high enough that
// no test trips the cap. Reach for createFunctionRateLimited to exercise the cap
// itself: the limiter reads rate_limit as a ceiling, so the boundary values are
// only visible to a test that sets them.
func createFunction(t *testing.T, pdb *db.ProjectDB, path, code, methods string, requiresAuth bool, secretsJSON string) {
	t.Helper()
	createFunctionRateLimited(t, pdb, path, code, methods, requiresAuth, secretsJSON, 1000)
}

func createFunctionRateLimited(t *testing.T, pdb *db.ProjectDB, path, code, methods string, requiresAuth bool, secretsJSON string, rateLimit int) {
	t.Helper()
	if _, err := pdb.Writer().Exec(
		`INSERT INTO of_function_endpoints (path, code, methods, requires_auth, rate_limit, secrets) VALUES (?,?,?,?,?,?)`,
		path, code, methods, requiresAuth, rateLimit, secretsJSON,
	); err != nil {
		t.Fatalf("insert function endpoint: %v", err)
	}
}

func callFunction(t *testing.T, h *Handler, pdb *db.ProjectDB, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/"+path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	if !h.serveFunctionIfExists(rec, req, &models.Project{ID: 1}, pdb, path) {
		t.Fatalf("serveFunctionIfExists returned false for %q (row not found?)", path)
	}
	return rec
}

func TestFunctionComputesResponse(t *testing.T) {
	pdb := newFunctionTestDB(t,
		`CREATE TABLE items (id INTEGER PRIMARY KEY, price INTEGER)`,
		`INSERT INTO items (id, price) VALUES (1, 30), (2, 5)`)
	createFunction(t, pdb, "quote", `function handler(req, ctx) {
	  const rows = ctx.query('SELECT price FROM items WHERE id = ?', [req.body.id]);
	  if (!rows.length) return { status: 404, body: { error: 'not found' } };
	  return { total: rows[0].price * (req.body.qty || 1) };
	}`, "POST", false, "")

	h := &Handler{deps: &Deps{}}
	rec := callFunction(t, h, pdb, "POST", "quote", `{"id":1,"qty":3}`, "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["total"].(float64) != 90 {
		t.Fatalf("total = %v, want 90 (30*3)", out["total"])
	}

	// A missing id returns the handler's own 404 envelope.
	rec404 := callFunction(t, h, pdb, "POST", "quote", `{"id":999}`, "")
	if rec404.Code != 404 {
		t.Fatalf("missing-id status = %d, want 404 (%s)", rec404.Code, rec404.Body.String())
	}
}

func TestFunctionDeclaredWriteLands(t *testing.T) {
	pdb := newFunctionTestDB(t,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, total INTEGER, note TEXT)`)
	createFunction(t, pdb, "order", `function handler(req, ctx) {
	  ctx.insert('orders', { total: req.body.total, note: 'via-fn' });
	  return { ok: true };
	}`, "POST", false, "")

	h := &Handler{deps: &Deps{}}
	rec := callFunction(t, h, pdb, "POST", "order", `{"total":42}`, "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var total int
	var note string
	if err := pdb.Reader().QueryRow("SELECT total, note FROM orders ORDER BY id DESC LIMIT 1").Scan(&total, &note); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if total != 42 || note != "via-fn" {
		t.Fatalf("declared write did not land: total=%d note=%q", total, note)
	}
}

func TestFunctionSecureColumnWriteRejected(t *testing.T) {
	pdb := newFunctionTestDB(t,
		`CREATE TABLE accounts (id INTEGER PRIMARY KEY, password TEXT, balance INTEGER)`,
		`INSERT INTO accounts (id, password, balance) VALUES (7, 'hashed', 100)`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('accounts', '{}', '{"password":"hash"}')`)
	createFunction(t, pdb, "pwn", `function handler(req, ctx) {
	  ctx.update('accounts', { password: 'attacker' }, { id: 7 });
	  return { ok: true };
	}`, "POST", false, "")

	h := &Handler{deps: &Deps{}}
	rec := callFunction(t, h, pdb, "POST", "pwn", `{}`, "")
	if rec.Code != 500 {
		t.Fatalf("secure-column write status = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	var pw string
	if err := pdb.Reader().QueryRow("SELECT password FROM accounts WHERE id = 7").Scan(&pw); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if pw != "hashed" {
		t.Fatalf("password = %q, want unchanged 'hashed' (secure-column write must be refused)", pw)
	}
}

func TestFunctionSystemTableQueryRejected(t *testing.T) {
	pdb := newFunctionTestDB(t)
	createFunction(t, pdb, "peek", `function handler(req, ctx) {
	  const rows = ctx.query('SELECT value_encrypted FROM of_secrets', []);
	  return { rows: rows };
	}`, "POST", false, "")

	h := &Handler{deps: &Deps{}}
	rec := callFunction(t, h, pdb, "POST", "peek", `{}`, "")
	if rec.Code != 500 {
		t.Fatalf("system-table read status = %d, want 500 (ctx.query must refuse of_* tables) (%s)", rec.Code, rec.Body.String())
	}
}

func TestFunctionMethodNotAllowed(t *testing.T) {
	pdb := newFunctionTestDB(t)
	createFunction(t, pdb, "post-only", `function handler(req, ctx) { return { ok: true }; }`, "POST", false, "")
	h := &Handler{deps: &Deps{}}
	rec := callFunction(t, h, pdb, "GET", "post-only", "", "")
	if rec.Code != 405 {
		t.Fatalf("GET on POST-only status = %d, want 405", rec.Code)
	}
}

func TestFunctionRequiresAuthUnauthorized(t *testing.T) {
	pdb := newFunctionTestDB(t)
	createFunction(t, pdb, "secure", `function handler(req, ctx) { return { uid: req.user && req.user.id }; }`, "POST", true, "")
	h := &Handler{deps: &Deps{}}
	rec := callFunction(t, h, pdb, "POST", "secure", `{}`, "") // no token
	if rec.Code != 401 {
		t.Fatalf("no-token status = %d, want 401", rec.Code)
	}
}

func TestFunctionSecretAllowlist(t *testing.T) {
	enc, err := security.NewEncryptor("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	encrypted, err := enc.Encrypt("sk_live_xyz")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	pdb := newFunctionTestDB(t)
	if _, err := pdb.Writer().Exec("INSERT INTO of_secrets (name, value_encrypted) VALUES ('API_KEY', ?)", encrypted); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	h := &Handler{deps: &Deps{Encryptor: enc}}

	// Granted secret resolves.
	createFunction(t, pdb, "use-key", `function handler(req, ctx) { return { k: ctx.secret('API_KEY') }; }`, "POST", false, `["API_KEY"]`)
	rec := callFunction(t, h, pdb, "POST", "use-key", `{}`, "")
	if rec.Code != 200 {
		t.Fatalf("granted-secret status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["k"] != "sk_live_xyz" {
		t.Fatalf("secret not resolved: %v", out["k"])
	}

	// An ungranted secret name throws -> 500, even though the secret exists.
	createFunction(t, pdb, "steal-key", `function handler(req, ctx) { return { k: ctx.secret('API_KEY') }; }`, "POST", false, `[]`)
	recDenied := callFunction(t, h, pdb, "POST", "steal-key", `{}`, "")
	if recDenied.Code != 500 {
		t.Fatalf("ungranted-secret status = %d, want 500 (allowlist must deny)", recDenied.Code)
	}
}

// TestLoadFunctionEndpoint_NormalizesZeroRateLimit pins the load-time floor that
// makes a stored 0 safe. checkEndpointRateLimit reads its argument as a ceiling, so a
// 0 reaching it would reject the first request; loadFunctionEndpoint is the only
// thing standing between the two, and every apiFunction call goes through it.
func TestLoadFunctionEndpoint_NormalizesZeroRateLimit(t *testing.T) {
	pdb := newFunctionTestDB(t)
	createFunctionRateLimited(t, pdb, "free", `function handler(req, ctx) { return { ok: true }; }`, "POST", false, "", 0)
	h := &Handler{deps: &Deps{}}

	ep, err := h.loadFunctionEndpoint(pdb.Reader(), "free")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ep.RateLimit <= 0 {
		t.Fatalf("loaded RateLimit = %d, want a positive floor (0 reaches the limiter as reject-everything)", ep.RateLimit)
	}
	if rec := callFunction(t, h, pdb, "POST", "free", `{}`, ""); rec.Code != 200 {
		t.Fatalf("first request status = %d, want 200 (a stored 0 must not 429)", rec.Code)
	}
}

// TestFunctionEndpoint_RateLimitCapStillApplies is the counterweight: the floor
// above must not turn a configured cap into a no-op.
func TestFunctionEndpoint_RateLimitCapStillApplies(t *testing.T) {
	pdb := newFunctionTestDB(t)
	createFunctionRateLimited(t, pdb, "capped", `function handler(req, ctx) { return { ok: true }; }`, "POST", false, "", 1)
	h := &Handler{deps: &Deps{}}

	if rec := callFunction(t, h, pdb, "POST", "capped", `{}`, ""); rec.Code != 200 {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	if rec := callFunction(t, h, pdb, "POST", "capped", `{}`, ""); rec.Code != 429 {
		t.Fatalf("second request status = %d, want 429 (rate_limit=1 must still cap)", rec.Code)
	}
}

// TestWriteFunctionResponseClampsStatus pins the status guard. A handler can
// return any integer; net/http's WriteHeader panics on codes outside [100, 999],
// so writeFunctionResponse must remap them. (httptest's recorder records the code
// without the stdlib's panic check, so the assertion is on the remapped value,
// which is exactly what keeps the real server from panicking.)
func TestWriteFunctionResponseClampsStatus(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"unset-defaults-to-200", 0, 200},
		{"negative-defaults-to-200", -5, 200},
		{"below-100-becomes-500", 50, 500},
		{"above-599-becomes-500", 1000, 500},
		{"valid-204-passes", 204, 204},
		{"valid-599-passes", 599, 599},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeFunctionResponse(rec, &funcsandbox.Response{Status: tc.in, Body: json.RawMessage(`{"ok":true}`)})
			if rec.Code != tc.want {
				t.Fatalf("status %d -> %d, want %d", tc.in, rec.Code, tc.want)
			}
		})
	}
}

// TestWriteFunctionResponseDropsUnsafeHeaders pins the header denylist: a handler
// returns application content, so it must not be able to mint a session cookie or
// steer the transport on the project's own origin.
func TestWriteFunctionResponseDropsUnsafeHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	writeFunctionResponse(rec, &funcsandbox.Response{
		Status: 200,
		Body:   json.RawMessage(`{}`),
		Headers: map[string]string{
			"Set-Cookie":        "session=attacker",
			"Transfer-Encoding": "chunked",
			"X-Custom":          "ok",
		},
	})
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie leaked through: %q", got)
	}
	if got := rec.Header().Get("Transfer-Encoding"); got != "" {
		t.Fatalf("Transfer-Encoding leaked through: %q", got)
	}
	if got := rec.Header().Get("X-Custom"); got != "ok" {
		t.Fatalf("X-Custom = %q, want ok (safe headers must still pass)", got)
	}
}

func TestFunctionSSRFBlocked(t *testing.T) {
	pdb := newFunctionTestDB(t)
	// Cloud metadata IP: netguard.ValidateExternalURL blocks it before any dial.
	createFunction(t, pdb, "fetchmeta", `function handler(req, ctx) {
	  const res = ctx.fetch('http://169.254.169.254/latest/meta-data/');
	  return { body: res.body };
	}`, "POST", false, "")
	h := &Handler{deps: &Deps{}}
	rec := callFunction(t, h, pdb, "POST", "fetchmeta", `{}`, "")
	if rec.Code != 500 {
		t.Fatalf("SSRF fetch status = %d, want 500 (metadata IP must be blocked) (%s)", rec.Code, rec.Body.String())
	}
}
