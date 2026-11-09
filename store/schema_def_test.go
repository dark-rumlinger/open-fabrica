package store

import "testing"

// Both column forms come out of ONE create_table call, so a reader that handles
// only one silently loses the other's columns. These pin the two failure modes:
// a slice reader seeing nothing, a map reader blanking the type.
func TestParseSchemaDefReadsBothColumnForms(t *testing.T) {
	cols := ParseSchemaDef(`{
		"note":   "TEXT",
		"title":  {"type":"TEXT","not_null":true},
		"views":  {"type":"INTEGER","not_null":true,"default":"0"},
		"code":   {"type":"TEXT","not_null":true,"generate":"code"}
	}`)
	if len(cols) != 4 {
		t.Fatalf("parsed %d columns, want 4: %+v", len(cols), cols)
	}
	by := map[string]SchemaColumn{}
	for _, c := range cols {
		by[c.Name] = c
	}
	if c := by["note"]; c.Type != "TEXT" || c.NotNull {
		t.Errorf("note = %+v, want TEXT nullable (bare-string form)", c)
	}
	if c := by["title"]; c.Type != "TEXT" || !c.NotNull || c.ServerFills {
		t.Errorf("title = %+v, want TEXT not-null (constraint-object form)", c)
	}
	if c := by["views"]; !c.ServerFills {
		t.Errorf("views = %+v, want ServerFills - a DEFAULT means the caller need not send it", c)
	}
	if c := by["code"]; !c.ServerFills {
		t.Errorf("code = %+v, want ServerFills - a generate expression is filled server-side", c)
	}
}

// The set form is membership only, and must include the constraint-object columns:
// a consumer that filters writable columns by this set drops any column it misses.
func TestSchemaDefColumnSetIncludesConstraintColumns(t *testing.T) {
	set := SchemaDefColumnSet(`{"points":{"type":"TEXT","not_null":true},"room":"TEXT"}`)
	for _, want := range []string{"points", "room"} {
		if !set[want] {
			t.Errorf("column %q missing from the set %v - a write_to_table insert would omit it", want, set)
		}
	}
}

// Column order must not depend on Go's randomized map iteration: callers render
// this into files that would otherwise churn between identical runs.
func TestParseSchemaDefIsDeterministic(t *testing.T) {
	const def = `{"zeta":"TEXT","alpha":"TEXT","mid":"INTEGER"}`
	first := ParseSchemaDef(def)
	for i := 0; i < 20; i++ {
		next := ParseSchemaDef(def)
		for j := range first {
			if first[j].Name != next[j].Name {
				t.Fatalf("column order varies between runs (%v vs %v)", first[j].Name, next[j].Name)
			}
		}
	}
	if first[0].Name != "alpha" {
		t.Errorf("columns should be name-sorted, got %q first", first[0].Name)
	}
}

// A malformed or non-object blob must not take a table's whole config down with
// it: the caller falls back rather than panicking or half-reading.
func TestParseSchemaDefRejectsNonObjectBlobs(t *testing.T) {
	for _, def := range []string{``, `[]`, `"TEXT"`, `not json`, `[{"name":"title"}]`} {
		if cols := ParseSchemaDef(def); cols != nil {
			t.Errorf("ParseSchemaDef(%q) = %+v, want nil", def, cols)
		}
	}
	// An unusable column is skipped; its siblings still parse.
	cols := ParseSchemaDef(`{"ok":"TEXT","weird":123,"noType":{"not_null":true}}`)
	if len(cols) != 1 || cols[0].Name != "ok" {
		t.Errorf("got %+v, want just the usable column", cols)
	}
}
