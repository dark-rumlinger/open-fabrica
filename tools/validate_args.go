package tools

import (
	"fmt"
	"reflect"
)

// isAnySliceKind reports whether v's underlying Go kind is a slice or array.
// Used by the "array" type check so Examples carrying typed Go slices (e.g.
// []map[string]interface{}) validate the same way the runtime []interface{}
// does. Without this, registration-time example validation rejects typed
// slice examples that the runtime accepts after JSON decode.
func isAnySliceKind(v any) bool {
	if v == nil {
		return false
	}
	k := reflect.TypeOf(v).Kind()
	return k == reflect.Slice || k == reflect.Array
}

// argAliases maps a canonical param name to alternate keys a model commonly reaches
// for instead. A weak model frequently sends "path" where the file tools declare
// "file_path" (the bare name is far more common in its training), then burns whole
// turns recovering from the missing-arg error. normalizeArgAliases copies such an
// alias into the canonical key when the canonical key is absent, so the natural call
// just works, generic over any tool whose schema declares the canonical param.
//
// The search tools each name "the search string" differently (manage_data
// action=search wants "query_text", manage_keyword_search "fts_query",
// manage_vector_search "semantic_query", manage_web_search "query"), so a model
// reaching for the natural "query" on the wrong one would miss the required arg.
// Aliasing every synonym onto each canonical lets any resolve; safe because a fill
// only happens for the param the tool actually declares (never clobbering a supplied
// value), and no search tool declares two of these.
var argAliases = map[string][]string{
	"file_path": {"path"},
	// manage_documents render_url takes a PAGE route; the model's natural "path"
	// (or "route"/"url") resolves onto it. Only manage_documents declares
	// page_route, so this fill is scoped to that tool.
	"page_route":     {"path", "route", "url"},
	"query_text":     {"query", "fts_query", "semantic_query"},
	"fts_query":      {"query", "query_text", "semantic_query"},
	"semantic_query": {"query", "query_text", "fts_query"},
}

// normalizeArgAliases fills a canonical arg from a known alias when the canonical
// key is absent or empty and the tool actually declares that param. Mutates args
// in place; safe to call before validateArgs. It only ever writes a param the
// tool declares, so it cannot inject an arg a handler does not expect, and it
// never overwrites a value the caller already supplied under the canonical name.
func normalizeArgAliases(def *ToolDef, args map[string]any) {
	if len(args) == 0 {
		return
	}
	for _, p := range def.Params {
		aliases, ok := argAliases[p.Name]
		if !ok {
			continue
		}
		if v, present := args[p.Name]; present && v != nil && !isEmptyValue(v, p.Type) {
			continue // canonical already supplied; leave it
		}
		for _, alias := range aliases {
			if v, present := args[alias]; present && v != nil {
				args[p.Name] = v
				break
			}
		}
	}
}

// validateArgs runs the per-call argument schema check before the handler
// dispatches. Catches missing required args, type mismatches, and enum
// violations and returns a Result the LLM can act on without the handler
// having to re-check anything. Called from ToolDef.Execute.
//
// All failures use a stable Result.Code so consumers can branch on the
// machine-readable tag instead of substring-matching Error.
func validateArgs(def *ToolDef, args map[string]any) *Result {
	for _, p := range def.Params {
		raw, present := args[p.Name]

		// Required check.
		if p.Required && (!present || raw == nil || isEmptyValue(raw, p.Type)) {
			return FailCode(CodeMissingArg, "%s", missingArgError(p))
		}
		if !present || raw == nil {
			continue
		}

		// Type check (only when value is present). AltType, when set, widens
		// the accepted concrete type (e.g. body: string OR object).
		if !typeMatches(raw, p.Type) && (p.AltType == "" || !typeMatches(raw, p.AltType)) {
			declared := p.Type
			if p.AltType != "" {
				declared = p.Type + " or " + p.AltType
			}
			return FailCode("invalid_type", "arg %q must be %s, got %T", p.Name, declared, raw)
		}

		// Enum check (string-typed enums only; the schema does not declare
		// enums on other types in this codebase). An empty string on an
		// optional enum means "unset" (e.g. an omitted kind the handler will
		// infer, or an absent list filter) and must not be rejected as an
		// out-of-enum value; the Required check above already covers the case
		// where empty is not allowed.
		if len(p.Enum) > 0 && p.Type == "string" {
			s, _ := raw.(string)
			if s == "" && !p.Required {
				continue
			}
			if !enumContains(p.Enum, s) {
				return FailCode("invalid_enum", "arg %q must be one of %v, got %q", p.Name, p.Enum, s)
			}
		}
	}
	return nil
}

// missingArgError builds the message for a missing required arg. It folds the
// param's allowed values and one-line description into the error so the model
// can correct the call on its FIRST failure instead of retrying the same empty
// shape until the loop's repeated-failure guard nudges it. A bare "missing
// required arg" tells the model what's absent but not what to supply; the enum
// and description close that gap.
func missingArgError(p Param) string {
	msg := fmt.Sprintf("missing required arg %q (type %s)", p.Name, p.Type)
	if len(p.Enum) > 0 {
		msg += fmt.Sprintf("; allowed values: %v", p.Enum)
	}
	if p.Description != "" {
		msg += ". " + p.Description
	}
	return msg
}

// isEmptyValue reports whether a present-but-empty value should count as
// "missing" for Required enforcement. Strings and arrays use length;
// other types pass through (presence + non-nil is enough).
//
// Arrays use reflect so a typed Go slice in an Example (e.g.
// []map[string]interface{}) validates the same way the runtime
// []interface{} (post-JSON-decode) does.
//
// Booleans deliberately pass through, so `arg:"confirm,required"` is satisfied by
// confirm:false. "Required" here means the KEY must be present, not that the value must be
// truthy - the acknowledgement gates (manage_schema's confirm, and every destructive
// action's) re-check the value themselves and refuse on false, with a message that says
// what to set. Treating false as missing here would replace that specific refusal with a
// generic missing_arg, which is the less useful of the two.
func isEmptyValue(v any, declared string) bool {
	switch declared {
	case "string":
		s, _ := v.(string)
		return s == ""
	case "array":
		if !isAnySliceKind(v) {
			return true
		}
		return reflect.ValueOf(v).Len() == 0
	}
	return false
}

// typeMatches reports whether raw's concrete type satisfies the declared
// Param.Type. Mirrors how JSON decodes into Go: numbers become float64,
// arrays become []interface{}, objects become map[string]interface{}.
func typeMatches(raw any, declared string) bool {
	switch declared {
	case "string":
		_, ok := raw.(string)
		return ok
	case "boolean":
		_, ok := raw.(bool)
		return ok
	case "number":
		switch raw.(type) {
		case float64, float32, int, int64, int32:
			return true
		}
		return false
	case "integer":
		switch v := raw.(type) {
		case int, int64, int32:
			return true
		case float64:
			// JSON numbers are float64; accept whole-valued floats as integers.
			return v == float64(int64(v))
		case float32:
			return v == float32(int32(v))
		}
		return false
	case "array":
		if _, ok := raw.([]any); ok {
			return true
		}
		// Accept any concrete Go slice. JSON-decoded args arrive as
		// []interface{}, but Example() values built in Go arrive as typed
		// slices (e.g. []map[string]interface{} for a [{search,replace}]
		// example). The runtime handlers downcast through json.Marshal so
		// the underlying element shape is what matters, not the slice type.
		return isAnySliceKind(raw)
	case "object":
		_, ok := raw.(map[string]any)
		return ok
	case "":
		return true
	}
	return true
}

func enumContains(enum []string, v string) bool {
	for _, e := range enum {
		if e == v {
			return true
		}
	}
	return false
}
