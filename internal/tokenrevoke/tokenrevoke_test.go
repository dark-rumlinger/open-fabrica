package tokenrevoke

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testDB(t *testing.T, withTable bool) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	d.SetMaxOpenConns(1) // an in-memory database lives on one connection
	t.Cleanup(func() { _ = d.Close() })
	if withTable {
		if _, err := d.Exec(`CREATE TABLE of_token_revocations (user_id INTEGER PRIMARY KEY, revoked_before INTEGER NOT NULL)`); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestRevokeEndsEarlierTokensOnly(t *testing.T) {
	d := testDB(t, true)
	s := New()
	at := time.UnixMilli(1_800_000_000_000)

	before := at.Add(-time.Second)
	after := at.Add(time.Millisecond)
	if s.Revoked(d, 1, 7, &before) {
		t.Fatal("a user nobody revoked has a revoked token")
	}
	if err := s.Revoke(d, 1, 7, at); err != nil {
		t.Fatal(err)
	}
	if !s.Revoked(d, 1, 7, &before) {
		t.Error("a token issued before the revocation still works")
	}
	if s.Revoked(d, 1, 7, &after) {
		t.Error("a token issued after the revocation was refused")
	}
	same := at
	if s.Revoked(d, 1, 7, &same) {
		t.Error("a token issued at the revocation instant was refused (it is not earlier)")
	}
	// Other users and other projects are untouched.
	if s.Revoked(d, 1, 8, &before) {
		t.Error("revoking one user revoked another")
	}
	other := testDB(t, true) // every project has its own database
	if s.Revoked(other, 2, 7, &before) {
		t.Error("revoking in one project revoked the same id in another")
	}
	// A token with no issue time cannot be shown to be newer.
	if !s.Revoked(d, 1, 7, nil) {
		t.Error("a token with no issue time passed for a revoked user")
	}
	if s.Revoked(d, 1, 8, nil) {
		t.Error("a token with no issue time failed for a user nobody revoked")
	}
}

func TestRevokeAgainMovesTheInstantForward(t *testing.T) {
	d := testDB(t, true)
	s := New()
	t0 := time.UnixMilli(1_800_000_000_000)
	t1 := t0.Add(time.Hour)
	if err := s.Revoke(d, 1, 7, t0); err != nil {
		t.Fatal(err)
	}
	between := t0.Add(time.Minute)
	if s.Revoked(d, 1, 7, &between) {
		t.Fatal("a token issued after the first revocation was refused")
	}
	if err := s.Revoke(d, 1, 7, t1); err != nil {
		t.Fatal(err)
	}
	if !s.Revoked(d, 1, 7, &between) {
		t.Error("the second revocation did not end the token issued between them")
	}
}

// A fresh process reads what an earlier one stored, and a database that has no
// table yet is simply "nothing revoked".
func TestRevocationsSurviveARestart(t *testing.T) {
	d := testDB(t, true)
	at := time.UnixMilli(1_800_000_000_000)
	if err := New().Revoke(d, 1, 7, at); err != nil {
		t.Fatal(err)
	}
	restarted := New()
	before := at.Add(-time.Second)
	if !restarted.Revoked(d, 1, 7, &before) {
		t.Error("a revocation was lost across a restart")
	}

	bare := testDB(t, false)
	if New().Revoked(bare, 1, 7, &before) {
		t.Error("a database without the table reported a revocation")
	}
}

func TestForgetDropsTheCachedProject(t *testing.T) {
	d := testDB(t, true)
	s := New()
	at := time.UnixMilli(1_800_000_000_000)
	_ = s.Revoke(d, 1, 7, at)
	s.Forget(1)
	if _, err := d.Exec(`DELETE FROM of_token_revocations`); err != nil {
		t.Fatal(err)
	}
	before := at.Add(-time.Second)
	if s.Revoked(d, 1, 7, &before) {
		t.Error("a forgotten project still answered from its old cache")
	}
}
