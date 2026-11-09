package admin

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	_ "modernc.org/sqlite"
)

// sqlExecWriteAdapter adapts a *sql.DB to the diskPathDB interface (which wants
// an ExecWrite method) so the remap can be tested without a full ProjectDB.
type sqlExecWriteAdapter struct{ *sql.DB }

func (a sqlExecWriteAdapter) ExecWrite(query string, args ...any) (sql.Result, error) {
	return a.DB.Exec(query, args...)
}

func mustExec(t *testing.T, d *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := d.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func TestRemapDiskPath(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		oldID, newID  int
		wantNormSlash string // expected result, normalised to forward slashes
	}{
		{"forward slashes", "data/projects/7/files/logo.png", 7, 12, "data/projects/12/files/logo.png"},
		{"backslashes", `data\projects\7\files\logo.png`, 7, 12, "data/projects/12/files/logo.png"},
		{"assets subdir", "data/projects/3/assets/hero.jpg", 3, 9, "data/projects/9/assets/hero.jpg"},
		{"non-matching prefix left alone", "somewhere/else/x.png", 7, 12, "somewhere/else/x.png"},
		{"absolute unix path", "/srv/of/data/projects/7/files/logo.png", 7, 12, "data/projects/12/files/logo.png"},
		{"absolute windows path", `C:\of\data\projects\7\files\logo.png`, 7, 12, "data/projects/12/files/logo.png"},
		{"different id not touched", "data/projects/99/files/x.png", 7, 12, "data/projects/99/files/x.png"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := remapDiskPath(c.in, c.oldID, filepath.Join("data", "projects", strconv.Itoa(c.newID)))
			if filepath.ToSlash(got) != c.wantNormSlash {
				t.Errorf("remapDiskPath(%q, %d, %d) = %q, want (normalised) %q", c.in, c.oldID, c.newID, got, c.wantNormSlash)
			}
		})
	}
}

func TestSlugifyProjectName(t *testing.T) {
	cases := map[string]string{
		"My Cool App":     "my-cool-app",
		"  Spaces  ":      "spaces",
		"Wëird!!Chars$$":  "w-ird-chars",
		"":                "project",
		"___":             "project",
		"Already-good_99": "already-good-99",
	}
	for in, want := range cases {
		if got := slugifyProjectName(in); got != want {
			t.Errorf("slugifyProjectName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeJoin(t *testing.T) {
	base := filepath.Join("data", "projects", "5")
	if _, ok := safeJoin(base, "files/x.png"); !ok {
		t.Errorf("expected files/x.png to be safe")
	}
	if _, ok := safeJoin(base, "project.db"); !ok {
		t.Errorf("expected project.db to be safe")
	}
	if _, ok := safeJoin(base, "../6/secret"); ok {
		t.Errorf("expected ../6/secret to be rejected (zip-slip)")
	}
	if _, ok := safeJoin(base, "../../etc/passwd"); ok {
		t.Errorf("expected ../../etc/passwd to be rejected (zip-slip)")
	}
}

func TestRemapProjectDiskPaths(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "project.db")
	d, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustExec(t, d, `CREATE TABLE of_files (id INTEGER PRIMARY KEY, storage TEXT, disk_path TEXT)`)
	mustExec(t, d, `CREATE TABLE of_blobs (id INTEGER PRIMARY KEY, storage_path TEXT)`)
	// A disk artifact, an inline artifact (no disk_path), and a blob.
	mustExec(t, d, `INSERT INTO of_files (storage, disk_path) VALUES ('disk', 'data/projects/7/files/logo.png')`)
	mustExec(t, d, `INSERT INTO of_files (storage, disk_path) VALUES ('inline', '')`)
	mustExec(t, d, `INSERT INTO of_blobs (storage_path) VALUES ('data/projects/7/files/of_blobs/cache.bin')`)
	d.Close()

	verify, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer verify.Close()

	if err := remapProjectDiskPaths(sqlExecWriteAdapter{verify}, 7, filepath.Join("data", "projects", "42")); err != nil {
		t.Fatalf("remapProjectDiskPaths: %v", err)
	}

	var artPath, blobPath string
	verify.QueryRow(`SELECT disk_path FROM of_files WHERE storage = 'disk'`).Scan(&artPath)
	verify.QueryRow(`SELECT storage_path FROM of_blobs`).Scan(&blobPath)
	if filepath.ToSlash(artPath) != "data/projects/42/files/logo.png" {
		t.Errorf("artifact disk_path = %q, want data/projects/42/files/logo.png", artPath)
	}
	if filepath.ToSlash(blobPath) != "data/projects/42/files/of_blobs/cache.bin" {
		t.Errorf("blob storage_path = %q, want data/projects/42/files/of_blobs/cache.bin", blobPath)
	}
}

func TestSanitizeExportDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "project.db")

	// Build a minimal project DB with the tables the sanitiser touches plus an
	// artifact that must survive.
	d, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	schema := []string{
		`CREATE TABLE of_secrets (id INTEGER PRIMARY KEY, name TEXT, value_encrypted TEXT)`,
		`CREATE TABLE of_file_history (id INTEGER PRIMARY KEY, path TEXT)`,
		`CREATE TABLE of_chat_messages (id INTEGER PRIMARY KEY, body TEXT)`,
		`CREATE TABLE of_files (id INTEGER PRIMARY KEY, path TEXT, body TEXT)`,
		`CREATE TABLE of_webhooks (id INTEGER PRIMARY KEY, name TEXT, secret_encrypted TEXT, url TEXT)`,
		`CREATE TABLE of_user_totp (id INTEGER PRIMARY KEY, secret_encrypted TEXT)`,
		`CREATE TABLE of_magic_tokens (id INTEGER PRIMARY KEY, token TEXT)`,
		`CREATE TABLE of_ws_room_tokens (id INTEGER PRIMARY KEY, token TEXT)`,
		`CREATE TABLE of_tenant_invites (id INTEGER PRIMARY KEY, token TEXT)`,
		`CREATE TABLE of_tenants (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE of_auth_endpoints (id INTEGER PRIMARY KEY, table_name TEXT)`,
		// app_users is an end-user account table an auth endpoint points at; posts
		// is ordinary app content that must be preserved.
		`CREATE TABLE app_users (id INTEGER PRIMARY KEY, email TEXT, password TEXT)`,
		`CREATE TABLE posts (id INTEGER PRIMARY KEY, title TEXT)`,
	}
	for _, s := range schema {
		if _, err := d.Exec(s); err != nil {
			t.Fatalf("schema %q: %v", s, err)
		}
	}
	d.Exec(`INSERT INTO of_secrets (name, value_encrypted) VALUES ('STRIPE_KEY', 'cipher-blob')`)
	d.Exec(`INSERT INTO of_file_history (path) VALUES ('index.html')`)
	d.Exec(`INSERT INTO of_chat_messages (body) VALUES ('hello')`)
	d.Exec(`INSERT INTO of_files (path, body) VALUES ('index.html', '<h1>hi</h1>')`)
	d.Exec(`INSERT INTO of_webhooks (name, secret_encrypted, url) VALUES ('hook', 'sealed-ciphertext', 'https://example.com')`)
	d.Exec(`INSERT INTO of_user_totp (secret_encrypted) VALUES ('totp-cipher')`)
	d.Exec(`INSERT INTO of_magic_tokens (token) VALUES ('magic-tok')`)
	d.Exec(`INSERT INTO of_ws_room_tokens (token) VALUES ('room-tok')`)
	d.Exec(`INSERT INTO of_tenant_invites (token) VALUES ('invite-tok')`)
	d.Exec(`INSERT INTO of_tenants (name) VALUES ('Acme')`)
	d.Exec(`INSERT INTO of_auth_endpoints (table_name) VALUES ('app_users')`)
	d.Exec(`INSERT INTO app_users (email, password) VALUES ('user@example.com', 'bcrypt-hash')`)
	d.Exec(`INSERT INTO posts (title) VALUES ('Hello world')`)
	d.Close()

	if err := sanitizeExportDB(dbPath); err != nil {
		t.Fatalf("sanitizeExportDB: %v", err)
	}

	d2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()

	var secretName, secretVal string
	if err := d2.QueryRow(`SELECT name, value_encrypted FROM of_secrets`).Scan(&secretName, &secretVal); err != nil {
		t.Fatalf("read secret: %v", err)
	}
	if secretName != "STRIPE_KEY" {
		t.Errorf("secret name = %q, want STRIPE_KEY (names must be kept)", secretName)
	}
	if secretVal != "" {
		t.Errorf("secret value_encrypted = %q, want blank (values must be stripped)", secretVal)
	}

	var histCount, chatCount, artifactCount int
	d2.QueryRow(`SELECT COUNT(*) FROM of_file_history`).Scan(&histCount)
	d2.QueryRow(`SELECT COUNT(*) FROM of_chat_messages`).Scan(&chatCount)
	d2.QueryRow(`SELECT COUNT(*) FROM of_files`).Scan(&artifactCount)
	if histCount != 0 {
		t.Errorf("of_file_history count = %d, want 0 (history stripped)", histCount)
	}
	if chatCount != 0 {
		t.Errorf("of_chat_messages count = %d, want 0 (ephemeral cleared)", chatCount)
	}
	if artifactCount != 1 {
		t.Errorf("of_files count = %d, want 1 (authored content preserved)", artifactCount)
	}

	// The webhook config row survives, but its plaintext signing secret is blanked.
	var whCount int
	var whSecret string
	d2.QueryRow(`SELECT COUNT(*) FROM of_webhooks`).Scan(&whCount)
	d2.QueryRow(`SELECT secret_encrypted FROM of_webhooks`).Scan(&whSecret)
	if whCount != 1 {
		t.Errorf("of_webhooks count = %d, want 1 (config row preserved)", whCount)
	}
	if whSecret != "" {
		t.Errorf("of_webhooks.secret_encrypted = %q, want blank (signing secret stripped)", whSecret)
	}

	// Per-user 2FA secrets, live tokens, and tenant records are cleared entirely.
	var totpCount, magicCount, roomCount, inviteCount, tenantCount int
	d2.QueryRow(`SELECT COUNT(*) FROM of_user_totp`).Scan(&totpCount)
	d2.QueryRow(`SELECT COUNT(*) FROM of_magic_tokens`).Scan(&magicCount)
	d2.QueryRow(`SELECT COUNT(*) FROM of_ws_room_tokens`).Scan(&roomCount)
	d2.QueryRow(`SELECT COUNT(*) FROM of_tenant_invites`).Scan(&inviteCount)
	d2.QueryRow(`SELECT COUNT(*) FROM of_tenants`).Scan(&tenantCount)
	if totpCount != 0 || magicCount != 0 || roomCount != 0 || inviteCount != 0 || tenantCount != 0 {
		t.Errorf("instance-scoped rows not fully cleared: totp=%d magic=%d room=%d invite=%d tenant=%d",
			totpCount, magicCount, roomCount, inviteCount, tenantCount)
	}

	// The auth-referenced account table is emptied; ordinary app content is kept.
	var userCount, postCount int
	d2.QueryRow(`SELECT COUNT(*) FROM app_users`).Scan(&userCount)
	d2.QueryRow(`SELECT COUNT(*) FROM posts`).Scan(&postCount)
	if userCount != 0 {
		t.Errorf("app_users count = %d, want 0 (end-user accounts stripped)", userCount)
	}
	if postCount != 1 {
		t.Errorf("posts count = %d, want 1 (app content preserved)", postCount)
	}
}

// Payment, push, webhook-log and run-transcript rows are end-user data that a
// shared template must not carry; authored config next to them stays.
func TestSanitizeExportDBClearsEndUserRecords(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "project.db")
	d, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cleared := []string{
		"of_payments", "of_subscriptions", "of_push_subscriptions", "of_webhook_logs",
		"of_processed_webhooks", "of_tenant_members", "of_agent_runs", "of_ws_room_state",
		"of_llm_usage_daily",
	}
	for _, tbl := range append([]string{"of_payment_config"}, cleared...) {
		if _, err := d.Exec(`CREATE TABLE ` + tbl + ` (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
			t.Fatalf("create %s: %v", tbl, err)
		}
		if _, err := d.Exec(`INSERT INTO ` + tbl + ` (v) VALUES ('x')`); err != nil {
			t.Fatalf("insert %s: %v", tbl, err)
		}
	}
	d.Close()

	if err := sanitizeExportDB(dbPath); err != nil {
		t.Fatalf("sanitizeExportDB: %v", err)
	}

	d2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	for _, tbl := range cleared {
		var n int
		d2.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n)
		if n != 0 {
			t.Errorf("%s has %d rows after export sanitising, want 0", tbl, n)
		}
	}
	var cfg int
	d2.QueryRow(`SELECT COUNT(*) FROM of_payment_config`).Scan(&cfg)
	if cfg != 1 {
		t.Errorf("of_payment_config count = %d, want 1 (authored config preserved)", cfg)
	}
}
