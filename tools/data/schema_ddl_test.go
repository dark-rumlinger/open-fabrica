package data

import "testing"

// These characterize the pure DDL-construction helpers in schema_ddl.go
// (parseColumnDef / columnSQL / normalizeCheckExpr / quoteDefaultValue). They
// are security-sensitive, they build the SQL that creates dynamic tables, so
// the behavior is pinned here before any refactor.

func TestParseColumnDef_SimpleAndExtended(t *testing.T) {
	// Simple string form, case-insensitive.
	cd, err := parseColumnDef("title", "text")
	if err != nil {
		t.Fatalf("simple: %v", err)
	}
	if cd.Type != "TEXT" || cd.SQLType != "TEXT" {
		t.Errorf("simple type: got %q/%q", cd.Type, cd.SQLType)
	}

	// Extended object form with constraints.
	cd, err = parseColumnDef("status", map[string]any{
		"type": "TEXT", "not_null": true, "unique": true, "default": "draft",
	})
	if err != nil {
		t.Fatalf("extended: %v", err)
	}
	if !cd.NotNull || !cd.Unique || cd.Default != "draft" {
		t.Errorf("extended constraints not captured: %+v", cd)
	}
}

func TestParseColumnDef_DefaultTypeCoercion(t *testing.T) {
	// JSON numbers arrive as float64; whole numbers format as integers.
	cd, _ := parseColumnDef("qty", map[string]any{"type": "INTEGER", "default": float64(5)})
	if cd.Default != "5" {
		t.Errorf("whole float default: got %q, want 5", cd.Default)
	}
	// JSON booleans (e.g. BOOLEAN default false) must not be silently dropped.
	cd, _ = parseColumnDef("flag", map[string]any{"type": "BOOLEAN", "default": false})
	if cd.Default != "0" {
		t.Errorf("bool default false: got %q, want 0", cd.Default)
	}
	cd, _ = parseColumnDef("flag", map[string]any{"type": "BOOLEAN", "default": true})
	if cd.Default != "1" {
		t.Errorf("bool default true: got %q, want 1", cd.Default)
	}
}

func TestParseColumnDef_Errors(t *testing.T) {
	if _, err := parseColumnDef("x", "NARWHAL"); err == nil {
		t.Error("expected error for unsupported type")
	}
	if _, err := parseColumnDef("x", map[string]any{"not_null": true}); err == nil {
		t.Error("expected error when 'type' missing in object form")
	}
	if _, err := parseColumnDef("x", map[string]any{"type": "TEXT", "references": "not valid"}); err == nil {
		t.Error("expected error for malformed references")
	}
	if _, err := parseColumnDef("x", 42); err == nil {
		t.Error("expected error for non-string/object raw value")
	}
}

func TestParseColumnDef_SecureColumns(t *testing.T) {
	cd, err := parseColumnDef("pw", "PASSWORD")
	if err != nil {
		t.Fatalf("password: %v", err)
	}
	if !cd.IsSecure || cd.Kind != "hash" || cd.SQLType != "TEXT" {
		t.Errorf("PASSWORD: got secure=%v kind=%q sql=%q", cd.IsSecure, cd.Kind, cd.SQLType)
	}
	cd, _ = parseColumnDef("secret", "ENCRYPTED")
	if !cd.IsSecure || cd.Kind != "encrypt" {
		t.Errorf("ENCRYPTED: got secure=%v kind=%q", cd.IsSecure, cd.Kind)
	}
}

func TestColumnSQL(t *testing.T) {
	cd := &schemaColumnDef{Name: "status", SQLType: "TEXT", NotNull: true, Default: "draft", Check: "IN ('draft','live')"}
	got := cd.columnSQL()
	want := "status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','live'))"
	if got != want {
		t.Errorf("columnSQL:\n got %q\nwant %q", got, want)
	}

	fk := &schemaColumnDef{Name: "user_id", SQLType: "INTEGER", References: "users(id)"}
	if got := fk.columnSQL(); got != "user_id INTEGER REFERENCES users(id)" {
		t.Errorf("fk columnSQL: got %q", got)
	}
}

func TestQuoteDefaultValue(t *testing.T) {
	cases := []struct{ val, sqlType, want string }{
		{"draft", "TEXT", "'draft'"},                       // bare text gets quoted
		{"'draft'", "TEXT", "'draft'"},                     // already-quoted passes through
		{"O'Brien", "TEXT", "'O''Brien'"},                  // injection guard: internal quote escaped
		{"NULL", "TEXT", "NULL"},                           // keyword preserved
		{"CURRENT_TIMESTAMP", "TEXT", "CURRENT_TIMESTAMP"}, // keyword preserved
		{"0", "INTEGER", "0"},                              // numeric passes through bare
		{"3.14", "REAL", "3.14"},                           // real passes through bare
	}
	for _, c := range cases {
		if got := quoteDefaultValue(c.val, c.sqlType); got != c.want {
			t.Errorf("quoteDefaultValue(%q,%q): got %q, want %q", c.val, c.sqlType, got, c.want)
		}
	}
}
