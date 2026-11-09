package tools

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"openfabrica/store"
)

// NormalizeEndpointPath returns the canonical suffix form of an endpoint path.
// Endpoint paths are stored across of_{api,auth,ws,stream,llm,upload,oauth}_endpoints
// as bare suffixes (e.g. "auth", "users", "v1/posts"); the platform prepends
// "/api/" at request time and the page router matches the suffix verbatim.
//
// Some LLMs emit "/api/auth" or "/auth" instead of "auth", which round-trips
// through the platform unchanged but breaks plan-coverage validation (the
// validator looks up by plan-declared path; storage holds the suffix form).
// Normalising at every boundary (PLAN parse and tool input) keeps storage and
// lookup byte-identical.
//
// Strips, in order: surrounding whitespace, leading slashes, a leading "api/"
// segment (case-insensitive), trailing slashes. Empty/whitespace-only in -> "".
func NormalizeEndpointPath(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for strings.HasPrefix(s, "/") {
		s = s[1:]
	}
	if len(s) >= 4 && strings.EqualFold(s[:4], "api/") {
		s = s[4:]
	}
	for strings.HasSuffix(s, "/") {
		s = s[:len(s)-1]
	}
	return s
}

// ValidatePathSegments rejects endpoint paths containing :param or {param}
// style placeholders. The router matches stored paths literally; a path like
// "collab/:roomId" never resolves at request time, so accepting it silently
// produces a 404 that looks like a server bug.
//
// Returns the empty string when the path is acceptable, otherwise a short
// human-readable error suitable for surfacing back through a tool Result.
func ValidatePathSegments(path string) string {
	if path == "" {
		return ""
	}
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, ":") || (strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")) {
			return fmt.Sprintf("endpoint_path segments are literal - %q is not a path parameter. %s A function endpoint reads a dynamic value from the query string (req.query) or body (req.body), not the path.", seg, store.PathParamNote)
		}
	}
	return ""
}

// endpointPathCharset matches a safe endpoint path: one or more URL-safe
// segments (letters, digits, hyphen, underscore) joined by single slashes, e.g.
// "auth", "users", "v1/posts". Deliberately strict: endpoint paths are spliced
// into generated client JS (js/auth.js) and email hrefs, so a path carrying
// quotes, parentheses, semicolons, or whitespace is a script/markup injection
// vector - restrict it to the slug charset the router and those emitters expect.
var endpointPathCharset = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)*$`)

// ValidateEndpointPathCharset returns a short human-readable error when path
// contains characters outside the safe endpoint-path charset, else the empty
// string. Expects an already-NormalizeEndpointPath'd value (no leading/trailing
// slash, no api/ prefix). Empty input is treated as acceptable here - callers
// reject empty separately with a clearer "required" message.
func ValidateEndpointPathCharset(path string) string {
	if path == "" {
		return ""
	}
	if !endpointPathCharset.MatchString(path) {
		return fmt.Sprintf("endpoint_path %q contains invalid characters - use only letters, digits, hyphen, underscore, and / between segments", path)
	}
	return ""
}

// reservedRouteSuffixes are the trailing route segments the runtime appends for
// the non-CRUD endpoint families: SSE /stream, file /upload, CSV /export and
// /import. A stored endpoint_path must never END in one of these as its own
// segment - the model meant the bare resource and the runtime adds the suffix, so
// "events/stream" would be served at /api/events/stream/stream and also collide
// with the /stream route of the "events" resource.
var reservedRouteSuffixes = map[string]bool{"stream": true, "upload": true, "export": true, "import": true}

// ValidateReservedSuffix rejects an endpoint_path whose final slash-separated
// segment is a reserved runtime suffix. A bare single-segment path (no slash) is
// left alone: only a redundant trailing suffix is the mistake. Returns "" when
// acceptable.
func ValidateReservedSuffix(path string) string {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return ""
	}
	last := path[i+1:]
	if reservedRouteSuffixes[last] {
		return fmt.Sprintf("endpoint_path %q ends with the reserved %q route segment, which the runtime appends itself - this would be served at /api/%s/%s and collide with the %s route of the base resource. Pass the bare resource path %q; the runtime adds /%s.",
			path, last, path, last, last, path[:i], last)
	}
	return ""
}

// ValidateAndNormalizeEndpointPath is the one-call form of the normalize +
// segment + charset + reserved-suffix checks every endpoint tool runs on an
// incoming path. It returns the canonical path and nil, or "" and an error Result
// to hand back to the model. Empty input normalizes to "" with no error - callers
// reject empty separately with their own clearer "required" message.
func ValidateAndNormalizeEndpointPath(raw string) (string, *Result) {
	path := NormalizeEndpointPath(raw)
	if msg := ValidatePathSegments(path); msg != "" {
		return "", FailCode(CodeBadArg, "%s", msg)
	}
	if msg := ValidateEndpointPathCharset(path); msg != "" {
		return "", FailCode(CodeBadArg, "%s", msg)
	}
	if msg := ValidateReservedSuffix(path); msg != "" {
		return "", FailCode("reserved_path", "%s", msg)
	}
	return path, nil
}

// DispatchAction resolves the action from args and routes to the matching
// handler. A missing action returns an error Result.
func DispatchAction(
	ctx *ToolContext,
	args map[string]any,
	routes map[string]ActionHandler,
) (*Result, error) {
	action, errResult := RequireAction(args)
	if errResult != nil {
		return errResult, nil
	}
	if handler, ok := routes[action]; ok {
		return handler(ctx, args)
	}
	// Build list of valid actions for error message.
	var valid []string
	for k := range routes {
		valid = append(valid, k)
	}
	sort.Strings(valid)
	return Fail("unknown action %q, valid actions: %s", action, strings.Join(valid, ", ")), nil
}

// OptionalString reads an optional string from a JSON-shaped map with a
// default. Tool arguments are decoded into typed Action structs (see
// PopulateAction); this survives for parsing external JSON payloads that stay
// maps by nature - webhook bodies, provider API responses.
func OptionalString(args map[string]any, key, defaultVal string) string {
	if val, ok := args[key].(string); ok && val != "" {
		return val
	}
	return defaultVal
}

// CodeNotFound tags every "X not found" rejection, so triage buckets them by
// name instead of collapsing into the unnamed reject bucket.
const CodeNotFound = "not_found"

// NotFound returns a standard "not found" error Result. Use for any "X not
// found" message so phrasing stays consistent across tools.
func NotFound(kind, name string) *Result {
	if name == "" {
		return FailCode(CodeNotFound, "%s", kind+" not found")
	}
	return FailCode(CodeNotFound, "%s %q not found", kind, name)
}

// NullIfEmpty returns nil for empty strings, otherwise the value. Useful for
// SQL inserts where a missing optional field should be NULL rather than "".
func NullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ResolveTemplateVars expands {{field}} placeholders in template against
// values in payload. Used by trigger dry-runs, email template rendering,
// and any other call path that does "fill in the blanks" string substitution
// on user-stored copy. Values are formatted with %v so non-string payload
// entries (numbers, bools) render readably.
func ResolveTemplateVars(template string, payload map[string]any) string {
	for key, val := range payload {
		template = strings.ReplaceAll(template, "{{"+key+"}}", fmt.Sprintf("%v", val))
	}
	return template
}
