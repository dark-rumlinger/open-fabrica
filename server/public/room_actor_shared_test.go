package public

import (
	"encoding/json"
	"testing"

	"openfabrica/db"
)

// sharedPoolReducer echoes the shared jackpot pool into state, or -1 when no
// event.shared was injected.
const sharedPoolReducer = `
function reduce(state, event, ctx) {
  if (event.type === '@init') return { pool: 0 };
  return { pool: (event.shared && event.shared[0]) ? event.shared[0].pool : -1 };
}`

func newSharedActor(t *testing.T) (*roomActor, *db.ProjectDB) {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	if _, err := pdb.Writer().Exec(
		`CREATE TABLE jackpot (id INTEGER PRIMARY KEY, pool INTEGER);
		 INSERT INTO jackpot (id, pool) VALUES (1, 5000);`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newWritesActor(t, pdb, sharedPoolReducer)
	a.sharedTable = "jackpot"
	a.sharedColumns = []string{"pool"}
	return a, pdb
}

func statePool(t *testing.T, a *roomActor) float64 {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(a.state, &m); err != nil {
		t.Fatalf("decode state %s: %v", a.state, err)
	}
	p, _ := m["pool"].(float64)
	return p
}

// TestReducerReadsSharedState: a reducer sees the endpoint's shared table rows as
// event.shared on a normal (non-tick) event - cross-room/global state mid-reduce.
func TestReducerReadsSharedState(t *testing.T) {
	a, _ := newSharedActor(t)
	a.apply(map[string]any{"type": "@init"}, "")
	a.apply(map[string]any{"type": "peek"}, "")
	if got := statePool(t, a); got != 5000 {
		t.Fatalf("reducer did not read shared pool via event.shared: pool=%v state=%s", got, a.state)
	}
}

// TestReducerSharedSkippedOnTick: event.shared is NOT injected on @tick (a sim
// would hammer the DB), so the reducer sees no shared context there.
func TestReducerSharedSkippedOnTick(t *testing.T) {
	a, _ := newSharedActor(t)
	a.apply(map[string]any{"type": "@init"}, "")
	a.apply(map[string]any{"type": "@tick", "dt": 33}, "")
	if got := statePool(t, a); got != -1 {
		t.Fatalf("event.shared must be skipped on @tick, but reducer saw pool=%v", got)
	}
}
