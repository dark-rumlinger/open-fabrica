package api

import (
	"strings"
	"testing"

	"openfabrica/tools"
)

// TestHTTPRequestForwardsURLAndBlocks pins the typed forwarding for manage_http:
// the url arg must reach the SSRF guard (an absolute loopback URL is rejected)
// and an empty url returns the required-arg error. A live-network round trip is
// avoided so the test does not depend on any local server.
func TestHTTPRequestForwardsURLAndBlocks(t *testing.T) {
	ctx := &tools.ToolContext{}

	// Empty url -> required error (url arg forwarded through).
	res, err := (&HTTPRequest{}).Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "url is required") {
		t.Fatalf("expected 'url is required', got %+v", res)
	}

	// Absolute loopback URL with an object body -> SSRF guard rejects it. This
	// exercises the typed url + request_body path without touching the network.
	res, err = (&HTTPRequest{
		URL:         "http://127.0.0.1:9/blocked",
		Method:      "POST",
		RequestBody: map[string]any{"marker": "BODY_KEEP"},
		Headers:     map[string]any{"X-Marker": "HDR_KEEP"},
	}).Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "blocked URL") {
		t.Fatalf("expected SSRF block for loopback URL, got %+v", res)
	}
}
