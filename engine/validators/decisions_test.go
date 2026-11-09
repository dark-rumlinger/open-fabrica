package validators

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"
)

// setupDecisionDB builds a project DB with of_runs (seeded with the plan) and an
// empty of_ws_endpoints, so a test can insert the "built" endpoint and run the
// conformance validator over the round-tripped plan JSON.
func setupDecisionDB(t *testing.T, plan Plan) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1) // keep the single in-memory DB alive across queries
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_runs (id INTEGER PRIMARY KEY, plan_json TEXT, started_at TEXT DEFAULT (datetime('now')))`); err != nil {
		t.Fatalf("create of_runs: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE of_ws_endpoints (
		path TEXT PRIMARY KEY, tick_hz INTEGER DEFAULT 0, authoritative INTEGER DEFAULT 0,
		reducer TEXT DEFAULT '', e2ee INTEGER DEFAULT 0, matchmaking INTEGER DEFAULT 0,
		room_gated INTEGER DEFAULT 0, ephemeral INTEGER DEFAULT 0)`); err != nil {
		t.Fatalf("create of_ws_endpoints: %v", err)
	}
	pj, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_runs (plan_json) VALUES (?)`, string(pj)); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return db
}

func tickDecisionPlan(check *DecisionCheck) Plan {
	return Plan{
		Goal:      "collaborative canvas",
		Tasks:     []PlanTaskSpec{{Title: "Realtime tick", Tools: []string{"manage_tick"}}},
		Decisions: []PlanDecision{{Topic: "realtime model", Choice: "tick", Rationale: "live cursors", Check: check}},
	}
}

func runDecisionValidator(t *testing.T, db *sql.DB) []Issue {
	t.Helper()
	return decisionConformanceValidator{}.Validate(context.Background(), db, "", "")
}

// The canvas failure mode: plan locked tick mode (tick_hz>0) but the endpoint was
// built as a bare relay (tick_hz=0). The validator must catch it.
func TestDecisionConformance_FlagsRelayBuiltForTickDecision(t *testing.T) {
	db := setupDecisionDB(t, tickDecisionPlan(&DecisionCheck{Artifact: "websocket", ID: "canvas", Field: "tick_hz", Op: "gt", Value: 0}))
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, tick_hz) VALUES ('canvas', 0)`); err != nil {
		t.Fatal(err)
	}
	issues := runDecisionValidator(t, db)
	if len(issues) != 1 {
		t.Fatalf("expected 1 decision_conformance issue, got %d: %+v", len(issues), issues)
	}
	if issues[0].Category != "decision_conformance" || issues[0].Severity != "error" || !issues[0].Deterministic {
		t.Errorf("unexpected issue shape: %+v", issues[0])
	}
}

func TestDecisionConformance_PassesWhenBuiltMatches(t *testing.T) {
	db := setupDecisionDB(t, tickDecisionPlan(&DecisionCheck{Artifact: "websocket", ID: "canvas", Field: "tick_hz", Op: "gt", Value: 0}))
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, tick_hz) VALUES ('canvas', 30)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 0 {
		t.Fatalf("matched build must not flag, got %d: %+v", len(issues), issues)
	}
}

// Conservative-by-design: an unresolvable check (unknown artifact type) must SKIP,
// never fabricate a finding that traps the fix loop. A missing endpoint is the one
// exception, and only when NO endpoint carries the locked property: that is the
// locked model never being built, which must not pass silently.
func TestDecisionConformance_SkipsUnresolvable(t *testing.T) {
	// Missing endpoint row, but a sibling under another id carries the property:
	// the planner only guessed the id, so this must not flag.
	db := setupDecisionDB(t, tickDecisionPlan(&DecisionCheck{Artifact: "websocket", ID: "missing", Field: "tick_hz", Op: "gt", Value: 0}))
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, tick_hz) VALUES ('arena', 30)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 0 {
		t.Fatalf("missing endpoint with a conforming sibling must skip, got %d: %+v", len(issues), issues)
	}
	// Unknown artifact type.
	db2 := setupDecisionDB(t, tickDecisionPlan(&DecisionCheck{Artifact: "widget", ID: "x", Field: "y", Op: "eq", Value: 1}))
	if issues := runDecisionValidator(t, db2); len(issues) != 0 {
		t.Fatalf("unknown artifact must skip, got %d: %+v", len(issues), issues)
	}
}

func TestDecisionConformance_NoCheckNoIssue(t *testing.T) {
	db := setupDecisionDB(t, tickDecisionPlan(nil))
	if issues := runDecisionValidator(t, db); len(issues) != 0 {
		t.Fatalf("a decision without a check must not flag, got %d: %+v", len(issues), issues)
	}
}

func paymentsDecisionPlan(check *DecisionCheck) Plan {
	return Plan{
		Goal:      "sell a digital product",
		Tasks:     []PlanTaskSpec{{Title: "Payments", Tools: []string{"manage_payments"}}},
		Decisions: []PlanDecision{{Topic: "fulfillment authority", Choice: "webhook-trigger", Rationale: "never grant on the redirect", Check: check}},
	}
}

// Payments fulfilment must be a server-side trigger on payment.completed. A build
// that grants entitlement on the client redirect has no such trigger, so the
// trigger-artifact check (count > 0) bounces a fix-pass.
func TestDecisionConformance_FlagsMissingPaymentTrigger(t *testing.T) {
	db := setupDecisionDB(t, paymentsDecisionPlan(&DecisionCheck{Artifact: "trigger", ID: "payment.completed", Field: "count", Op: "gt", Value: 0}))
	if _, err := db.Exec(`CREATE TABLE of_triggers (id INTEGER PRIMARY KEY, event_type TEXT, is_enabled INTEGER DEFAULT 1)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 1 {
		t.Fatalf("a missing payment.completed trigger must flag, got %d: %+v", len(issues), issues)
	}
}

func TestDecisionConformance_PassesWithPaymentTrigger(t *testing.T) {
	db := setupDecisionDB(t, paymentsDecisionPlan(&DecisionCheck{Artifact: "trigger", ID: "payment.completed", Field: "count", Op: "gt", Value: 0}))
	if _, err := db.Exec(`CREATE TABLE of_triggers (id INTEGER PRIMARY KEY, event_type TEXT, is_enabled INTEGER DEFAULT 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO of_triggers (event_type, is_enabled) VALUES ('payment.completed', 1)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 0 {
		t.Fatalf("a built payment.completed trigger must satisfy the locked decision, got %d: %+v", len(issues), issues)
	}
}

// The authoritative case also works (string/int columns), proving the framework
// is generic across fields, not tick_hz-specific.
func TestDecisionConformance_AuthoritativeField(t *testing.T) {
	plan := Plan{
		Decisions: []PlanDecision{{Topic: "realtime model", Choice: "authoritative",
			Check: &DecisionCheck{Artifact: "websocket", ID: "game", Field: "authoritative", Op: "eq", Value: 1}}},
	}
	db := setupDecisionDB(t, plan)
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, authoritative) VALUES ('game', 0)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 1 {
		t.Fatalf("authoritative-decision built as non-authoritative must flag, got %d", len(issues))
	}
}

// Regression for a false alarm on boolean-valued decisions: a boolean-valued locked decision
// (matchmaking = false) must MATCH an endpoint whose matchmaking column reads back
// as integer 0. Before toFloat handled bool, looseEqual(0, false) fell through to a
// string compare ("0" != "false") and fired a bogus decision_conformance error on a
// build that was in fact correct.
func TestDecisionConformance_BoolFalseMatchesIntZero(t *testing.T) {
	plan := Plan{
		Decisions: []PlanDecision{{Topic: "multiplayer room model", Choice: "global-room",
			Check: &DecisionCheck{Artifact: "websocket", ID: "space", Field: "matchmaking", Op: "eq", Value: false}}},
	}
	db := setupDecisionDB(t, plan)
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, tick_hz, matchmaking) VALUES ('space', 20, 0)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 0 {
		t.Fatalf("matchmaking=false must match stored 0 (no false alarm), got %d: %+v", len(issues), issues)
	}
}

// The bool normalization must not over-match: a locked matchmaking = true against a
// stored 0 must still flag, proving false->0 / true->1 (not "every bool passes").
func TestDecisionConformance_BoolTrueFlagsIntZero(t *testing.T) {
	plan := Plan{
		Decisions: []PlanDecision{{Topic: "multiplayer room model", Choice: "matchmade",
			Check: &DecisionCheck{Artifact: "websocket", ID: "space", Field: "matchmaking", Op: "eq", Value: true}}},
	}
	db := setupDecisionDB(t, plan)
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, matchmaking) VALUES ('space', 0)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 1 {
		t.Fatalf("matchmaking=true built as 0 must flag, got %d", len(issues))
	}
}

func authoritativeDecisionPlan(check *DecisionCheck) Plan {
	return Plan{
		Goal:      "shared voxel world",
		Tasks:     []PlanTaskSpec{{Title: "Realtime authoritative", Tools: []string{"manage_authoritative"}}},
		Decisions: []PlanDecision{{Topic: "realtime model", Choice: "authoritative", Rationale: "server owns block edits", Check: check}},
	}
}

// A HYBRID realtime split puts positions on a tick endpoint (`game`,
// non-authoritative) and the authoritative reducer on the sibling relay endpoint
// (`game-events`). The locked check pins `game`, but the capability genuinely
// lives next door, so the system capability is satisfied. Existential matching
// must NOT fabricate a finding here.
func TestDecisionConformance_ExistentialAuthoritativeOnSibling(t *testing.T) {
	db := setupDecisionDB(t, authoritativeDecisionPlan(&DecisionCheck{Artifact: "websocket", ID: "game", Field: "authoritative", Op: "eq", Value: 1}))
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, tick_hz, authoritative) VALUES ('game', 30, 0), ('game-events', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 0 {
		t.Fatalf("authoritative on a sibling endpoint satisfies the system capability, got %d: %+v", len(issues), issues)
	}
}

// Existential must not mask a genuine miss: when NO endpoint is authoritative, the
// locked authoritative decision still flags.
func TestDecisionConformance_ExistentialNoMaskWhenNoneAuthoritative(t *testing.T) {
	db := setupDecisionDB(t, authoritativeDecisionPlan(&DecisionCheck{Artifact: "websocket", ID: "game", Field: "authoritative", Op: "eq", Value: 1}))
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, tick_hz, authoritative) VALUES ('game', 30, 0), ('game-events', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 1 {
		t.Fatalf("no authoritative endpoint anywhere must still flag, got %d: %+v", len(issues), issues)
	}
}

// The tick capability is existential too: a locked tick decision pinned to the wrong
// endpoint of a hybrid split (cursors/discrete on a relay, positions on a tick) is
// satisfied by whichever endpoint actually ticks.
func TestDecisionConformance_ExistentialTickOnSibling(t *testing.T) {
	db := setupDecisionDB(t, tickDecisionPlan(&DecisionCheck{Artifact: "websocket", ID: "chat", Field: "tick_hz", Op: "gt", Value: 0}))
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, tick_hz) VALUES ('chat', 0), ('positions', 30)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 0 {
		t.Fatalf("a ticking sibling endpoint satisfies the tick decision, got %d: %+v", len(issues), issues)
	}
}

// A relay decision asserts the ABSENCE of tick (tick_hz eq 0). If the endpoint is
// mistakenly built as a tick endpoint (tick_hz=20), the literal check fails - and the
// existential relaxation must NOT mask it just because a sibling relay/auth endpoint
// legitimately reads tick_hz=0. A relay built as tick silently coalesces its net.send
// frames and drops messages, so this must flag. The relaxation is for PRESENCE
// assertions only (tick_hz gt 0, authoritative eq 1), never an absence assertion.
func TestDecisionConformance_RelayBuiltAsTickNotMaskedBySibling(t *testing.T) {
	plan := Plan{
		Goal:  "chat room",
		Tasks: []PlanTaskSpec{{Title: "Realtime relay", Tools: []string{"manage_relay"}}},
		Decisions: []PlanDecision{{Topic: "realtime model", Choice: "relay", Rationale: "discrete messages only",
			Check: &DecisionCheck{Artifact: "websocket", ID: "chat", Field: "tick_hz", Op: "eq", Value: 0}}},
	}
	db := setupDecisionDB(t, plan)
	// `chat` was mis-built as a tick endpoint; `notify` is a correct relay (tick_hz=0).
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, tick_hz) VALUES ('chat', 20), ('notify', 0)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 1 {
		t.Fatalf("a relay decision (tick_hz eq 0) built as tick=20 must flag even with a tick_hz=0 sibling; "+
			"the existential relaxation is presence-only, got %d: %+v", len(issues), issues)
	}
}

// A correctly-built relay (tick_hz=0) under the same absence check must NOT flag: the
// literal check passes, so the presence-gated relaxation is never consulted.
func TestDecisionConformance_RelayBuiltAsRelayPasses(t *testing.T) {
	plan := Plan{
		Goal:  "chat room",
		Tasks: []PlanTaskSpec{{Title: "Realtime relay", Tools: []string{"manage_relay"}}},
		Decisions: []PlanDecision{{Topic: "realtime model", Choice: "relay", Rationale: "discrete messages only",
			Check: &DecisionCheck{Artifact: "websocket", ID: "chat", Field: "tick_hz", Op: "eq", Value: 0}}},
	}
	db := setupDecisionDB(t, plan)
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, tick_hz) VALUES ('chat', 0)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 0 {
		t.Fatalf("a correctly-built relay (tick_hz=0) must not flag, got %d: %+v", len(issues), issues)
	}
}

// Existential matching is SCOPED to realtime capabilities. A per-endpoint field
// (matchmaking) stays literal: a check pinned to `space` must still flag when `space`
// itself fails, even though a sibling `arena` matches - otherwise existential leakage
// would hide a genuinely mis-configured endpoint.
func TestDecisionConformance_NonCapabilityFieldStaysLiteral(t *testing.T) {
	plan := Plan{
		Decisions: []PlanDecision{{Topic: "multiplayer room model", Choice: "matchmade",
			Check: &DecisionCheck{Artifact: "websocket", ID: "space", Field: "matchmaking", Op: "eq", Value: 1}}},
	}
	db := setupDecisionDB(t, plan)
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, matchmaking) VALUES ('space', 0), ('arena', 1)`); err != nil {
		t.Fatal(err)
	}
	if issues := runDecisionValidator(t, db); len(issues) != 1 {
		t.Fatalf("a non-capability field must match the named endpoint literally, got %d: %+v", len(issues), issues)
	}
}

// Two distinct failing decisions must produce distinct Fingerprints, so each earns
// its own fix-pass. Before Signature was set, both collapsed to
// "decision_conformance\x00\x00" and triageRound demoted all but the first.
func TestDecisionConformance_DistinctFingerprintsPerDecision(t *testing.T) {
	plan := Plan{
		Decisions: []PlanDecision{
			{Topic: "realtime model", Choice: "authoritative",
				Check: &DecisionCheck{Artifact: "websocket", ID: "game", Field: "authoritative", Op: "eq", Value: 1}},
			{Topic: "multiplayer room model", Choice: "matchmade",
				Check: &DecisionCheck{Artifact: "websocket", ID: "game", Field: "matchmaking", Op: "eq", Value: 1}},
		},
	}
	db := setupDecisionDB(t, plan)
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, authoritative, matchmaking) VALUES ('game', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	issues := runDecisionValidator(t, db)
	if len(issues) != 2 {
		t.Fatalf("two failing decisions must both flag, got %d: %+v", len(issues), issues)
	}
	if fp0, fp1 := issues[0].Fingerprint(), issues[1].Fingerprint(); fp0 == fp1 {
		t.Fatalf("distinct decisions must have distinct fingerprints, both = %q", fp0)
	}
}

// The locked realtime model was never built: the named endpoint is missing and no
// other endpoint carries the property. That used to skip-as-pass.
func TestDecisionConformance_FlagsLockedEndpointNeverBuilt(t *testing.T) {
	db := setupDecisionDB(t, tickDecisionPlan(&DecisionCheck{Artifact: "websocket", ID: "canvas", Field: "tick_hz", Op: "gt", Value: 0}))
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, tick_hz) VALUES ('chat', 0)`); err != nil {
		t.Fatal(err)
	}
	issues := runDecisionValidator(t, db)
	if len(issues) != 1 || issues[0].Category != "decision_conformance" {
		t.Fatalf("a locked endpoint that was never built must flag once, got %d: %+v", len(issues), issues)
	}
}
