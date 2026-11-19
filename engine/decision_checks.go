package engine

import "strings"

// The PLAN-time half of the decision-check vocabulary. The conformance
// validator that READS built rows against these checks lives in
// engine/validators (decisions.go); the two halves share WSNumericCheckCols so
// the field vocabulary cannot drift between plan warning and validation.

// WSNumericCheckCols maps a decision check's websocket field to its INTEGER
// column (0/1 for the boolean flags). Shared by the validators package's
// per-path resolver and existential any-endpoint resolver so both understand
// the same field vocabulary DecisionCheckResolvable admits.
var WSNumericCheckCols = map[string]string{
	"tick_hz":       "COALESCE(tick_hz,0)",
	"authoritative": "COALESCE(authoritative,0)",
	"e2ee":          "COALESCE(e2ee,0)",
	"matchmaking":   "COALESCE(matchmaking,0)",
	"room_gated":    "COALESCE(room_gated,0)",
	"ephemeral":     "COALESCE(ephemeral,0)",
}

// DecisionCheckResolvable reports whether an (artifact, field) pair is one the
// conformance validator's resolvers can actually read. Used at PLAN time -
// before any artifact exists - so planCheckWarnings can flag a check that would
// silently skip-as-pass because it targets vocabulary the validator never
// implemented. A check outside this set enforces nothing.
func DecisionCheckResolvable(artifact, field string) bool {
	f := strings.ToLower(strings.TrimSpace(field))
	switch strings.ToLower(strings.TrimSpace(artifact)) {
	case "websocket", "ws":
		// model: the realtime tool that built the endpoint (relay | tick | authoritative
		// | matchmaking | signaling), the one field that tells a relay from a signaling
		// endpoint or an authoritative/matchmaking one (all have tick_hz 0).
		if f == "reducer" || f == "model" {
			return true
		}
		_, ok := WSNumericCheckCols[f]
		return ok
	case "trigger", "triggers":
		return f == "count" || f == "exists"
	case "endpoint", "api":
		// A REST endpoint's access model, derived from its stored auth columns the way
		// manage_api's auth_model writes them: public | authenticated | role | owner |
		// authored. The data-ownership decision checks this.
		return f == "auth_model"
	}
	return false
}
