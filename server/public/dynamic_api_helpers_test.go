package public

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// setupRowIDColumnDB opens an in-memory SQLite and runs the supplied DDL.
// Returned DB is closed via t.Cleanup.
func setupRowIDColumnDB(t *testing.T, ddl ...string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("ddl %q: %v", stmt, err)
		}
	}
	return db
}

func TestRowIDColumn_NumericMatchesID(t *testing.T) {
	db := setupRowIDColumnDB(t, `CREATE TABLE rooms (id INTEGER PRIMARY KEY, code TEXT UNIQUE)`)
	col, val := rowIDColumn(db, "rooms", "42", nil)
	if col != "id" || val != "42" {
		t.Fatalf("numeric should match id; got col=%q val=%v", col, val)
	}
}

func TestRowIDColumn_ExplicitLookupColumnWins(t *testing.T) {
	db := setupRowIDColumnDB(t, `CREATE TABLE rooms (id INTEGER PRIMARY KEY, code TEXT UNIQUE, slug TEXT UNIQUE)`)
	ep := &apiEndpoint{LookupColumn: "code"}
	col, val := rowIDColumn(db, "rooms", "calm-ridge-74", ep)
	if col != "code" || val != "calm-ridge-74" {
		t.Fatalf("explicit lookup_column should win even over slug; got col=%q val=%v", col, val)
	}
}

func TestRowIDColumn_SlugFallback(t *testing.T) {
	db := setupRowIDColumnDB(t, `CREATE TABLE posts (id INTEGER PRIMARY KEY, slug TEXT UNIQUE, body TEXT)`)
	col, val := rowIDColumn(db, "posts", "my-first-post", nil)
	if col != "slug" || val != "my-first-post" {
		t.Fatalf("slug column should be auto-detected; got col=%q val=%v", col, val)
	}
}

func TestRowIDColumn_UUIDFallback(t *testing.T) {
	db := setupRowIDColumnDB(t, `CREATE TABLE artifacts (id INTEGER PRIMARY KEY, uuid TEXT UNIQUE, body TEXT)`)
	col, _ := rowIDColumn(db, "artifacts", "deadbeef-cafe", nil)
	if col != "uuid" {
		t.Fatalf("uuid column should be auto-detected; got col=%q", col)
	}
}

func TestRowIDColumn_SoleUniqueTextAutoDetect(t *testing.T) {
	// rooms.code is the only UNIQUE TEXT column besides id - autodetect picks it
	// even with no lookup_column and without relying on special-cased slug/uuid names.
	db := setupRowIDColumnDB(t, `CREATE TABLE rooms (id INTEGER PRIMARY KEY, code TEXT UNIQUE, status TEXT)`)
	col, val := rowIDColumn(db, "rooms", "calm-ridge-74", nil)
	if col != "code" || val != "calm-ridge-74" {
		t.Fatalf("sole non-id UNIQUE TEXT column should be auto-detected; got col=%q val=%v", col, val)
	}
}

func TestRowIDColumn_MultipleUniqueTextDeclines(t *testing.T) {
	// With two UNIQUE TEXT columns and no explicit lookup_column / slug / uuid,
	// autodetect must NOT guess. Falls back to "id" so the 404 surfaces in the
	// normal path rather than silently picking the wrong column.
	db := setupRowIDColumnDB(t, `CREATE TABLE invites (id INTEGER PRIMARY KEY, token TEXT UNIQUE, email TEXT UNIQUE)`)
	col, _ := rowIDColumn(db, "invites", "any-string", nil)
	if col != "id" {
		t.Fatalf("ambiguous case should fall back to id; got col=%q", col)
	}
}

func TestRowIDColumn_NoUniqueTextFallsBackToID(t *testing.T) {
	db := setupRowIDColumnDB(t, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`)
	col, _ := rowIDColumn(db, "notes", "abc", nil)
	if col != "id" {
		t.Fatalf("no candidate column should fall back to id; got col=%q", col)
	}
}

func TestParseFilterParam_Operators(t *testing.T) {
	cases := []struct {
		key             string
		col, op         string
		isLike, isToken bool
		ok              bool
	}{
		{"tags", "tags", "=", false, false, true},
		{"title__like", "title", "LIKE", true, false, true},
		{"tags__has", "tags", "", false, true, true},
		{"views__gt", "views", ">", false, false, true},
		{"views__gte", "views", ">=", false, false, true},
		{"views__lt", "views", "<", false, false, true},
		{"views__lte", "views", "<=", false, false, true},
		{"status__ne", "status", "!=", false, false, true},
		{"col__bogus", "", "", false, false, false},
	}
	for _, c := range cases {
		col, op, isLike, isToken, ok := parseFilterParam(c.key)
		if col != c.col || op != c.op || isLike != c.isLike || isToken != c.isToken || ok != c.ok {
			t.Errorf("%s: got (col=%q op=%q isLike=%v isToken=%v ok=%v), want (col=%q op=%q isLike=%v isToken=%v ok=%v)",
				c.key, col, op, isLike, isToken, ok, c.col, c.op, c.isLike, c.isToken, c.ok)
		}
	}
}

// TestTokenMembership_SQL runs the exact clause/pattern the handler builds
// against in-memory SQLite, proving whole-token semantics: a tag matches only
// as a complete comma-delimited element, case-insensitively, regardless of
// spacing after the comma.
func TestTokenMembership_SQL(t *testing.T) {
	db := setupRowIDColumnDB(t, `CREATE TABLE posts (id INTEGER PRIMARY KEY, slug TEXT, tags TEXT)`,
		`INSERT INTO posts (slug, tags) VALUES
			('a', 'go,web,api'),
			('b', 'go, web, api'),
			('c', 'golang,webdev'),
			('d', 'Web,Design'),
			('e', 'machine learning, ai'),
			('f', '')`)

	query := `SELECT slug FROM posts WHERE ` + tokenMembershipClause(`"tags"`) + ` ORDER BY slug`

	collect := func(token string) []string {
		rows, err := db.Query(query, tokenMembershipPattern(token))
		if err != nil {
			t.Fatalf("query token %q: %v", token, err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows token %q: %v", token, err)
		}
		return got
	}

	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	cases := []struct {
		token string
		want  []string
	}{
		{"go", []string{"a", "b"}},          // exact token, not the "go" inside "golang"
		{"web", []string{"a", "b", "d"}},    // case-insensitive, spaced and unspaced
		{"api", []string{"a", "b"}},         // trailing token
		{"golang", []string{"c"}},           // substring of nothing else
		{"machine learning", []string{"e"}}, // internal space preserved
		{"design", []string{"d"}},           // case-insensitive against "Design"
		{"missing", nil},                    // no match
	}
	for _, c := range cases {
		if got := collect(c.token); !eq(got, c.want) {
			t.Errorf("token %q: got %v, want %v", c.token, got, c.want)
		}
	}
}

func TestRowIDColumn_InvalidLookupColumnIgnored(t *testing.T) {
	// If lookup_column references a column that doesn't exist on the table,
	// rowIDColumn must not interpolate it (would cause a SQL error or worse).
	// Falls through to the next resolution step.
	db := setupRowIDColumnDB(t, `CREATE TABLE rooms (id INTEGER PRIMARY KEY, code TEXT UNIQUE)`)
	ep := &apiEndpoint{LookupColumn: "missing_col"}
	col, _ := rowIDColumn(db, "rooms", "calm-ridge-74", ep)
	if col != "code" {
		t.Fatalf("non-existent lookup_column should fall through to autodetect; got col=%q", col)
	}
}
