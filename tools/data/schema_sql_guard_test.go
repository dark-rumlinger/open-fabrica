package data

import (
	"strings"
	"testing"
)

// TestValidateCheckExpr_RejectsInjection covers the hostile shapes an authored
// `check` can take. Each case carries the reason it must be refused. These reach
// a live Exec, where the driver runs every statement in the string, so an
// accepted case here is a dropped table.
func TestValidateCheckExpr_RejectsInjection(t *testing.T) {
	cases := []struct {
		name   string
		check  string
		reason string
	}{
		{"stacked drop", "> 0)); DROP TABLE of_dynamic_tables; --",
			"closes the CREATE TABLE and appends a DROP of a table the model is otherwise denied"},
		{"stacked without close", "> 0; DROP TABLE users",
			"a bare ';' starts a second statement even with parens balanced"},
		{"early close", "> 0)) --",
			"unbalanced ')' closes the column list, then '--' hides the real remainder"},
		{"early close no comment", "> 0))",
			"unbalanced ')' truncates the generated column list"},
		{"line comment", "> 0 -- rest is hidden",
			"'--' comments out the columns the builder appends after this fragment"},
		{"block comment", "> 0 /* swallow */",
			"'/*' swallows the builder's own trailing SQL"},
		{"unterminated literal", "IN ('a",
			"an unclosed quote swallows the remainder of the statement"},
		{"paren left open", "IN ('a','b'",
			"an unclosed '(' leaves the emitted CHECK malformed"},
		{"attach", "> 0); ATTACH DATABASE '/tmp/x' AS y; --",
			"stacked ATTACH reaches the filesystem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCheckExpr("amount", tc.check); err == nil {
				t.Errorf("validateCheckExpr(%q) = nil, want rejection\nreason: %s", tc.check, tc.reason)
			}
		})
	}
}

// TestValidateCheckExpr_AcceptsLegitimate is the negative control: a structural
// guard that rejects real schemas is worse than none, because a caller cannot
// route around it. Every shape TestNormalizeCheckExpr pins must survive, plus the
// literal-content cases that prove quoted regions are treated as content.
func TestValidateCheckExpr_AcceptsLegitimate(t *testing.T) {
	cases := []struct {
		name  string
		check string
	}{
		{"bare comparison", "> 0"},
		{"bare gte", ">= 18"},
		{"bare in", "IN ('a','b')"},
		{"bare in no space", "IN('a','b')"},
		{"bare not in", "NOT IN ('x')"},
		{"bare like", "LIKE 'A%'"},
		{"full predicate in", "discipline IN ('Branding','Editorial','Web','Type')"},
		{"full predicate func", "length(name) > 0"},
		{"nested parens", "(qty > 0 AND qty < 100)"},
		{"semicolon inside literal", "IN (';', 'b')"},
		{"dashes inside literal", "IN ('a--b', 'c')"},
		{"escaped quote inside literal", "IN ('it''s', 'b')"},
		{"between", "BETWEEN 1 AND 10"},
		{"quoted identifier", "\"order\" > 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCheckExpr("col", tc.check); err != nil {
				t.Errorf("validateCheckExpr(%q) = %v, want nil (legitimate constraint must not be rejected)", tc.check, err)
			}
		})
	}
}

// TestValidateDefaultLiteral guards the two passthrough branches in
// quoteDefaultValue: an already-quoted value emitted verbatim, and a numeric
// column's value emitted with no quoting at all.
func TestValidateDefaultLiteral(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		sqlType string
		wantErr bool
		reason  string
	}{
		{"numeric injection", "1); DROP TABLE of_dynamic_tables; --", "INTEGER", true,
			"a numeric default is emitted unquoted, so anything but a number is live SQL"},
		{"numeric with trailing sql", "0 OR 1=1", "INTEGER", true,
			"unquoted passthrough makes an expression out of a literal"},
		{"quoted text escape", "'draft'); DROP TABLE of_dynamic_tables; --", "TEXT", true,
			"the leading quote hits the already-quoted passthrough, bypassing the escaping below it"},
		{"quoted with trailing text", "'a' || 'b'", "TEXT", true,
			"text after the closing quote lands outside the literal in the emitted DDL"},
		{"unterminated quote", "'draft", "TEXT", true,
			"an unclosed quote swallows the builder's trailing SQL"},

		{"plain int", "0", "INTEGER", false, ""},
		{"negative int", "-2", "INTEGER", false, ""},
		{"real", "2.5", "REAL", false, ""},
		{"exponent from %g", "1e+20", "REAL", false, "Go renders large JSON number defaults this way"},
		{"hex", "0x1F", "INTEGER", false, ""},
		{"bool one", "1", "BOOLEAN", false, ""},
		{"quoted literal", "'draft'", "TEXT", false, ""},
		{"quoted with escaped quote", "'it''s'", "TEXT", false, ""},
		{"bare text is escaped by the caller", "draft", "TEXT", false,
			"quoteDefaultValue escapes and quotes bare text, so it is safe by construction"},
		{"bare text with quote", "it's", "TEXT", false,
			"the caller doubles the quote; no passthrough branch is reached"},
		{"keyword null", "NULL", "TEXT", false, ""},
		{"keyword current_timestamp", "CURRENT_TIMESTAMP", "TEXT", false, ""},
		{"keyword false on numeric", "FALSE", "BOOLEAN", false, ""},
		{"empty", "", "TEXT", false, "no DEFAULT is emitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDefaultLiteral("col", tc.val, tc.sqlType)
			if tc.wantErr && err == nil {
				t.Errorf("validateDefaultLiteral(%q, %s) = nil, want rejection\nreason: %s", tc.val, tc.sqlType, tc.reason)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateDefaultLiteral(%q, %s) = %v, want nil. %s", tc.val, tc.sqlType, err, tc.reason)
			}
		})
	}
}

// TestParseColumnDef_RejectsInjectedDDL is the end-to-end assertion: a payload
// must die at parseColumnDef, before columnSQL can compose it into DDL.
func TestParseColumnDef_RejectsInjectedDDL(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
	}{
		{"check", map[string]any{"type": "INTEGER", "check": "> 0)); DROP TABLE of_dynamic_tables; --"}},
		{"numeric default", map[string]any{"type": "INTEGER", "default": "1); DROP TABLE of_dynamic_tables; --"}},
		{"text default", map[string]any{"type": "TEXT", "default": "'x'); DROP TABLE of_dynamic_tables; --"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cd, err := parseColumnDef("col", tc.raw)
			if err == nil {
				t.Fatalf("parseColumnDef accepted an injection payload; columnSQL would emit: %s", cd.columnSQL())
			}
			if !strings.Contains(err.Error(), "col") {
				t.Errorf("error should name the offending column so the model can fix it, got: %v", err)
			}
		})
	}
}

// TestParseColumnDef_AcceptsRealSchema is the negative control for the end-to-end
// path: a representative real column must parse and emit its DDL unchanged.
func TestParseColumnDef_AcceptsRealSchema(t *testing.T) {
	cd, err := parseColumnDef("discipline", map[string]any{
		"type":     "TEXT",
		"not_null": true,
		"default":  "'Web'",
		"check":    "discipline IN ('Branding','Editorial','Web','Type')",
	})
	if err != nil {
		t.Fatalf("parseColumnDef rejected a legitimate column: %v", err)
	}
	got := cd.columnSQL()
	want := "discipline TEXT NOT NULL DEFAULT 'Web' CHECK (discipline IN ('Branding','Editorial','Web','Type'))"
	if got != want {
		t.Errorf("columnSQL() = %q, want %q", got, want)
	}
}
