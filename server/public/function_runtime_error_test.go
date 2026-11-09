package public

import (
	"context"
	"strings"
	"testing"

	"openfabrica/db"
)

// The response stays generic on purpose (a thrown message can name tables,
// secrets, or internal paths), so the cause has to survive somewhere inside the
// project. Both halves are the contract: the caller learns nothing, the project
// keeps the reason.
func TestFunctionError_RecordsCauseBehindGenericBody(t *testing.T) {
	pdb := newFunctionTestDB(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, karma INTEGER)`)
	// A where clause passed as a SQL string instead of an equality map: the
	// sandbox rejects the argument before any write is declared.
	createFunction(t, pdb, "vote", `function handler(req, ctx) {
	  ctx.update('users', { karma: 100 }, 'id = 1');
	  return { ok: true };
	}`, "POST", false, "")

	h := &Handler{deps: &Deps{}}
	rec := callFunction(t, h, pdb, "POST", "vote", `{}`, "")
	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "users") {
		t.Errorf("the public body must not leak handler detail: %s", rec.Body.String())
	}

	re, found := db.LastRuntimeError(pdb.ToolHandle(context.Background()), db.RuntimeErrorFunction, "vote")
	if !found {
		t.Fatal("the thrown cause must be recorded against the endpoint")
	}
	// The recorded message has to name the actual fault, since it is the only
	// thing standing between a 500 and rewriting the handler to find out why.
	if !strings.Contains(re.Message, "convert") {
		t.Errorf("recorded cause does not name the fault: %q", re.Message)
	}
}

// A handler that answers normally records nothing, so a later failure is never
// explained by a stale row.
func TestFunctionError_SuccessRecordsNothing(t *testing.T) {
	pdb := newFunctionTestDB(t)
	createFunction(t, pdb, "ping", `function handler(req, ctx) { return { ok: true }; }`, "POST", false, "")

	h := &Handler{deps: &Deps{}}
	if rec := callFunction(t, h, pdb, "POST", "ping", `{}`, ""); rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if _, found := db.LastRuntimeError(pdb.ToolHandle(context.Background()), db.RuntimeErrorFunction, "ping"); found {
		t.Error("a successful call must record no failure")
	}
}
