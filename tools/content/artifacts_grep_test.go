package content

import (
	"strings"
	"testing"

	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

type grepRow struct{ kind, path, body string }

func setupGrepDB(t *testing.T, rows []grepRow) *tools.ToolContext {
	t.Helper()
	db := tooltest.ProjectDB(t)
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO of_files (kind, path, body, storage, is_deleted) VALUES (?, ?, ?, 'inline', 0)`,
			r.kind, r.path, r.body,
		); err != nil {
			t.Fatal(err)
		}
	}
	return &tools.ToolContext{DB: db}
}

func grepData(t *testing.T, res *tools.Result) (map[string]any, []grepMatch) {
	t.Helper()
	if !res.Success {
		t.Fatalf("grep failed: %s", res.Error)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data is not a map: %T", res.Data)
	}
	matches, ok := data["matches"].([]grepMatch)
	if !ok {
		t.Fatalf("matches is not []grepMatch: %T", data["matches"])
	}
	return data, matches
}

func TestArtifactGrep_BasicMatch(t *testing.T) {
	ctx := setupGrepDB(t, []grepRow{
		{"page", "/", "<h1>home</h1>\n<p>hello world</p>\n"},
		{"page", "/about", "<h1>about</h1>\n<p>nothing here</p>\n"},
		{"file", "js/app.js", "console.log('hello');\n"},
	})
	res, err := artifactGrep(ctx, "hello", "", "", "", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	data, matches := grepData(t, res)
	if got := data["match_count"]; got != 2 {
		t.Errorf("match_count = %v, want 2", got)
	}
	if got := data["files_hit"]; got != 2 {
		t.Errorf("files_hit = %v, want 2", got)
	}
	if len(matches) != 2 {
		t.Fatalf("matches len = %d, want 2", len(matches))
	}
	// Line numbers are 1-indexed.
	for _, m := range matches {
		if m.Line < 1 {
			t.Errorf("line %d should be 1-indexed", m.Line)
		}
		if m.Snippet == "" {
			t.Errorf("snippet should not be empty for match %+v", m)
		}
	}
}

func TestArtifactGrep_PathPrefix(t *testing.T) {
	ctx := setupGrepDB(t, []grepRow{
		{"file", "js/app.js", "needle\n"},
		{"file", "js/util.js", "needle\n"},
		{"file", "css/app.css", "needle\n"},
	})
	res, _ := artifactGrep(ctx, "needle", "", "js/", "", 0, 0, 0)
	data, _ := grepData(t, res)
	if got := data["match_count"]; got != 2 {
		t.Errorf("match_count = %v, want 2", got)
	}
	if got := data["files_scanned"]; got != 2 {
		t.Errorf("files_scanned = %v, want 2 (prefix should filter in SQL)", got)
	}
}

func TestArtifactGrep_PathGlob(t *testing.T) {
	ctx := setupGrepDB(t, []grepRow{
		{"page", "/checkout", "needle\n"},
		{"page", "/checkout/confirm", "needle\n"},
		{"page", "/checkout/payment", "needle\n"},
		{"page", "/about", "needle\n"},
	})
	res, _ := artifactGrep(ctx, "needle", "", "", "/checkout/*", 0, 0, 0)
	data, _ := grepData(t, res)
	if got := data["match_count"]; got != 2 {
		t.Errorf("match_count = %v, want 2 (only /checkout/{confirm,payment} match)", got)
	}
}

func TestArtifactGrep_PathGlobAndPrefix(t *testing.T) {
	ctx := setupGrepDB(t, []grepRow{
		{"file", "js/admin/users.js", "needle\n"},
		{"file", "js/admin/orders.js", "needle\n"},
		{"file", "js/public/home.js", "needle\n"},
		{"file", "css/admin/users.css", "needle\n"},
	})
	res, _ := artifactGrep(ctx, "needle", "", "js/", "js/admin/*", 0, 0, 0)
	data, _ := grepData(t, res)
	if got := data["match_count"]; got != 2 {
		t.Errorf("match_count = %v, want 2 (js/ prefix AND js/admin/* glob)", got)
	}
}

func TestArtifactGrep_InvalidGlob(t *testing.T) {
	ctx := setupGrepDB(t, []grepRow{{"page", "/", "needle\n"}})
	res, _ := artifactGrep(ctx, "needle", "", "", "[bad", 0, 0, 0)
	if res.Success {
		t.Fatal("expected failure on invalid glob")
	}
	if !strings.Contains(res.Error, "path_glob") {
		t.Errorf("error should mention path_glob: %v", res.Error)
	}
}

func TestArtifactGrep_ContextLines(t *testing.T) {
	body := "line1\nline2\nline3 needle\nline4\nline5\n"
	ctx := setupGrepDB(t, []grepRow{{"page", "/", body}})
	res, _ := artifactGrep(ctx, "needle", "", "", "", 0, 0, 2)
	data, matches := grepData(t, res)
	if got := data["context_lines"]; got != 2 {
		t.Errorf("context_lines echo = %v, want 2", got)
	}
	if len(matches) != 1 {
		t.Fatalf("matches len = %d, want 1", len(matches))
	}
	want := "line1\nline2\nline3 needle\nline4\nline5"
	if matches[0].Snippet != want {
		t.Errorf("snippet =\n%q\nwant\n%q", matches[0].Snippet, want)
	}
	if matches[0].Line != 3 {
		t.Errorf("line = %d, want 3 (matched-line stays the reported line)", matches[0].Line)
	}
}

func TestArtifactGrep_ContextLinesNearStart(t *testing.T) {
	body := "first line needle\nsecond\nthird\nfourth\n"
	ctx := setupGrepDB(t, []grepRow{{"page", "/", body}})
	res, _ := artifactGrep(ctx, "needle", "", "", "", 0, 0, 3)
	_, matches := grepData(t, res)
	// Match on line 1; window asks for 3 before (clipped to 0) and 3 after.
	if !strings.HasPrefix(matches[0].Snippet, "first line needle\nsecond\nthird\nfourth") {
		t.Errorf("snippet near start = %q", matches[0].Snippet)
	}
}

func TestArtifactGrep_ContextLinesClamp(t *testing.T) {
	ctx := setupGrepDB(t, []grepRow{{"page", "/", "needle\n"}})
	res, _ := artifactGrep(ctx, "needle", "", "", "", 0, 0, 999)
	data, _ := grepData(t, res)
	if got := data["context_lines"]; got != 5 {
		t.Errorf("context_lines = %v, want 5 (clamped from 999)", got)
	}
}

func TestArtifactGrep_MaxPerFile(t *testing.T) {
	body := strings.Repeat("needle\n", 20)
	ctx := setupGrepDB(t, []grepRow{{"page", "/", body}})
	res, _ := artifactGrep(ctx, "needle", "", "", "", 0, 3, 0)
	data, _ := grepData(t, res)
	if got := data["match_count"]; got != 3 {
		t.Errorf("match_count = %v, want 3 (max_per_file cap)", got)
	}
}

func TestArtifactGrep_Limit(t *testing.T) {
	rows := make([]grepRow, 0, 5)
	for i := 0; i < 5; i++ {
		rows = append(rows, grepRow{"page", "/p" + string(rune('0'+i)), "needle\n"})
	}
	ctx := setupGrepDB(t, rows)
	res, _ := artifactGrep(ctx, "needle", "", "", "", 2, 0, 0)
	data, _ := grepData(t, res)
	if got := data["match_count"]; got != 2 {
		t.Errorf("match_count = %v, want 2 (limit cap)", got)
	}
	if got := data["truncated"]; got != true {
		t.Errorf("truncated = %v, want true", got)
	}
}

func TestArtifactGrep_LegacySnippetShape(t *testing.T) {
	// With no context_lines arg the snippet must remain a single line (the
	// matched line), since callers may parse on that assumption.
	body := "line1\nline2 needle\nline3\n"
	ctx := setupGrepDB(t, []grepRow{{"page", "/", body}})
	res, _ := artifactGrep(ctx, "needle", "", "", "", 0, 0, 0)
	_, matches := grepData(t, res)
	if matches[0].Snippet != "line2 needle" {
		t.Errorf("default snippet = %q, want %q (must be single line)", matches[0].Snippet, "line2 needle")
	}
}

func TestArtifactGrep_LongLineTruncation(t *testing.T) {
	long := strings.Repeat("a", 300)
	body := long + " needle\n"
	ctx := setupGrepDB(t, []grepRow{{"page", "/", body}})
	res, _ := artifactGrep(ctx, "needle", "", "", "", 0, 0, 0)
	_, matches := grepData(t, res)
	if !strings.HasSuffix(matches[0].Snippet, "...") {
		t.Errorf("long line should be truncated with '...': %q", matches[0].Snippet)
	}
	if len(matches[0].Snippet) > 243 {
		t.Errorf("snippet length = %d, want <= 243 (240 + '...')", len(matches[0].Snippet))
	}
}
