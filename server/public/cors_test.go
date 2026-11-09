package public

import (
	"net/http/httptest"
	"testing"
)

// TestSetEndpointCORSOrigin pins the shared per-endpoint origin-matching logic
// used by the dynamic API, function endpoints, and LLM endpoints.
func TestSetEndpointCORSOrigin(t *testing.T) {
	cases := []struct {
		name        string
		corsOrigins string
		origin      string
		wantAllow   string // expected Access-Control-Allow-Origin ("" = header absent)
		wantVary    string // expected Vary ("" = header absent)
	}{
		{"empty spec writes nothing", "", "https://a.com", "", ""},
		{"wildcard allows any origin", "*", "https://a.com", "*", ""},
		{"wildcard with no origin still allows", "*", "", "*", ""},
		{"exact match reflects origin + Vary", "https://a.com,https://b.com", "https://b.com", "https://b.com", "Origin"},
		{"exact match tolerates surrounding spaces", "https://a.com, https://b.com", "https://b.com", "https://b.com", "Origin"},
		{"non-matching origin writes nothing", "https://a.com", "https://evil.com", "", ""},
		{"allow-list with empty origin writes nothing", "https://a.com", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			setEndpointCORSOrigin(rec, tc.corsOrigins, tc.origin)
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantAllow {
				t.Errorf("Allow-Origin = %q, want %q", got, tc.wantAllow)
			}
			if got := rec.Header().Get("Vary"); got != tc.wantVary {
				t.Errorf("Vary = %q, want %q", got, tc.wantVary)
			}
		})
	}
}

// TestSetEndpointCORSOrigin_OverridesCoarse pins the narrowing behavior: a
// specific per-endpoint allowlist is authoritative and must clear whatever the
// coarse instance-wide layer already wrote (default "*"). A disallowed origin is
// left with no Allow-Origin; a matched origin is reflected with credentials.
func TestSetEndpointCORSOrigin_OverridesCoarse(t *testing.T) {
	t.Run("disallowed origin drops inherited coarse *", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Access-Control-Allow-Origin", "*") // coarse allow-all
		setEndpointCORSOrigin(rec, "https://a.com", "https://evil.com")
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Allow-Origin = %q, want \"\" (coarse * must be dropped)", got)
		}
	})

	t.Run("disallowed origin drops inherited coarse credentials", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Access-Control-Allow-Origin", "https://a.com") // coarse matched a real allowlist
		rec.Header().Set("Access-Control-Allow-Credentials", "true")
		setEndpointCORSOrigin(rec, "https://b.com", "https://a.com") // a.com not in the per-endpoint list
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Allow-Origin = %q, want \"\"", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("Allow-Credentials = %q, want \"\"", got)
		}
	})

	t.Run("matched origin reflects with credentials, replacing coarse *", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Access-Control-Allow-Origin", "*")
		setEndpointCORSOrigin(rec, "https://a.com,https://b.com", "https://b.com")
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://b.com" {
			t.Errorf("Allow-Origin = %q, want https://b.com", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Errorf("Allow-Credentials = %q, want true", got)
		}
		if got := rec.Header().Get("Vary"); got != "Origin" {
			t.Errorf("Vary = %q, want Origin", got)
		}
	})

	t.Run("wildcard spec clears inherited credentials", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Access-Control-Allow-Origin", "https://a.com")
		rec.Header().Set("Access-Control-Allow-Credentials", "true")
		setEndpointCORSOrigin(rec, "*", "https://a.com")
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Allow-Origin = %q, want *", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("Allow-Credentials = %q, want \"\" (* + credentials is invalid)", got)
		}
	})
}
