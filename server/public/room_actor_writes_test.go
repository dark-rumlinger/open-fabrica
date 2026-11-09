package public

import (
	"encoding/json"
	"testing"

	"openfabrica/db"
	"openfabrica/internal/roomactor"
)

// newWritesActor builds a bare room actor (no hub) wired to a real project DB,
// enough to exercise apply()/applyWrites() directly. broadcastViews is a no-op
// because order/observers stay empty, so hub can be nil.
func newWritesActor(t *testing.T, pdb *db.ProjectDB, src string) *roomActor {
	t.Helper()
	red, err := roomactor.Compile(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return &roomActor{
		key: "k", room: "r", endpoint: "game", tenantID: 0,
		reducer: red, pdb: pdb,
		players:   map[string]json.RawMessage{},
		uids:      map[string]int{},
		observers: map[string]bool{},
		lastView:  map[string]string{},
	}
}

func bankroll(t *testing.T, pdb *db.ProjectDB, id int) int {
	t.Helper()
	var n int
	if err := pdb.Reader().QueryRow("SELECT bankroll FROM players WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatalf("read bankroll: %v", err)
	}
	return n
}

const settleReducer = `
function reduce(state, event, ctx) {
  if (event.type === '@init') return { round: 0 };
  if (event.type === 'settle') {
    ctx.update('players', { bankroll: { '$increment': 150 } }, { id: 7 });
    ctx.insert('hand_history', { player_id: 7, net: 150 });
    return { round: (state.round||0) + 1 };
  }
  if (event.type === '@tick') {
    if (event.dt >= 1) ctx.update('players', { bankroll: { '$increment': 999 } }, { id: 7 });
    return state;
  }
  return state;
}`

func TestReducerSettlementWritesLandInDB(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	if _, err := pdb.Writer().Exec(
		`CREATE TABLE players (id INTEGER PRIMARY KEY, bankroll INTEGER DEFAULT 0);
		 CREATE TABLE hand_history (id INTEGER PRIMARY KEY, player_id INTEGER, net INTEGER);
		 INSERT INTO players (id, bankroll) VALUES (7, 1000);`); err != nil {
		t.Fatalf("seed schema: %v", err)
	}

	a := newWritesActor(t, pdb, settleReducer)
	a.apply(map[string]any{"type": "@init"}, "")

	// A real (non-tick) event applies the declared writes.
	a.apply(map[string]any{"type": "settle"}, "")
	if got := bankroll(t, pdb, 7); got != 1150 {
		t.Fatalf("after settle bankroll = %d, want 1150", got)
	}
	var rows int
	if err := pdb.Reader().QueryRow("SELECT COUNT(*) FROM hand_history WHERE player_id = 7 AND net = 150").Scan(&rows); err != nil {
		t.Fatalf("count hand_history: %v", err)
	}
	if rows != 1 {
		t.Fatalf("hand_history rows = %d, want 1", rows)
	}

	// A @tick that declares nothing writes nothing.
	a.apply(map[string]any{"type": "@tick", "dt": 0.033}, "")
	if got := bankroll(t, pdb, 7); got != 1150 {
		t.Fatalf("after a plain @tick bankroll = %d, want 1150", got)
	}
	// A @tick that settles (declares a write) lands it: the contract recommends
	// settling a countdown in @tick.
	a.apply(map[string]any{"type": "@tick", "dt": 1.0}, "")
	if got := bankroll(t, pdb, 7); got != 2149 {
		t.Fatalf("after a settling @tick bankroll = %d, want 2149", got)
	}
}

func TestReadSeedFromDB(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	if _, err := pdb.Writer().Exec(
		`CREATE TABLE players (id INTEGER PRIMARY KEY, bankroll INTEGER DEFAULT 0, role TEXT);
		 INSERT INTO players (id, bankroll, role) VALUES (7, 2500, 'vip');`); err != nil {
		t.Fatalf("seed schema: %v", err)
	}

	a := newWritesActor(t, pdb, settleReducer)
	a.seedTable = "players"
	a.seedKeyCol = "id"
	a.seedColumns = []string{"bankroll", "role"}

	// An authenticated joiner: the seed comes from the DB keyed on the uid, so a
	// lie in client meta is irrelevant, readSeed never looks at meta.
	a.uids["c1"] = 7
	seed := a.readSeed("c1")
	if seed == nil {
		t.Fatal("expected a seed for a known authenticated player")
	}
	// SQLite via modernc returns INTEGER as int64.
	if br, _ := seed["bankroll"].(int64); br != 2500 {
		t.Fatalf("seed bankroll = %v, want 2500 (DB truth, not client meta)", seed["bankroll"])
	}
	if seed["role"] != "vip" {
		t.Fatalf("seed role = %v, want 'vip'", seed["role"])
	}

	// Anonymous connection (uid 0) cannot seed, nothing to key on.
	a.uids["c2"] = 0
	if s := a.readSeed("c2"); s != nil {
		t.Fatalf("anonymous connection should not seed, got %+v", s)
	}

	// Unknown player (no row) seeds nothing; the reducer applies its own default.
	a.uids["c3"] = 999
	if s := a.readSeed("c3"); s != nil {
		t.Fatalf("unknown player should not seed, got %+v", s)
	}
}

func TestReducerSecureColumnWriteRejected(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	if _, err := pdb.Writer().Exec(
		`CREATE TABLE accounts (id INTEGER PRIMARY KEY, password TEXT, bankroll INTEGER DEFAULT 0);
		 INSERT INTO accounts (id, password, bankroll) VALUES (7, 'hashed', 1000);
		 INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('accounts', '{}', '{"password":"hash"}');`); err != nil {
		t.Fatalf("seed schema: %v", err)
	}

	// Reducer tries to overwrite the PASSWORD column and also bump bankroll in one
	// event. The secure write is rejected (whole write skipped); the safe write is
	// unaffected.
	a := newWritesActor(t, pdb, `
function reduce(state, event, ctx) {
  if (event.type === '@init') return {};
  if (event.type === 'evt') {
    ctx.update('accounts', { password: 'attacker' }, { id: 7 });
    ctx.update('accounts', { bankroll: { '$increment': 5 } }, { id: 7 });
    return state;
  }
  return state;
}`)
	a.apply(map[string]any{"type": "@init"}, "")
	a.apply(map[string]any{"type": "evt"}, "")

	var pw string
	var bal int
	if err := pdb.Reader().QueryRow("SELECT password, bankroll FROM accounts WHERE id = 7").Scan(&pw, &bal); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if pw != "hashed" {
		t.Fatalf("password = %q, want unchanged 'hashed' (secure-column write must be rejected)", pw)
	}
	if bal != 1005 {
		t.Fatalf("bankroll = %d, want 1005 (the non-secure write should still apply)", bal)
	}
}
