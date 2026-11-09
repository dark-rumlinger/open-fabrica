package observability

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"openfabrica/security"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// mintTestTokenFromAnyUser mints only when a user exists, kind-agnostically.
func TestMintTestTokenFromAnyUser(t *testing.T) {
	pdb, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pdb.Close() })
	if _, err := pdb.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, role TEXT)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	ctx := &tools.ToolContext{DB: pdb, ProjectID: 7, JWTManager: security.NewJWTManager("s", time.Hour)}

	if _, ok := mintTestTokenFromAnyUser(ctx); ok {
		t.Fatal("no users -> no token")
	}
	if _, err := pdb.Exec(`INSERT INTO users (email, role) VALUES ('u@t', 'user')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if tok, ok := mintTestTokenFromAnyUser(ctx); !ok || tok == "" {
		t.Fatalf("expected a token once a user exists, got ok=%v tok=%q", ok, tok)
	}
}

// The generic auth path: an endpoint that 401s without a token is probed, and the
// probe mints a token from an existing user and retries ONCE - so a healthy auth
// endpoint of ANY kind reads as passing without the caller supplying a token.
func TestEndpointProbe_AuthSelfHeal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/secret", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	gdb, _ := sql.Open("sqlite", ":memory:")
	t.Cleanup(func() { gdb.Close() })
	mustExec(t, gdb, `CREATE TABLE projects (id INTEGER, domain TEXT)`)
	mustExec(t, gdb, `INSERT INTO projects (id, domain) VALUES (7, 'test.local')`)
	pdb, _ := sql.Open("sqlite", ":memory:")
	t.Cleanup(func() { pdb.Close() })
	mustExec(t, pdb, `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, role TEXT)`)
	mustExec(t, pdb, `INSERT INTO users (email, role) VALUES ('u@t','user')`)

	ctx := &tools.ToolContext{
		Ctx: context.Background(), DB: pdb, GlobalDB: gdb, ProjectID: 7,
		PublicPort: port, JWTManager: security.NewJWTManager("s", time.Hour),
	}

	// No token supplied: the 401 heals into a minted retry -> pass, flagged auto_auth.
	res, err := (&DiagTestEndpoint{EndpointPath: "secret", Method: "POST"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("auth self-heal should pass, got %+v", res)
	}
	if data, _ := res.Data.(map[string]any); data["auto_auth"] != true {
		t.Errorf("expected auto_auth=true after self-heal, got %v", data["auto_auth"])
	}

	// A deliberately-expected 401 (probing the auth gate) is NOT healed - it passes as 401.
	res2, err := (&DiagTestEndpoint{EndpointPath: "secret", Method: "POST", ExpectedStatus: 401}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res2 == nil || !res2.Success {
		t.Fatalf("an expected-401 probe should pass without healing, got %+v", res2)
	}
}

// expected_fields promises an object: a bare list (the classic envelope break) or a
// non-JSON body must fail the probe, not pass it silently.
func TestEndpointProbe_ExpectedFieldsRequireAnObject(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/list", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1}]`))
	})
	mux.HandleFunc("/api/env", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"rows":[],"count":0}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	gdb, _ := sql.Open("sqlite", ":memory:")
	t.Cleanup(func() { gdb.Close() })
	mustExec(t, gdb, `CREATE TABLE projects (id INTEGER, domain TEXT)`)
	mustExec(t, gdb, `INSERT INTO projects (id, domain) VALUES (7, 'test.local')`)
	pdb, _ := sql.Open("sqlite", ":memory:")
	t.Cleanup(func() { pdb.Close() })
	ctx := &tools.ToolContext{Ctx: context.Background(), DB: pdb, GlobalDB: gdb, ProjectID: 7, PublicPort: port}

	res, err := (&DiagTestEndpoint{EndpointPath: "list", ExpectedFields: []string{"rows"}}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("a bare list must fail expected_fields, got %+v", res)
	}
	res, err = (&DiagTestEndpoint{EndpointPath: "env", ExpectedFields: []string{"rows", "count"}}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("an envelope carrying the fields must pass, got %+v", res)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
