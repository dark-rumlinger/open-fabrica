package engine

import "testing"

func TestEndpointAuthModel(t *testing.T) {
	cases := []struct {
		name                                string
		requiresAuth, publicRead            bool
		requiredRole, ownerColumn, ownerScp string
		want                                string
	}{
		{"open endpoint", false, false, "", "", "", "public"},
		{"token required for everything", true, false, "", "", "", "authenticated"},
		// The api_response_contract guide offers public_read as the way to say "public":
		// reads are world-open, only writes need a token.
		{"public_read with token-gated writes", true, true, "", "", "", "public"},
		{"role gate wins over public_read", true, true, "admin", "", "", "role"},
		{"owner scoped", true, false, "", "user_id", "", "owner"},
		{"authored scope", true, false, "", "user_id", "authored", "authored"},
	}
	for _, c := range cases {
		got := EndpointAuthModel(c.requiresAuth, c.publicRead, c.requiredRole, c.ownerColumn, c.ownerScp)
		if got != c.want {
			t.Errorf("%s: EndpointAuthModel = %q, want %q", c.name, got, c.want)
		}
	}
}
