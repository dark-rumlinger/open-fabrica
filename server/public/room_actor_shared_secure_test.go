package public

import (
	"encoding/json"
	"testing"

	"openfabrica/db"
)

// readShared must never inject PASSWORD/ENCRYPTED columns into event.shared. With
// no explicit shared_columns ("all columns"), it expands "*" to the non-secure
// column list so a reducer (and hence view() output sent to clients) can't leak
// password hashes or ciphertext from the shared table.
func TestReadSharedExcludesSecureColumns(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	if _, err := pdb.Writer().Exec(
		`CREATE TABLE accounts (id INTEGER PRIMARY KEY, balance INTEGER, password TEXT);
		 INSERT INTO accounts (id, balance, password) VALUES (1, 500, 'secret-hash');`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Register the table so LoadSecureColumns/LoadTableSchema know its shape and that
	// `password` is a PASSWORD column.
	if _, err := pdb.Writer().Exec(
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES (?, ?, ?)`,
		"accounts",
		`{"id":"INTEGER","balance":"INTEGER","password":"PASSWORD"}`,
		`{"password":"hash"}`); err != nil {
		t.Fatalf("register table: %v", err)
	}

	const colsReducer = `
	function reduce(state, event, ctx) {
	  if (event.type === '@init') return { cols: [] };
	  var row = event.shared && event.shared[0];
	  return { cols: row ? Object.keys(row).sort() : [] };
	}`
	a := newWritesActor(t, pdb, colsReducer)
	a.sharedTable = "accounts"
	a.sharedColumns = nil // "all columns" -> must exclude secure ones

	a.apply(map[string]any{"type": "@init"}, "")
	a.apply(map[string]any{"type": "peek"}, "")

	var m struct {
		Cols []string `json:"cols"`
	}
	if err := json.Unmarshal(a.state, &m); err != nil {
		t.Fatalf("decode state %s: %v", a.state, err)
	}
	has := func(c string) bool {
		for _, x := range m.Cols {
			if x == c {
				return true
			}
		}
		return false
	}
	if has("password") {
		t.Errorf("readShared leaked the PASSWORD column into event.shared: %v", m.Cols)
	}
	if !has("balance") || !has("id") {
		t.Errorf("readShared should still expose non-secure columns, got %v", m.Cols)
	}
}

// The secure-column exclusion must fail CLOSED. If the projection can't be
// enumerated, readShared cannot know which columns are secret, so it must inject
// nothing rather than fall back to "*". Losing shared context degrades a reducer;
// leaking password hashes to every client in the room does not degrade, it
// breaches. Note LoadSecureColumns returns (empty, nil) for an unregistered
// table - an error here means a real database failure (lock contention,
// corruption), so failing closed costs nothing on the happy path.
func TestReadSharedFailsClosedWhenSecureColumnsUnreadable(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	if _, err := pdb.Writer().Exec(
		`CREATE TABLE accounts (id INTEGER PRIMARY KEY, balance INTEGER, password TEXT);
		 INSERT INTO accounts (id, balance, password) VALUES (1, 500, 'secret-hash');`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Force LoadSecureColumns to fail the way a corrupt/unavailable database
	// would: the projection it reads is gone, so the query errors rather than
	// reporting "this table has no secure columns".
	if _, err := pdb.Writer().Exec(`DROP TABLE of_dynamic_tables`); err != nil {
		t.Fatalf("drop registry: %v", err)
	}

	const colsReducer = `
	function reduce(state, event, ctx) {
	  if (event.type === '@init') return { cols: [] };
	  var row = event.shared && event.shared[0];
	  return { cols: row ? Object.keys(row).sort() : [] };
	}`
	a := newWritesActor(t, pdb, colsReducer)
	a.sharedTable = "accounts"
	a.sharedColumns = nil

	a.apply(map[string]any{"type": "@init"}, "")
	a.apply(map[string]any{"type": "peek"}, "")

	var m struct {
		Cols []string `json:"cols"`
	}
	if err := json.Unmarshal(a.state, &m); err != nil {
		t.Fatalf("decode state %s: %v", a.state, err)
	}
	for _, c := range m.Cols {
		if c == "password" {
			t.Fatalf("readShared fell back to SELECT * when the secure-column projection was unreadable, leaking the PASSWORD column: %v", m.Cols)
		}
	}
}
