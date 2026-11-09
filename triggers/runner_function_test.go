package triggers

import (
	"testing"

	"openfabrica/db"
	"openfabrica/events"
)

// TestRunFunctionTriggerAppliesWrites drives the run_function action: an event
// invokes a stored handler with the payload as req.body, and the handler's
// declared write lands in the DB.
func TestRunFunctionTriggerAppliesWrites(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	w := pdb.Writer()
	if _, err := w.Exec(`CREATE TABLE audit (id INTEGER PRIMARY KEY, who TEXT, amount INTEGER)`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	code := `function handler(req, ctx){ ctx.insert('audit', { who: req.body.email, amount: (req.body.total||0) * 2 }); return { ok: true }; }`
	if _, err := w.Exec(`INSERT INTO of_function_endpoints (path, code, methods) VALUES ('audit-fn', ?, 'POST')`, code); err != nil {
		t.Fatalf("seed function: %v", err)
	}

	r := &Runner{}
	ev := events.Event{ProjectID: 1, Type: "payment.completed", Payload: map[string]any{"email": "a@b.co", "total": 21}}

	if err := r.executeRunFunction(w, ev, map[string]any{"endpoint_path": "audit-fn"}); err != nil {
		t.Fatalf("executeRunFunction: %v", err)
	}
	var who string
	var amount int
	if err := pdb.Reader().QueryRow("SELECT who, amount FROM audit ORDER BY id DESC LIMIT 1").Scan(&who, &amount); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if who != "a@b.co" || amount != 42 {
		t.Fatalf("audit row wrong: who=%q amount=%d, want a@b.co / 42", who, amount)
	}

	// A missing endpoint surfaces a clear error (logged by the runner).
	if err := r.executeRunFunction(w, ev, map[string]any{"endpoint_path": "nope"}); err == nil {
		t.Fatal("expected an error for a missing function endpoint")
	}
}
