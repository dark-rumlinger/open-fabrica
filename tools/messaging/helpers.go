package messaging

import (
	"encoding/json"
)

// Messaging-package helpers. Provider loading and the POST-with-auth path
// live in tools/internal/provider and tools/internal/httpclient; endpoint
// deletion routes through api.DeleteEndpointByPath. This file only holds
// the messaging-specific JSON-arg normalization helper.

// jsonStringArg normalizes an arg that the schema declares as an array but
// which the LLM may still hand us as a pre-serialized JSON string. Arrays
// are re-marshalled; strings pass through; anything else returns "".
// Downstream (DB, event bus) expects a JSON-string representation.
func jsonStringArg(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		s := string(b)
		// A typed-nil slice/map - which is what an omitted `options`/`fields`
		// arg arrives as - is NOT caught by `case nil` and marshals to the
		// literal "null", not "". Normalize it so the DB column stays empty
		// instead of storing "null", which the backend's hasFields check and
		// the frontend renderer would otherwise mistake for a real payload.
		if s == "null" {
			return ""
		}
		return s
	}
}
