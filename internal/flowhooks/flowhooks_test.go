package flowhooks

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"openfabrica/internal/replayoutbox"
	"openfabrica/probe"
	"openfabrica/security"
)

func testDB(t *testing.T, ddl ...string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, q := range ddl {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return db
}

func TestSeedUserCreatesARoleAccountAndToken(t *testing.T) {
	db := testDB(t,
		`CREATE TABLE of_auth_endpoints (id INTEGER PRIMARY KEY, table_name TEXT, username_column TEXT, password_column TEXT, default_role TEXT, role_column TEXT)`,
		`INSERT INTO of_auth_endpoints VALUES (1, 'members', 'email', 'password_hash', 'member', 'role')`,
		`CREATE TABLE members (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, role TEXT, display_name TEXT NOT NULL, age INTEGER NOT NULL, bio TEXT)`,
	)
	jwt := security.NewJWTManager("0123456789abcdef0123456789abcdef", time.Hour)
	h := New(3, db, jwt, nil)
	u := probe.FlowSeedUser{Email: "of-test+x-admin@example.test", Username: "oftest_x_admin", Password: "Of-secret!9aZ", Role: "admin"}
	if _, err := h.SeedUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	// Again: the leftover is replaced, not a unique-constraint failure.
	tok, err := h.SeedUser(context.Background(), u)
	if err != nil || tok == "" {
		t.Fatalf("reseed: %q %v", tok, err)
	}
	var email, hash, role, name string
	var n int
	if err := db.QueryRow(`SELECT email, password_hash, role, display_name, COUNT(*) FROM members`).Scan(&email, &hash, &role, &name, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 || email != u.Email || role != "admin" || name == "" || !security.CheckPassword(u.Password, hash) {
		t.Fatalf("row = %s %s %s n=%d", email, role, name, n)
	}
	claims, err := jwt.Validate(tok)
	if err != nil || claims.Role != "admin" || claims.ProjectID != 3 {
		t.Fatalf("token claims = %+v, %v", claims, err)
	}

	// No auth at all: a clear reason.
	if _, err := New(3, testDB(t, `CREATE TABLE of_auth_endpoints (id INTEGER PRIMARY KEY, table_name TEXT, username_column TEXT, password_column TEXT, default_role TEXT, role_column TEXT)`), nil, nil).
		SeedUser(context.Background(), u); err == nil || !strings.Contains(err.Error(), "no sign-in") {
		t.Fatalf("err = %v", err)
	}
}

func TestWebhookSignatureMatchesTheServerCheck(t *testing.T) {
	enc, err := security.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := enc.SealOptional("whsec_1")
	if err != nil {
		t.Fatal(err)
	}
	db := testDB(t,
		`CREATE TABLE of_webhooks (id INTEGER PRIMARY KEY, name TEXT, secret_encrypted TEXT NOT NULL DEFAULT '', is_enabled INTEGER)`,
		`INSERT INTO of_webhooks VALUES (1, 'payments', '`+sealed+`', 1), (2, 'open', '', 1), (3, 'broken', 'not-ciphertext', 1)`,
	)
	h := New(3, db, nil, enc)
	body := []byte(`{"type":"payment.completed"}`)
	sig, err := h.WebhookSignature("payments", body)
	if err != nil || sig != "sha256="+security.HMACSHA256Hex([]byte("whsec_1"), body) {
		t.Fatalf("sig = %q %v", sig, err)
	}
	// No such webhook, no secret set, and a secret that will not decrypt (a replaced
	// key) must each be a clear error, never an unsigned or wrongly signed request.
	for _, name := range []string{"missing", "open", "broken"} {
		if _, err := h.WebhookSignature(name, body); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := New(3, db, nil, nil).WebhookSignature("payments", body); err == nil {
		t.Error("signed without an encryption key")
	}
}

func TestMessagesComeFromTheReplayOutbox(t *testing.T) {
	h := New(41, nil, nil, nil)
	end := h.Begin()
	defer end()
	replayoutbox.Capture(41, replayoutbox.Message{Kind: "email", To: "a@b.c", Subject: "Hi", Body: "x"})
	if got := h.Messages(); len(got) != 1 || got[0].Subject != "Hi" {
		t.Fatalf("messages = %+v", got)
	}
	if err := h.RunSchedule(context.Background(), "digest"); err == nil {
		t.Fatal("without a registered runner, runSchedule explains itself")
	}
}
