package validators

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// ownershipDB is the RLS schema plus the runs table and the auth columns the
// endpoint auth_model resolver reads, with a plan that locks one data-ownership
// decision on the notes endpoint.
func ownershipDB(t *testing.T, lockedModel string) *sql.DB {
	t.Helper()
	db := setupRLSDB(t)
	for _, s := range []string{
		`ALTER TABLE of_api_endpoints ADD COLUMN required_role TEXT`,
		`ALTER TABLE of_api_endpoints ADD COLUMN owner_scope TEXT`,
		`CREATE TABLE of_runs (id INTEGER PRIMARY KEY, plan_json TEXT, started_at TEXT DEFAULT (datetime('now')))`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	plan := Plan{Goal: "notes", Decisions: []PlanDecision{{
		Topic: "data ownership", Choice: lockedModel,
		Check: &DecisionCheck{Artifact: "endpoint", ID: "notes", Field: "auth_model", Op: "eq", Value: lockedModel},
	}}}
	pj, _ := json.Marshal(plan)
	if _, err := db.Exec(`INSERT INTO of_runs (plan_json) VALUES (?)`, string(pj)); err != nil {
		t.Fatal(err)
	}
	return db
}

// The plan locked notes as owner-scoped but the endpoint was built merely
// authenticated: the conformance validator names the locked model and the fix.
func TestDecisionConformance_EndpointOwnershipMismatch(t *testing.T) {
	db := ownershipDB(t, "owner")
	addRLSEndpoint(t, db, "notes", "notes", 1, 0, 0, "")
	issues := decisionConformanceValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 || !strings.Contains(issues[0].Message, "auth_model") || !strings.Contains(issues[0].Message, "manage_api action=update") {
		t.Fatalf("an owner decision built as authenticated must be flagged with the fix, got %+v", issues)
	}
}

func TestDecisionConformance_EndpointOwnershipMatches(t *testing.T) {
	db := ownershipDB(t, "owner")
	addRLSEndpoint(t, db, "notes", "notes", 1, 0, 0, "user_id")
	if issues := (decisionConformanceValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("an owner-scoped endpoint conforms to an owner decision, got %+v", issues)
	}
}

// A table the plan deliberately locked public is not an RLS leak: rls_scope defers to
// the decision (the conformance validator owns the comparison).
func TestRLSScope_DefersToLockedOwnershipDecision(t *testing.T) {
	db := ownershipDB(t, "public")
	addRLSEndpoint(t, db, "notes", "notes", 1, 0, 0, "")
	if issues := (rlsScopeValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("rls_scope must defer to a locked ownership decision, got %+v", issues)
	}
}

// A table anyone may read but only staff may write is public data ownership: the
// derived model reads "role", yet the locked "public" decision is satisfied.
func TestDecisionConformance_PublicReadRoleWriteIsPublic(t *testing.T) {
	db := ownershipDB(t, "public")
	addRLSEndpoint(t, db, "notes", "notes", 1, 1, 0, "")
	if _, err := db.Exec(`UPDATE of_api_endpoints SET required_role = 'staff' WHERE path = 'notes'`); err != nil {
		t.Fatal(err)
	}
	if issues := (decisionConformanceValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("a public-read, role-write endpoint conforms to a public decision, got %+v", issues)
	}
	// Without public_read the same endpoint is not public.
	if _, err := db.Exec(`UPDATE of_api_endpoints SET public_read = 0 WHERE path = 'notes'`); err != nil {
		t.Fatal(err)
	}
	if issues := (decisionConformanceValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 1 {
		t.Fatalf("a role-gated endpoint with no public read is not public, got %+v", issues)
	}
}
