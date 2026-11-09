package validators

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"openfabrica/db"
)

func styleDriftDB(t *testing.T, pages map[string]string) *sql.DB {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.CloseAll)
	if _, err := pdb.ExecWrite(`INSERT INTO of_files (kind, path, body, metadata, scope) VALUES ('file', 'css/site.css', ':root{--c:#123}.btn{color:var(--c)}', '{}', 'global')`); err != nil {
		t.Fatal(err)
	}
	for path, body := range pages {
		if _, err := pdb.ExecWrite(`INSERT INTO of_files (kind, path, body, metadata, scope, status) VALUES ('page', ?, ?, '{}', 'page', 'published')`, path, body); err != nil {
			t.Fatal(err)
		}
	}
	return pdb.Reader()
}

// A page that restyles itself while its siblings use the shared sheet is flagged; a
// page with a few computed inline styles is not, and a one-page project is never judged.
func TestStyleDrift(t *testing.T) {
	var inline strings.Builder
	for i := 0; i < maxInlineStyles+3; i++ {
		inline.WriteString(`<div style="margin:4px"></div>`)
	}
	d := styleDriftDB(t, map[string]string{
		"/":      `<h1 class="title">Home</h1><div style="width:40%"></div>`,
		"/about": `<h1 class="title">About</h1>` + inline.String(),
	})
	got := styleDriftValidator{}.Validate(context.Background(), d, "", "")
	if len(got) != 1 || got[0].Page != "/about" || got[0].Category != "style_drift" || !got[0].Deterministic ||
		!strings.Contains(got[0].Message, "css/site.css") {
		t.Fatalf("want one finding on /about naming the shared sheet, got %+v", got)
	}

	one := styleDriftDB(t, map[string]string{"/": inline.String()})
	if got := (styleDriftValidator{}).Validate(context.Background(), one, "", ""); len(got) != 0 {
		t.Fatalf("a one-page project has nothing to drift from, got %+v", got)
	}
}
