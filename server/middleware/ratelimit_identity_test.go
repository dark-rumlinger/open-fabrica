package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"openfabrica/internal/replayid"
)

// A replay's session tab is rate-limited as the visitor it simulates, so its flows
// and users do not share the one loopback bucket. The cookie counts only from
// loopback and only while its replay runs: a remote client, or a finished replay's
// token, falls back to the client IP.
func TestRateLimitIdentity_ReplayVisitorOnlyFromLoopbackWhileActive(t *testing.T) {
	token, end := replayid.Begin()
	defer end()

	req := func(remote, value string) string {
		r := httptest.NewRequest("GET", "/api/x", nil)
		r.RemoteAddr = remote
		if value != "" {
			r.AddCookie(&http.Cookie{Name: replayid.Cookie, Value: value})
		}
		return RateLimitIdentity(r)
	}
	bob := req("127.0.0.1:5000", replayid.Value(token, "0.bob"))
	main := req("127.0.0.1:5000", replayid.Value(token, "0.main"))
	if bob == "127.0.0.1" || bob == main {
		t.Fatalf("replay sessions must be separate visitors, got bob=%q main=%q", bob, main)
	}
	if got := req("203.0.113.7:4000", replayid.Value(token, "0.bob")); got != "203.0.113.7" {
		t.Errorf("a remote client's cookie must be ignored, got %q", got)
	}
	if got := req("127.0.0.1:5000", ""); got != "127.0.0.1" {
		t.Errorf("a plain loopback request is keyed by IP, got %q", got)
	}

	// Separate buckets: exhausting one visitor leaves the other untouched.
	rl := NewRateLimiter(0.0001, 2)
	for i := 0; i < 2; i++ {
		rl.Allow(bob)
	}
	if rl.Allow(bob) {
		t.Fatal("bob's bucket should be empty")
	}
	if !rl.Allow(main) {
		t.Error("another replay visitor must have its own bucket")
	}

	end()
	if got := req("127.0.0.1:5000", replayid.Value(token, "0.bob")); got != "127.0.0.1" {
		t.Errorf("a finished replay's cookie must not count, got %q", got)
	}
}
