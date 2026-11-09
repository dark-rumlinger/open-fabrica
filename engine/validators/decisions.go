package validators

// The decision-conformance validator: the post-build gate that binds the plan to
// the build. The planner LOCKS load-bearing choices in plan.decisions, each
// optionally carrying a machine-checkable `check` over a built artifact; this
// asserts the build matches. Static and deliberately CONSERVATIVE: an unknown
// artifact/row/field SKIPS the check rather than failing it, so it never
// fabricates a finding and traps a fix loop.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"openfabrica/engine"
	"openfabrica/internal/wsendpoint"
)

type decisionConformanceValidator struct{}

func (decisionConformanceValidator) Name() string { return "decision_conformance" }

func (decisionConformanceValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	plan := latestPlan(pdb)
	if plan == nil || len(plan.Decisions) == 0 {
		return nil
	}
	var issues []Issue
	for _, d := range plan.Decisions {
		if d.Check == nil || strings.TrimSpace(d.Check.Field) == "" {
			continue
		}
		actual, ok := resolveDecisionField(pdb, d.Check.Artifact, d.Check.ID, d.Check.Field)
		if !ok {
			// The one unresolvable case that IS a finding: the check names a realtime
			// endpoint the build never created, while the field is one we can read. A
			// planner-guessed id is forgiven when ANY endpoint satisfies the check;
			// otherwise the locked model was simply not built, which must be flagged.
			if iss := missingWSEndpointIssue(pdb, d); iss != nil {
				issues = append(issues, *iss)
			}
			continue // otherwise unresolvable -> skip; never a false positive
		}
		if decisionConforms(d.Check.Op, actual, d.Check.Value) {
			continue
		}
		// A table that anyone may read but only a role may write is "public" data
		// ownership as far as a locked decision cares: the lock is about who can see the
		// rows, and the write gate is the stricter, intended half of the same design.
		if publicReadGatedWrites(pdb, d.Check, actual) {
			continue
		}
		// The literal endpoint fails, but a system-level realtime capability may
		// live on a SIBLING endpoint (a hybrid split). Honor the capability if ANY
		// endpoint of this kind satisfies it: the planner could only guess endpoint
		// ids before the build laid them out.
		if isRealtimeCapabilityField(d.Check.Artifact, d.Check.Field) &&
			isPresenceAssertion(d.Check.Op, d.Check.Value) &&
			wsAnyEndpointSatisfies(pdb, d.Check.Field, d.Check.Op, d.Check.Value) {
			continue
		}
		issues = append(issues, Issue{
			Severity:      "error",
			Category:      "decision_conformance",
			Deterministic: true,
			// Signature discriminates distinct decisions: without it every failing
			// decision (no page/file) collapses to one Fingerprint() and triageRound
			// demotes all but the first. Derived from the persisted plan (stable
			// across runs), never the volatile message or actual value.
			Signature: strings.TrimSpace(d.Topic) + "\x00" + d.Check.Artifact + "\x00" +
				d.Check.ID + "\x00" + d.Check.Field,
			Message: fmt.Sprintf(
				"The plan LOCKED the decision %q = %q: it requires the %s `%s` to have `%s` %s %v, "+
					"but the built one has `%s` = %v. %s",
				strings.TrimSpace(d.Topic), strings.TrimSpace(d.Choice),
				d.Check.Artifact, d.Check.ID, d.Check.Field, humanCheckOp(d.Check.Op), d.Check.Value,
				d.Check.Field, actual, decisionFixHint(d.Check.Artifact, d.Check.Field, d.Check.ID)),
		})
	}
	return issues
}

// publicReadGatedWrites reports whether c is a "public" access-model check on a REST
// endpoint that is world-readable (public_read) and merely gates its writes by role or
// sign-in, which satisfies the decision even though its derived model reads "role".
func publicReadGatedWrites(pdb *sql.DB, c *engine.DecisionCheck, actual any) bool {
	switch strings.ToLower(strings.TrimSpace(c.Artifact)) {
	case "endpoint", "api":
	default:
		return false
	}
	if strings.ToLower(strings.TrimSpace(c.Field)) != "auth_model" || strings.ToLower(strings.TrimSpace(c.Op)) != "eq" {
		return false
	}
	if want, ok := c.Value.(string); !ok || strings.ToLower(strings.TrimSpace(want)) != "public" {
		return false
	}
	if got, _ := actual.(string); got != "role" && got != "authenticated" {
		return false
	}
	path := strings.Trim(strings.TrimPrefix(strings.TrimSpace(c.ID), "/api/"), "/")
	var publicRead bool
	var owner string
	if err := pdb.QueryRow(`SELECT COALESCE(public_read,0), COALESCE(owner_column,'') FROM of_api_endpoints WHERE path = ?`, path).Scan(&publicRead, &owner); err != nil {
		return false
	}
	return publicRead && owner == ""
}

// missingWSEndpointIssue reports a websocket decision check whose endpoint does not exist
// and which no other endpoint satisfies either. nil when the check is not a websocket
// one, its field is outside the vocabulary, the endpoint exists, or a sibling conforms.
func missingWSEndpointIssue(pdb *sql.DB, d engine.PlanDecision) *Issue {
	c := d.Check
	switch strings.ToLower(strings.TrimSpace(c.Artifact)) {
	case "websocket", "ws":
	default:
		return nil
	}
	if !engine.DecisionCheckResolvable(c.Artifact, c.Field) {
		return nil
	}
	var n int
	if err := pdb.QueryRow("SELECT COUNT(*) FROM of_ws_endpoints WHERE path = ?", strings.TrimSpace(c.ID)).Scan(&n); err != nil || n > 0 {
		return nil // present (or unreadable): not a missing-endpoint finding
	}
	if wsAnyEndpointConforms(pdb, c.Field, c.Op, c.Value) {
		return nil // a sibling endpoint carries the locked property under another id
	}
	return &Issue{
		Severity:      "error",
		Category:      "decision_conformance",
		Deterministic: true,
		Signature:     strings.TrimSpace(d.Topic) + "\x00missing\x00" + c.ID,
		Message: fmt.Sprintf(
			"The plan LOCKED the decision %q = %q on the realtime endpoint `%s`, but no such endpoint exists and no other realtime endpoint has `%s` %s %v. %s",
			strings.TrimSpace(d.Topic), strings.TrimSpace(d.Choice), c.ID, c.Field, humanCheckOp(c.Op), c.Value,
			decisionFixHint(c.Artifact, c.Field, c.ID)),
	}
}

// humanCheckOp renders a decision-check op code as plain words, so the mismatch
// message reads "to have `tick_hz` greater than 0" instead of "to have tick_hz gt
// 0". Unknown ops fall back to the raw code.
func humanCheckOp(op string) string {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case "gt":
		return "greater than"
	case "gte":
		return "at least"
	case "lt":
		return "less than"
	case "lte":
		return "at most"
	case "eq":
		return "equal to"
	case "ne":
		return "not equal to"
	case "exists":
		return "present"
	case "nexists":
		return "absent"
	}
	return op
}

// decisionFixHint names the EXACT tool + action that sets the checked field, so
// the failure is actionable. Only the fields a guide '## Decision:' block
// actually backs appear here (websocket tick_hz/authoritative, trigger count).
func decisionFixHint(artifact, field, id string) string {
	a := strings.ToLower(strings.TrimSpace(artifact))
	f := strings.ToLower(strings.TrimSpace(field))
	switch {
	case (a == "websocket" || a == "ws") && f == "tick_hz":
		return fmt.Sprintf("Fix: the realtime endpoint %q must be a continuous-state (tick) endpoint - "+
			"build it with `manage_tick create` (tick_hz 20/30/60), not a bare `manage_relay`.", id)
	case (a == "websocket" || a == "ws") && f == "authoritative":
		return fmt.Sprintf("Fix: the realtime endpoint %q must be server-authoritative - build it with "+
			"`manage_authoritative create` then `set_reducer`, not a trusting relay/tick.", id)
	case (a == "websocket" || a == "ws") && f == "matchmaking":
		return fmt.Sprintf("Fix: the realtime endpoint %q must be a server matchmaking lobby - build it with "+
			"`manage_matchmaking create`, not client-side pairing on a relay.", id)
	case (a == "websocket" || a == "ws") && f == "model":
		return fmt.Sprintf("Fix: build the realtime endpoint %q with the tool of the locked model "+
			"(relay -> manage_relay, tick -> manage_tick, authoritative -> manage_authoritative, "+
			"matchmaking -> manage_matchmaking, signaling -> manage_signaling).", id)
	case (a == "endpoint" || a == "api") && f == "auth_model":
		return fmt.Sprintf("Fix: set the endpoint's access model to the locked one with `manage_api action=update endpoint_path=%s auth_model=<locked model>` (owner and authored also need owner_column; authenticated plus public_write=true is the anonymous-submit, admin-read shape). Never make writes public to satisfy a lock: a table that is public to read but role-gated to write keeps public_read on and counts as public; if the plan was wrong for this table, say so in your summary.", strings.TrimPrefix(id, "/api/"))
	case (a == "trigger" || a == "triggers") && (f == "count" || f == "exists"):
		return fmt.Sprintf("Fix: add a `manage_triggers create` task on event_type %q that performs the "+
			"locked server-side action.", id)
	}
	return "Fix: build the artifact so it matches the locked decision."
}

// latestPlan loads and parses the most recent run's plan_json, mirroring the PWA
// validator's lookup. Returns nil on any read/parse error (fails closed).
func latestPlan(pdb *sql.DB) *Plan {
	var planJSON sql.NullString
	if err := pdb.QueryRow(`SELECT plan_json FROM of_runs
		WHERE plan_json IS NOT NULL AND plan_json != ''
		ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&planJSON); err != nil {
		return nil
	}
	if !planJSON.Valid || planJSON.String == "" {
		return nil
	}
	plan, err := ParsePlan(planJSON.String)
	if err != nil {
		return nil
	}
	return plan
}

// resolveDecisionField reads one property of a built artifact for a decision
// check. Returns (value, true) only when fully resolved; an unknown artifact type,
// a missing row, or an unknown field returns (nil, false) so the check is skipped.
func resolveDecisionField(pdb *sql.DB, artifact, id, field string) (any, bool) {
	switch strings.ToLower(strings.TrimSpace(artifact)) {
	case "websocket", "ws":
		return wsEndpointField(pdb, id, field)
	case "trigger", "triggers":
		return triggerField(pdb, id, field)
	case "endpoint", "api":
		return endpointField(pdb, id, field)
	default:
		return nil, false // unknown artifact type - skip
	}
}

// endpointField resolves a REST endpoint's auth_model for a data-ownership decision.
// The id is the endpoint path (an "/api/" prefix or slashes are tolerated). A missing
// endpoint or unknown field -> (nil, false), so the check is skipped, never invented.
func endpointField(pdb *sql.DB, id, field string) (any, bool) {
	if strings.ToLower(strings.TrimSpace(field)) != "auth_model" {
		return nil, false
	}
	path := strings.Trim(strings.TrimPrefix(strings.TrimSpace(id), "/api/"), "/")
	var requiresAuth, publicRead bool
	var role, owner, scope string
	if err := pdb.QueryRow(`SELECT COALESCE(requires_auth,0), COALESCE(public_read,0), COALESCE(required_role,''), COALESCE(owner_column,''), COALESCE(owner_scope,'')
		FROM of_api_endpoints WHERE path = ?`, path).Scan(&requiresAuth, &publicRead, &role, &owner, &scope); err != nil {
		return nil, false
	}
	return engine.EndpointAuthModel(requiresAuth, publicRead, role, owner, scope), true
}

// triggerField resolves a trigger-set property for a decision check. The `id` is
// the event_type; the only field is "count" (alias "exists"), counting ENABLED
// triggers on that event. Missing table / unknown field -> (nil, false) so the
// check is skipped, never a false positive.
func triggerField(pdb *sql.DB, eventType, field string) (any, bool) {
	f := strings.ToLower(strings.TrimSpace(field))
	if f != "count" && f != "exists" {
		return nil, false
	}
	var n int
	if err := pdb.QueryRow(
		"SELECT COUNT(*) FROM of_triggers WHERE event_type = ? AND COALESCE(is_enabled,1) = 1",
		strings.TrimSpace(eventType)).Scan(&n); err != nil {
		return nil, false
	}
	return n, true
}

// wsEndpointField returns one column of a websocket endpoint by path. Numeric
// columns scan as int, the reducer column as string. Unknown field or missing
// endpoint -> (nil, false).
func wsEndpointField(pdb *sql.DB, path, field string) (any, bool) {
	f := strings.ToLower(strings.TrimSpace(field))
	if f == "model" {
		row, err := wsendpoint.Load(pdb, strings.TrimSpace(path))
		if err != nil {
			return nil, false
		}
		return row.EffectiveModel(), true
	}
	if f == "reducer" {
		var s string
		if err := pdb.QueryRow("SELECT COALESCE(reducer,'') FROM of_ws_endpoints WHERE path = ?", strings.TrimSpace(path)).Scan(&s); err != nil {
			return nil, false
		}
		return s, true
	}
	col, ok := engine.WSNumericCheckCols[f]
	if !ok {
		return nil, false
	}
	var n int
	if err := pdb.QueryRow("SELECT "+col+" FROM of_ws_endpoints WHERE path = ?", strings.TrimSpace(path)).Scan(&n); err != nil {
		return nil, false
	}
	return n, true
}

// wsAnyEndpointConforms reports whether ANY websocket endpoint satisfies op(value) on a
// decision-check field: the numeric columns via wsAnyEndpointSatisfies, model (effective,
// so legacy rows infer one) and reducer as strings. A read error -> false.
func wsAnyEndpointConforms(pdb *sql.DB, field, op string, expected any) bool {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "model":
		rows, err := wsendpoint.LoadAll(pdb)
		if err != nil {
			return false
		}
		for _, r := range rows {
			if decisionConforms(op, r.EffectiveModel(), expected) {
				return true
			}
		}
		return false
	case "reducer":
		rows, err := pdb.Query("SELECT COALESCE(reducer,'') FROM of_ws_endpoints")
		if err != nil {
			return false
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			if rows.Scan(&r) == nil && decisionConforms(op, r, expected) {
				return true
			}
		}
		// A scan cut short by an error proves nothing conforms; the answer stays false.
		_ = rows.Err()
		return false
	}
	return wsAnyEndpointSatisfies(pdb, field, op, expected)
}

// isRealtimeCapabilityField reports whether a check asserts a realtime capability
// of the SYSTEM (any endpoint may carry it) rather than one endpoint's property.
// Scoped to the two fields whose multi-endpoint split is sanctioned. Per-endpoint
// fields (requires_auth, e2ee, matchmaking, room_gated) stay literal - existential
// matching there would mask a genuinely mis-configured endpoint.
func isRealtimeCapabilityField(artifact, field string) bool {
	switch strings.ToLower(strings.TrimSpace(artifact)) {
	case "websocket", "ws":
	default:
		return false
	}
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "authoritative", "tick_hz":
		return true
	}
	return false
}

// isPresenceAssertion reports whether a check asserts a capability is PRESENT
// rather than ABSENT. The existential sibling-endpoint relaxation is sound only
// for presence: an absence assertion must stay literal, or a sibling that
// legitimately has tick_hz=0 would mask THIS endpoint being mistakenly built as
// a tick endpoint. Reuses decisionConforms so the op semantics match exactly.
func isPresenceAssertion(op string, value any) bool {
	return decisionConforms(op, 1, value) && !decisionConforms(op, 0, value)
}

// wsAnyEndpointSatisfies reports whether ANY websocket endpoint's numeric column
// satisfies op(value), reusing decisionConforms so the comparison semantics match
// the per-path check exactly. Unknown field or a query error -> false (never
// turns a missing capability into a pass).
func wsAnyEndpointSatisfies(pdb *sql.DB, field, op string, expected any) bool {
	col, ok := engine.WSNumericCheckCols[strings.ToLower(strings.TrimSpace(field))]
	if !ok {
		return false
	}
	rows, err := pdb.Query("SELECT " + col + " FROM of_ws_endpoints")
	if err != nil {
		return false
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			continue
		}
		if decisionConforms(op, n, expected) {
			found = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return false
	}
	return found
}

// decisionConforms evaluates op(actual, expected). Numeric ops coerce to float64;
// eq/ne also handle strings; exists/nexists test presence. An op the validator
// cannot evaluate returns true (never blocks on an uncomparable assertion).
func decisionConforms(op string, actual, expected any) bool {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case "exists":
		return !isZeroish(actual)
	case "nexists":
		return isZeroish(actual)
	case "eq":
		return looseEqual(actual, expected)
	case "ne":
		return !looseEqual(actual, expected)
	case "gt", "gte", "lt", "lte":
		a, ok1 := toFloat(actual)
		e, ok2 := toFloat(expected)
		if !ok1 || !ok2 {
			return true // can't compare numerically -> don't block
		}
		switch strings.ToLower(strings.TrimSpace(op)) {
		case "gt":
			return a > e
		case "gte":
			return a >= e
		case "lt":
			return a < e
		case "lte":
			return a <= e
		}
	}
	return true
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case bool:
		// Boolean-valued locked decisions (matchmaking/authoritative/e2ee/room_gated/
		// ephemeral) carry a JSON bool, but the columns they check are INTEGER and
		// resolve to 0/1. Normalize bool -> 0/1 so `matchmaking eq false` matches a
		// stored 0 instead of falling through to a "false" != "0" string mismatch.
		if n {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

func looseEqual(a, b any) bool {
	if af, ok := toFloat(a); ok {
		if bf, ok2 := toFloat(b); ok2 {
			return af == bf
		}
	}
	// Case-insensitive: a planner writing value "Tick" means the model the tool
	// stores as "tick".
	return strings.EqualFold(strings.TrimSpace(fmt.Sprint(a)), strings.TrimSpace(fmt.Sprint(b)))
}

func isZeroish(v any) bool {
	if f, ok := toFloat(v); ok {
		return f == 0
	}
	return strings.TrimSpace(fmt.Sprint(v)) == ""
}

func init() {
	RegisterValidator(decisionConformanceValidator{})
}
