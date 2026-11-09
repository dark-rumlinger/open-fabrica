package engine

import (
	"strings"
	"testing"

	"openfabrica/db"
)

// The first live wake after a build that shipped flagged sees what is open: the
// failing checks (errors first, flow findings left out) and the unbuilt tasks.
func TestKnownIssuesBlock(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	if got := knownIssuesBlock(pdb.Reader()); got != "" {
		t.Fatalf("a clean project renders no block, got %q", got)
	}

	sweep := `{"issues":[` +
		`{"severity":"warning","category":"webfont","page":"/","file":"","message":"Font Fredoka is never loaded."},` +
		`{"severity":"error","category":"flow","page":"","file":"","message":"1 of 2 flows failed"},` +
		`{"severity":"error","category":"runtime","page":"/staff","file":"","message":"TypeError: x is undefined"}]}`
	for _, q := range []string{
		`INSERT INTO of_run_log (run_id, kind, payload) VALUES ('run_1', 'validation', '{"issues":[]}')`,
		`INSERT INTO of_run_log (run_id, kind, payload) VALUES ('run_1', 'validation', '` + sweep + `')`,
		`INSERT INTO of_tasks (id, title, intent, status, position) VALUES ('menu-management', 'Menu management panel', '', 'pending', 1)`,
		`INSERT INTO of_tasks (id, title, intent, status, position) VALUES ('order-page', 'Order page', '', 'done', 0)`,
	} {
		if _, err := pdb.ExecWrite(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	got := knownIssuesBlock(pdb.Reader())
	for _, want := range []string{"<known_issues>", "[error runtime] /staff: TypeError", "[warning webfont] /", "`menu-management` Menu management panel"} {
		if !strings.Contains(got, want) {
			t.Errorf("block missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "flows failed") || strings.Contains(got, "Order page") {
		t.Errorf("flow findings and done tasks stay out:\n%s", got)
	}
	if strings.Index(got, "runtime") > strings.Index(got, "webfont") {
		t.Errorf("errors list before warnings:\n%s", got)
	}
}
