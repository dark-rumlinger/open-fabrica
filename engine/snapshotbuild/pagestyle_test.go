package snapshotbuild

// A page's OWN stylesheet must be answerable from the snapshot.
//
// buildStyleContracts filters to scope='global' - correctly, a page one-off is
// not shared vocabulary - but global-ONLY meant a page-scope stylesheet was
// absent from the snapshot entirely, turning routine class lookups into
// stylesheet reads. So: same names-only surface, its OWN section, page-keyed.
// These tests pin both halves - the page class is answerable, and it never
// leaks into the shared design system.

import (
	"context"
	"strings"
	"testing"
)

func TestSnapshot_PageStyleContract(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('page','/','','<div id="app"></div>','text/html')`)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('file','site.global.css','global',':root{--c-bg:#fff}.btn{color:red}','text/css')`)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('file','game.css','page','.shake{}.dissolving{}.error{border:1px solid red}@keyframes pulse{}','text/css')`)
	mustExec(t, db, `INSERT INTO of_page_assets (page_path, file_path) VALUES ('/','game.css')`)

	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	if len(s.PageStyles) != 1 {
		t.Fatalf("expected 1 page-style contract, got %d", len(s.PageStyles))
	}
	ps := s.PageStyles[0]
	if ps.Page != "/" {
		t.Errorf("page-style contract keyed on %q, want \"/\"", ps.Page)
	}
	for _, want := range []string{"shake", "dissolving", "error"} {
		found := false
		for _, sc := range ps.Sheets {
			for _, c := range sc.Classes {
				if c == want {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("class %q missing from the page-style contract - the model must re-read the stylesheet to learn it exists, which is the read this section removes", want)
		}
	}

	out := s.Render()
	if !strings.Contains(out, "### Page-scope styles") {
		t.Fatal("Render() emits no Page-scope styles section")
	}
	// The critical separation: a page one-off must NOT land in the shared design
	// system, whose prose tells every page to reuse what it lists.
	shared := out[strings.Index(out, "### Design system (shared CSS)"):]
	if end := strings.Index(shared, "### Page-scope styles"); end >= 0 {
		shared = shared[:end]
	}
	if strings.Contains(shared, "shake") || strings.Contains(shared, "game.css") {
		t.Error("a PAGE-scope class leaked into \"Design system (shared CSS)\", whose prose says to reuse it on every page. " +
			"That is exactly what buildStyleContracts' scope='global' filter exists to prevent - keep page one-offs in their own section.")
	}
	// ...and the global vocabulary must still be in the shared section, not the page one.
	if !strings.Contains(shared, "btn") {
		t.Error("the global stylesheet's classes vanished from the shared design system")
	}
}

// A page with no page-scope stylesheet gets no entry: gated like the DOM contract, so
// this grows by pages-that-have-CSS, never by page count.
func TestSnapshot_PageStyleGatedAndFingerprinted(t *testing.T) {
	db := setupSnapshotDB(t)
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('page','/bare','','<p>hi</p>','text/html')`)
	s, err := BuildProjectSnapshot(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BuildProjectSnapshot: %v", err)
	}
	if len(s.PageStyles) != 0 {
		t.Fatalf("a page with no page-scope CSS must yield no contract, got %d", len(s.PageStyles))
	}
	if strings.Contains(s.Render(), "Page-scope styles") {
		t.Error("empty Page-scope styles section rendered; no section beats an empty one")
	}

	// Adding a class must move the entity fingerprint, or the delta never re-renders
	// the contract and the model keeps wiring against a stale class list.
	mustExec(t, db, `INSERT INTO of_files (kind, path, scope, body, content_type) VALUES ('file','p.css','page','.a{}','text/css')`)
	mustExec(t, db, `INSERT INTO of_page_assets (page_path, file_path) VALUES ('/bare','p.css')`)
	s1, _ := BuildProjectSnapshot(context.Background(), db, nil)
	before := s1.Entities["pagestyle:/bare"]
	if before == "" {
		t.Fatal("no pagestyle entity fingerprint - the delta renderer cannot track this section")
	}

	mustExec(t, db, `UPDATE of_files SET body = '.a{}.b{}' WHERE path = 'p.css'`)
	s2, _ := BuildProjectSnapshot(context.Background(), db, nil)
	if s2.Entities["pagestyle:/bare"] == before {
		t.Error("adding a class left the pagestyle fingerprint unchanged - the snapshot would never re-render it")
	}

	// A value-only edit must NOT churn the fingerprint (names-only contract).
	mustExec(t, db, `UPDATE of_files SET body = '.a{color:red}.b{color:blue}' WHERE path = 'p.css'`)
	s3, _ := BuildProjectSnapshot(context.Background(), db, nil)
	if s3.Entities["pagestyle:/bare"] != s2.Entities["pagestyle:/bare"] {
		t.Error("a value-only CSS edit moved the pagestyle fingerprint - it must track NAMES only, or every restyle re-bills the prompt cache")
	}
}
