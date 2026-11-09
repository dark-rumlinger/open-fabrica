package snapshotbuild

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// wsSnapDB builds the of_ws_endpoints shape the snapshot reads. Only the columns
// the query names, since that is exactly the contract under test.
func wsSnapDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_ws_endpoints (
		path TEXT, requires_auth BOOLEAN DEFAULT 0, authoritative BOOLEAN DEFAULT 0,
		matchmaking BOOLEAN DEFAULT 0, tick_hz INTEGER DEFAULT 0, reducer TEXT DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func addWS(t *testing.T, db *sql.DB, path string, authoritative, matchmaking, tickHz int, reducer string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, authoritative, matchmaking, tick_hz, reducer)
		VALUES (?, ?, ?, ?, ?)`, path, authoritative, matchmaking, tickHz, reducer); err != nil {
		t.Fatal(err)
	}
}

func wsSnap(t *testing.T, db *sql.DB) *ProjectSnapshot {
	t.Helper()
	s, err := BuildProjectSnapshot(context.Background(), db, NewModuleParseCache())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSnapshotDistinguishesWSModes pins that the four WS models render as four
// different rows. They share a transport and a path shape, so before the mode rode
// along, a chat relay and a reducer-backed game were the same line of project_state
// and the model had nothing to tell them apart by.
func TestSnapshotDistinguishesWSModes(t *testing.T) {
	db := wsSnapDB(t)
	addWS(t, db, "chat", 0, 0, 0, "")
	addWS(t, db, "world", 0, 0, 20, "")
	addWS(t, db, "match", 0, 1, 0, "")
	addWS(t, db, "arena", 1, 0, 30, "function reduce(s,e,c){ return s; }")

	rendered := wsSnap(t, db).Render()
	for _, want := range []string{
		"`WS chat` [relay]",
		"`WS world` [tick, 20Hz]",
		"`WS match` [matchmaking]",
		"`WS arena` [authoritative, 30Hz, reducer attached]",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("project_state should render %q\ngot:\n%s", want, rendered)
		}
	}
}

// TestSnapshotNamesAMissingReducer is the case the whole change exists for. Create
// and set_reducer are two calls; an authoritative endpoint stuck between them
// accepts connections and applies nothing, and every other signal in the snapshot
// looks identical to a finished game.
func TestSnapshotNamesAMissingReducer(t *testing.T) {
	db := wsSnapDB(t)
	addWS(t, db, "arena", 1, 0, 30, "")

	rendered := wsSnap(t, db).Render()
	if !strings.Contains(rendered, "NO REDUCER") {
		t.Errorf("an authoritative endpoint with no reducer must say so\ngot:\n%s", rendered)
	}
	// A finding the model cannot act on is a finding it will not act on: the
	// reducer is a DB column, so the row has to name the tool that attaches one.
	if !strings.Contains(rendered, "set_reducer") {
		t.Errorf("the missing-reducer row must name the tool that fixes it\ngot:\n%s", rendered)
	}
}

// TestAttachingAReducerChangesTheFingerprint pins the fingerprint invariant that
// matters most. The fingerprint decides when the model is re-shown state; attaching
// a reducer is the single largest change a game undergoes, so it MUST change these
// bytes - otherwise it produces no delta and the model is never told.
func TestAttachingAReducerChangesTheFingerprint(t *testing.T) {
	fpOf := func(reducer string) string {
		db := wsSnapDB(t)
		addWS(t, db, "arena", 1, 0, 30, reducer)
		for _, d := range wsSnap(t, db).entityList() {
			if strings.HasPrefix(d.key, "endpoint:") {
				return d.desc
			}
		}
		t.Fatal("no endpoint entity in the snapshot")
		return ""
	}
	if fpOf("") == fpOf("function reduce(s,e,c){ return s; }") {
		t.Fatal("attaching a reducer must change the endpoint fingerprint, or set_reducer produces no delta")
	}
}

// TestWSModeChangeChangesTheFingerprint covers the same hazard for the mode: a
// relay promoted to authoritative is a different endpoint to build against, and a
// stale cached row would describe an API that no longer works.
func TestWSModeChangeChangesTheFingerprint(t *testing.T) {
	fpOf := func(authoritative, tickHz int) string {
		db := wsSnapDB(t)
		addWS(t, db, "room", authoritative, 0, tickHz, "")
		for _, d := range wsSnap(t, db).entityList() {
			if strings.HasPrefix(d.key, "endpoint:") {
				return d.desc
			}
		}
		t.Fatal("no endpoint entity in the snapshot")
		return ""
	}
	if fpOf(0, 0) == fpOf(1, 0) {
		t.Error("relay and authoritative must not share an endpoint fingerprint")
	}
	if fpOf(0, 0) == fpOf(0, 20) {
		t.Error("a tick rate change must change the endpoint fingerprint")
	}
}
