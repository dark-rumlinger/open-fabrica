package public

import (
	"net/http"
	"testing"
)

// The OAuth token/userinfo client must keep the SSRF egress guard: a redirect to
// an unrelated internal address is refused, while the provider's own configured
// host stays reachable (so a self-hosted provider on a LAN keeps working). This
// pins the behavior wired in via oauthHTTPClient / netguard.SSRFSafeClientAllowingHost.
func TestOAuthHTTPClientBlocksSSRFRedirect(t *testing.T) {
	client := oauthHTTPClient("https://oauth.example.com/token")

	// A redirect to an unrelated private host must be blocked.
	toPrivate, err := http.NewRequest(http.MethodGet, "http://192.168.1.50/callback", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := client.CheckRedirect(toPrivate, nil); err == nil {
		t.Fatal("expected redirect to a private host to be blocked, got nil")
	}

	// A redirect back to the provider's own configured host is allowed even
	// though the guard otherwise blocks private ranges.
	toProvider, err := http.NewRequest(http.MethodGet, "https://oauth.example.com/next", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := client.CheckRedirect(toProvider, nil); err != nil {
		t.Fatalf("expected redirect to the provider host to be allowed, got %v", err)
	}
}
