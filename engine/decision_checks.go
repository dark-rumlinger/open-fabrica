package engine

import (
	"strings"

	"openfabrica/internal/endpoints"
)

// The PLAN-time half of the decision-check vocabulary. The conformance
// validator that READS built rows against these checks lives in
// engine/validators (decisions.go); the two halves share WSNumericCheckCols so
// the field vocabulary cannot drift between plan warning and validation.

// WSNumericCheckCols maps a decision check's websocket field to its INTEGER
// column (0/1 for the boolean flags). Shared by the validators package's
// per-path resolver and existential any-endpoint resolver so both understand
// the same field vocabulary decisionCheckResolvable admits.
var WSNumericCheckCols = map[string]string{
	"tick_hz":       "COALESCE(tick_hz,0)",
	"authoritative": "COALESCE(authoritative,0)",
	"e2ee":          "COALESCE(e2ee,0)",
	"matchmaking":   "COALESCE(matchmaking,0)",
	"room_gated":    "COALESCE(room_gated,0)",
	"ephemeral":     "COALESCE(ephemeral,0)",
}

// decisionCheckResolvable reports whether an (artifact, field) pair is one the
// conformance validator's resolvers can actually read. Used at PLAN time -
// before any artifact exists - so planCheckWarnings can flag a check that would
// silently skip-as-pass because it targets vocabulary the validator never
// implemented. A check outside this set enforces nothing.
func decisionCheckResolvable(artifact, field string) bool {
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

// EndpointAuthModel derives the auth_model a stored REST endpoint has from the columns
// manage_api writes (translateAuthModel's inverse): authored scope wins, then an owner
// column, a required role, a token requirement without public_read, else public. A
// public_read endpoint is world-readable (only its writes need a token), which is what a
// "public" data-ownership decision asserts. Shared by the conformance validator and the
// RLS validator so both read an endpoint's model the same way.
func EndpointAuthModel(requiresAuth, publicRead bool, requiredRole, ownerColumn, ownerScope string) string {
	return endpoints.AuthModel(requiresAuth, publicRead, requiredRole, ownerColumn, ownerScope)
}

// DecisionCheckResolvable is decisionCheckResolvable for the conformance validator, which
// must tell "the endpoint is missing" from "the field is outside the vocabulary".
func DecisionCheckResolvable(artifact, field string) bool {
	return decisionCheckResolvable(artifact, field)
}
