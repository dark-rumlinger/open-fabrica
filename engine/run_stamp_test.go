package engine

import (
	"testing"

	"openfabrica/db"
)

// createRun must stamp each run with the engine binary identity (BuildVersion /
// BuildBuiltAt) so a build export records which binary produced it - the field a
// post-mortem reads instead of diffing binary mtimes against source (a slow,
// error-prone forensic step).
func TestCreateRun_StampsEngineBuild(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	prevV, prevB := BuildVersion, BuildBuiltAt
	BuildVersion, BuildBuiltAt = "9.9.9-test", "2026-06-19T11:40:03Z"
	t.Cleanup(func() { BuildVersion, BuildBuiltAt = prevV, prevB })

	r, err := createRun(pdb, "build", "plan", "")
	if err != nil {
		t.Fatalf("createRun: %v", err)
	}

	var gotV, gotB string
	if err := pdb.QueryRow(
		`SELECT engine_version, engine_built_at FROM of_runs WHERE id = ?`, r.ID,
	).Scan(&gotV, &gotB); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotV != "9.9.9-test" || gotB != "2026-06-19T11:40:03Z" {
		t.Errorf("engine stamp = (%q, %q), want (9.9.9-test, 2026-06-19T11:40:03Z)", gotV, gotB)
	}
}
