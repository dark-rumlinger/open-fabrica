package tools

import "testing"

// TestNormalizeEndpointPath covers the canonicalisation contract: strip
// surrounding whitespace, leading slashes, a leading "api/" segment
// (case-insensitive), and trailing slashes. The platform stores endpoint
// paths in this suffix form across every of_*_endpoints table and the
// validator queries by it; mismatched formats here cause silent
// plan-coverage false negatives.
func TestNormalizeEndpointPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"auth", "auth"},
		{"/auth", "auth"},
		{"//auth", "auth"},
		{"api/auth", "auth"},
		{"/api/auth", "auth"},
		{"/API/auth", "auth"}, // case-insensitive prefix
		{"Api/auth", "auth"},
		{"  /api/auth  ", "auth"},
		{"auth/", "auth"},
		{"/api/auth/", "auth"},
		{"v1/users/profile", "v1/users/profile"},
		{"/api/v1/users/profile", "v1/users/profile"},
		// "api" alone is NOT a prefix (no trailing slash); leave intact.
		{"api", "api"},
		// "apicalls" must not have its prefix trimmed.
		{"apicalls", "apicalls"},
		// Trailing slash without prefix.
		{"chat/", "chat"},
	}
	for _, c := range cases {
		got := NormalizeEndpointPath(c.in)
		if got != c.want {
			t.Errorf("NormalizeEndpointPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestValidatePathSegments guards the catch for LLM-invented :param paths.
// The router matches stored paths literally - "collab/:roomId" never resolves
// at request time and silently 404s, which looks like a runtime bug. The
// validator must reject these at tool-call time with an actionable hint.
func TestValidatePathSegments(t *testing.T) {
	bad := []string{
		"collab/:roomId",
		":id",
		"users/:userId/posts",
		"collab/{roomId}",
		"{id}",
	}
	for _, p := range bad {
		if msg := ValidatePathSegments(p); msg == "" {
			t.Errorf("ValidatePathSegments(%q) = \"\", want non-empty error", p)
		}
	}
	good := []string{
		"",
		"chat",
		"collab",
		"v1/users/profile",
		"api-v2/items",
	}
	for _, p := range good {
		if msg := ValidatePathSegments(p); msg != "" {
			t.Errorf("ValidatePathSegments(%q) = %q, want empty", p, msg)
		}
	}
}

// TestValidateReservedSuffix guards the double-suffix mistake: an SSE stream at
// endpoint_path "events/stream" is served at /api/events/stream/stream and
// collides with the /stream route of "events". The runtime appends the suffix, so
// a redundant trailing one is rejected; a bare resource that happens to be named
// like a suffix (no slash) is fine, and an unrelated multi-segment path is left
// alone.
func TestValidateReservedSuffix(t *testing.T) {
	bad := []string{
		"events/stream",
		"files/upload",
		"orders/export",
		"orders/import",
	}
	for _, p := range bad {
		if msg := ValidateReservedSuffix(p); msg == "" {
			t.Errorf("ValidateReservedSuffix(%q) = \"\", want a redundant-suffix rejection", p)
		}
	}
	good := []string{
		"",
		"events",
		"stream", // a bare resource, not a redundant suffix
		"export", // ditto
		"v1/events",
		"streams", // not an exact suffix segment
	}
	for _, p := range good {
		if msg := ValidateReservedSuffix(p); msg != "" {
			t.Errorf("ValidateReservedSuffix(%q) = %q, want empty", p, msg)
		}
	}
}

// TestValidateEndpointPathCharset guards the injection catch: an endpoint path
// is spliced into generated client JS (js/auth.js) and email hrefs, so any
// character outside the slug charset must be rejected before storage.
func TestValidateEndpointPathCharset(t *testing.T) {
	bad := []string{
		"auth');alert(1);//", // JS string breakout
		"a b",                // whitespace
		"users?x=1",          // query
		"a'b",                // quote
		"a\"b",               // double quote
		"a;b",                // semicolon
		"a<b",                // angle bracket
		"a/",                 // trailing slash (unnormalized)
		"a//b",               // empty segment
		"café",               // non-ascii
	}
	for _, p := range bad {
		if msg := ValidateEndpointPathCharset(p); msg == "" {
			t.Errorf("ValidateEndpointPathCharset(%q) = \"\", want non-empty error", p)
		}
	}
	good := []string{
		"", // empty handled by the caller's required check
		"auth",
		"users",
		"v1/posts",
		"api-v2/items",
		"_query",
		"a_b-c",
	}
	for _, p := range good {
		if msg := ValidateEndpointPathCharset(p); msg != "" {
			t.Errorf("ValidateEndpointPathCharset(%q) = %q, want empty", p, msg)
		}
	}
}
