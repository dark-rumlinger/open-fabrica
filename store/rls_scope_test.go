package store

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// setupRLSDB builds the minimal schema UnscopedOwnerEndpoints reads:
// of_api_endpoints plus the backing tables it introspects via PRAGMA table_info.
// notes/posts are row-owned (user_id / author_id); tags is shared (no owner-like
// column).
func setupRLSDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		`CREATE TABLE of_api_endpoints (
			id INTEGER PRIMARY KEY,
			path TEXT,
			table_name TEXT,
			requires_auth INTEGER DEFAULT 0,
			public_read INTEGER DEFAULT 0,
			public_write INTEGER DEFAULT 0,
			owner_column TEXT,
			tenant_column TEXT,
			read_only INTEGER DEFAULT 0
		)`,
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, user_id INTEGER, body TEXT)`,
		`CREATE TABLE posts (id INTEGER PRIMARY KEY, author_id INTEGER, title TEXT)`,
		`CREATE TABLE tags (id INTEGER PRIMARY KEY, label TEXT)`,
		// messages is collaborative: an author column (user_id) AND a container
		// reference (channel_id), the shape a per-user owner scope would break.
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, channel_id INTEGER, user_id INTEGER, body TEXT)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	return db
}

func addEndpoint(t *testing.T, db *sql.DB, path, table string, requiresAuth, publicRead, readOnly int, owner, tenant string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_api_endpoints
		(path, table_name, requires_auth, public_read, owner_column, tenant_column, read_only)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		path, table, requiresAuth, publicRead, owner, tenant, readOnly); err != nil {
		t.Fatalf("insert endpoint %s: %v", path, err)
	}
}

func TestOwnerLikeColumn(t *testing.T) {
	// First match in the given order wins (callers pass definition order).
	if got := OwnerLikeColumn([]string{"id", "body", "USER_ID"}); got != "USER_ID" {
		t.Errorf("owner-like column = %q, want USER_ID (case-insensitive match)", got)
	}
	if got := OwnerLikeColumn([]string{"id", "author_id", "user_id"}); got != "author_id" {
		t.Errorf("owner-like column = %q, want author_id (first in order)", got)
	}
	if got := OwnerLikeColumn([]string{"id", "label"}); got != "" {
		t.Errorf("owner-like column = %q, want empty (no owner-like column)", got)
	}
}

func TestUnscopedOwnerEndpoints_FlagsAuthUnscoped(t *testing.T) {
	db := setupRLSDB(t)
	addEndpoint(t, db, "notes", "notes", 1, 0, 0, "", "")

	got := UnscopedOwnerEndpoints(db)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(got), got)
	}
	f := got[0]
	if f.Path != "notes" || f.Table != "notes" || f.Column != "user_id" {
		t.Errorf("finding = %+v, want path/table=notes column=user_id", f)
	}
	if !f.RequiresAuth || f.PublicRead {
		t.Errorf("finding flags = %+v, want RequiresAuth=true PublicRead=false", f)
	}
	// A private-per-user table carries no container reference.
	if f.Container != "" {
		t.Errorf("notes finding Container=%q, want empty (no container reference)", f.Container)
	}
}

func TestContainerColumn(t *testing.T) {
	if got := ContainerColumn([]string{"id", "user_id", "Channel_ID", "body"}); got != "Channel_ID" {
		t.Errorf("container column = %q, want Channel_ID (case-insensitive match)", got)
	}
	if got := ContainerColumn([]string{"id", "user_id", "body"}); got != "" {
		t.Errorf("container column = %q, want empty (no container reference)", got)
	}
	// The container and owner-like sets are disjoint: an owner-like column is never
	// mistaken for a container.
	if got := ContainerColumn([]string{"id", "user_id", "created_by"}); got != "" {
		t.Errorf("container column = %q, want empty (owner-like columns are not containers)", got)
	}
}

// TestUnscopedOwnerEndpoints_ContainerMarksSharedContent proves the collaborative
// signal reaches the finding: a table with an author column AND a container FK
// carries the container so the validator can lead with authored instead of a
// per-user owner scope.
func TestUnscopedOwnerEndpoints_ContainerMarksSharedContent(t *testing.T) {
	db := setupRLSDB(t)
	addEndpoint(t, db, "messages", "messages", 1, 0, 0, "", "")

	got := UnscopedOwnerEndpoints(db)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(got), got)
	}
	f := got[0]
	if f.Column != "user_id" {
		t.Errorf("owner-like column = %q, want user_id", f.Column)
	}
	if f.Container != "channel_id" {
		t.Errorf("container = %q, want channel_id (marks likely-shared content)", f.Container)
	}
}

// A public_write endpoint over an owner-like table is not flagged: its writes are
// anonymous (no identity to scope to) and its reads are admin-gated, so there is
// no cross-user leak, and owner-scoping it would be invalid.
func TestUnscopedOwnerEndpoints_PublicWriteExcluded(t *testing.T) {
	db := setupRLSDB(t)
	// notes has user_id (owner-like); requires_auth=1, public_write=1.
	if _, err := db.Exec(`INSERT INTO of_api_endpoints
		(path, table_name, requires_auth, public_read, public_write, owner_column, tenant_column, read_only)
		VALUES ('notes','notes',1,0,1,'','',0)`); err != nil {
		t.Fatal(err)
	}
	if got := UnscopedOwnerEndpoints(db); len(got) != 0 {
		t.Fatalf("public_write endpoint must be excluded from rls_scope, got: %+v", got)
	}
}

func TestUnscopedOwnerEndpoints_ScopedIsClean(t *testing.T) {
	db := setupRLSDB(t)
	// owner_column set -> runtime scopes the query -> nothing to flag.
	addEndpoint(t, db, "notes", "notes", 1, 0, 0, "user_id", "")
	// tenant_column set (no owner_column) is also scoped.
	addEndpoint(t, db, "posts", "posts", 1, 0, 0, "", "author_id")

	if got := UnscopedOwnerEndpoints(db); len(got) != 0 {
		t.Fatalf("scoped endpoints must be clean, got: %+v", got)
	}
}

func TestUnscopedOwnerEndpoints_PublicReadCarriesFlag(t *testing.T) {
	db := setupRLSDB(t)
	addEndpoint(t, db, "posts", "posts", 0, 1, 0, "", "")

	got := UnscopedOwnerEndpoints(db)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(got), got)
	}
	if !got[0].PublicRead || got[0].Column != "author_id" {
		t.Errorf("finding = %+v, want PublicRead=true column=author_id", got[0])
	}
}

func TestUnscopedOwnerEndpoints_SkipsReadOnlyAndSharedTables(t *testing.T) {
	db := setupRLSDB(t)
	// read_only (view-backed): no RLS surface even though the table is row-owned.
	addEndpoint(t, db, "notes-view", "notes", 0, 1, 1, "", "")
	// Shared table with no owner-like column: nothing to scope.
	addEndpoint(t, db, "tags", "tags", 1, 0, 0, "", "")
	// Endpoint whose backing table does not exist: best-effort -> no finding.
	addEndpoint(t, db, "ghost", "missing_table", 1, 0, 0, "", "")

	if got := UnscopedOwnerEndpoints(db); len(got) != 0 {
		t.Fatalf("read-only / shared / missing-table endpoints must be clean, got: %+v", got)
	}
}
