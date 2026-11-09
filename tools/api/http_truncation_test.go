package api

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"net/http"

	"openfabrica/tools"
)

// httpTestContext points the relative-URL path at a local httptest server, the
// one route that legitimately bypasses the SSRF guard.
func httpTestContext(t *testing.T, handler http.HandlerFunc) *tools.ToolContext {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("test server port: %v", err)
	}
	return &tools.ToolContext{PublicPort: port}
}

// TestHTTPRequestFlagsTruncatedBody: a response past the body cap must be
// sliced AND flagged - a silent clip hands the model an invalid payload with no
// signal about why it is malformed.
func TestHTTPRequestFlagsTruncatedBody(t *testing.T) {
	big := strings.Repeat("x", HTTPBodyCap+512)
	ctx := httpTestContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	})

	res, err := (&HTTPRequest{URL: "/big"}).Run(ctx)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if !res.Success {
		t.Fatalf("request failed: %s", res.Error)
	}
	data := res.Data.(map[string]any)
	if data["body_truncated"] != true {
		t.Fatal("expected body_truncated:true for an over-cap response")
	}
	body, _ := data["body"].(string)
	if len(body) != HTTPResultBodyCap {
		t.Fatalf("body length = %d, want exactly the %d result cap", len(body), HTTPResultBodyCap)
	}
}

// TestHTTPRequestNoFlagForSmallBody: under the cap, no flag rides the result.
func TestHTTPRequestNoFlagForSmallBody(t *testing.T) {
	ctx := httpTestContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	})

	res, err := (&HTTPRequest{URL: "/small"}).Run(ctx)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	data := res.Data.(map[string]any)
	if _, present := data["body_truncated"]; present {
		t.Fatal("body_truncated must be absent for an under-cap response")
	}
	if data["body"] != "hello" {
		t.Fatalf("body = %v, want hello", data["body"])
	}
}

// TestExtractTextClipsAtCap: strip_html output past the extraction cap is
// clipped and reported.
func TestExtractTextClipsAtCap(t *testing.T) {
	long := "<p>" + strings.Repeat("word ", extractTextCap/4) + "</p>"
	text, truncated := extractText([]byte(long))
	if !truncated {
		t.Fatal("expected truncation for over-cap text")
	}
	if len(text) != extractTextCap {
		t.Fatalf("text length = %d, want %d", len(text), extractTextCap)
	}

	text, truncated = extractText([]byte("<p>short</p>"))
	if truncated || text != "short" {
		t.Fatalf("short text: got (%q, %v)", text, truncated)
	}
}
