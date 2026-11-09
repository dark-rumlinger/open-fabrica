package project

import (
	"testing"

	"openfabrica/db"

	_ "modernc.org/sqlite"
)

// Every query in artifactStoreQueries must run against the real project schema.
// The deliverable guard treats a query error as a match (fails open, so a
// transient DB fault never blocks a build), which means a query naming a table or
// column that does not exist disables the guard silently for every task - it would
// pass its own unit tests and report nothing. Running the list against a
// fully-migrated database is what makes that impossible: a typo fails here instead
// of going unnoticed in production.
func TestArtifactStoreQueriesRunAgainstRealSchema(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}

	for _, q := range artifactStoreQueries {
		rows, err := pdb.Reader().Query(q)
		if err != nil {
			t.Errorf("artifact store query does not run against the project schema:\n  %s\n  %v", q, err)
			continue
		}
		_ = rows.Err()
		rows.Close()
	}
}
