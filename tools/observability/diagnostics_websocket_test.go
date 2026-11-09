package observability

import "testing"

// TestNormalizeWSPath pins that the test_websocket action accepts every shape a
// model is likely to pass for an endpoint - bare name, with /ws, fully qualified
// - and canonicalizes them all to the mounted /api/<path>/ws route.
func TestNormalizeWSPath(t *testing.T) {
	cases := map[string]string{
		"call":            "/api/call/ws",
		"/call":           "/api/call/ws",
		"call/ws":         "/api/call/ws",
		"/api/call/ws":    "/api/call/ws",
		"api/call/ws":     "/api/call/ws",
		"  call  ":        "/api/call/ws",
		"signaling/ws":    "/api/signaling/ws",
		"/api/signal/ws/": "/api/signal/ws",
	}
	for in, want := range cases {
		if got := normalizeWSPath(in); got != want {
			t.Errorf("normalizeWSPath(%q) = %q, want %q", in, got, want)
		}
	}
}
