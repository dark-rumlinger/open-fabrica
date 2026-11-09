package content

import (
	"testing"
	"time"

	"openfabrica/internal/dbtest"
	"openfabrica/tools"
)

// manage_seo validate audits every saved page. It must collect the page list
// before looking up each page's meta: ctx.DB is the single-connection writer
// pool, so a per-page lookup issued while the page cursor is still open waits on
// a connection that cannot free.
//
// RequirePagesFirst only admits validate once a page exists, so the loop body
// always runs - a project with pages is the only state this action ever sees.
func TestSEOValidate_NoDeadlockOnRealWriterPool(t *testing.T) {
	pdb := dbtest.ProjectWriterDB(t)
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_files (path, kind, is_deleted, metadata) VALUES ('/', 'page', 0, '{"title":"Home"}')`,
	); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	ctx := &tools.ToolContext{DB: pdb.Writer()}

	var res *tools.Result
	var err error
	dbtest.RunWithDeadlineGuard(t, 10*time.Second, "manage_seo validate", func() {
		res, err = seoValidate(ctx)
	})
	if err != nil {
		t.Fatalf("seoValidate: %v", err)
	}
	if !res.Success {
		t.Fatalf("seoValidate failed: %s", res.Error)
	}

	// The seeded page has no of_seo_meta row, so the audit must report it.
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected Data type %T", res.Data)
	}
	if got := data["total_issues"]; got != 1 {
		t.Errorf("total_issues = %v, want 1", got)
	}
}

// Multiple pages force repeated per-page lookups after the cursor is drained,
// which is where a partially-applied fix (draining but leaving the cursor open)
// would still wedge.
func TestSEOValidate_ManyPagesNoDeadlock(t *testing.T) {
	pdb := dbtest.ProjectWriterDB(t)
	for _, p := range []string{"/", "/about", "/pricing", "/contact"} {
		if _, err := pdb.ExecWrite(
			`INSERT INTO of_files (path, kind, is_deleted, metadata) VALUES (?, 'page', 0, '{}')`, p,
		); err != nil {
			t.Fatalf("seed page %s: %v", p, err)
		}
	}
	// One page fully specified: it must NOT be reported, proving the per-page
	// lookups still run against the right rows after the restructure.
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_seo_meta (page_path, title, description, og_title, og_description)
		 VALUES ('/about', 'About', 'About us', 'About', 'About us')`,
	); err != nil {
		t.Fatalf("seed meta: %v", err)
	}
	ctx := &tools.ToolContext{DB: pdb.Writer()}

	var res *tools.Result
	var err error
	dbtest.RunWithDeadlineGuard(t, 10*time.Second, "manage_seo validate", func() {
		res, err = seoValidate(ctx)
	})
	if err != nil {
		t.Fatalf("seoValidate: %v", err)
	}
	data := res.Data.(map[string]any)
	if got := data["total_issues"]; got != 3 {
		t.Errorf("total_issues = %v, want 3 (only /about is complete)", got)
	}
}
