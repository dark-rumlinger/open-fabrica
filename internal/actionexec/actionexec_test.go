package actionexec

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capture records what one request looked like on the wire.
type capture struct {
	method      string
	body        string
	contentType string
	userAgent   string
	custom      string
}

func serve(t *testing.T, status int, respBody string) (*httptest.Server, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.method = r.Method
		cap.body = string(b)
		cap.contentType = r.Header.Get("Content-Type")
		cap.userAgent = r.Header.Get("User-Agent")
		cap.custom = r.Header.Get("X-Custom")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

// TestHTTPRequestReconciledSemantics pins the unified http_request behavior the
// job worker and the trigger runner share: method defaulting, raw string body,
// JSON-marshaled non-string body, Content-Type only alongside a body, header overrides.
func TestHTTPRequestReconciledSemantics(t *testing.T) {
	ctx := context.Background()
	client := &http.Client{}

	t.Run("default_method_per_caller", func(t *testing.T) {
		for _, def := range []string{http.MethodGet, http.MethodPost} {
			srv, cap := serve(t, 200, "")
			if err := HTTPRequest(ctx, client, map[string]any{"url": srv.URL}, HTTPOptions{DefaultMethod: def}); err != nil {
				t.Fatalf("default %s: %v", def, err)
			}
			if cap.method != def {
				t.Errorf("default %s: request used %s", def, cap.method)
			}
		}
	})

	t.Run("explicit_method_uppercased", func(t *testing.T) {
		srv, cap := serve(t, 200, "")
		err := HTTPRequest(ctx, client, map[string]any{"url": srv.URL, "method": "put"}, HTTPOptions{DefaultMethod: http.MethodGet})
		if err != nil {
			t.Fatal(err)
		}
		if cap.method != http.MethodPut {
			t.Errorf("method %q not uppercased: got %s", "put", cap.method)
		}
	})

	t.Run("string_body_sent_raw", func(t *testing.T) {
		srv, cap := serve(t, 200, "")
		err := HTTPRequest(ctx, client, map[string]any{
			"url": srv.URL, "method": "POST", "body": `{"already":"json"}`,
		}, HTTPOptions{DefaultMethod: http.MethodPost})
		if err != nil {
			t.Fatal(err)
		}
		// Raw, not re-marshaled into a quoted JSON string.
		if cap.body != `{"already":"json"}` {
			t.Errorf("string body altered on the wire: %q", cap.body)
		}
		if cap.contentType != "application/json" {
			t.Errorf("Content-Type not defaulted with body present: %q", cap.contentType)
		}
	})

	t.Run("map_body_json_marshaled", func(t *testing.T) {
		srv, cap := serve(t, 200, "")
		err := HTTPRequest(ctx, client, map[string]any{
			"url": srv.URL, "body": map[string]any{"a": 1},
		}, HTTPOptions{DefaultMethod: http.MethodPost})
		if err != nil {
			t.Fatal(err)
		}
		if cap.body != `{"a":1}` {
			t.Errorf("map body not JSON-marshaled: %q", cap.body)
		}
	})

	t.Run("no_body_no_content_type", func(t *testing.T) {
		srv, cap := serve(t, 200, "")
		if err := HTTPRequest(ctx, client, map[string]any{"url": srv.URL}, HTTPOptions{DefaultMethod: http.MethodGet}); err != nil {
			t.Fatal(err)
		}
		if cap.contentType != "" {
			t.Errorf("bodyless request got Content-Type %q, want none", cap.contentType)
		}
		if cap.userAgent != "OpenFabrica-Action/1.0" {
			t.Errorf("User-Agent = %q, want OpenFabrica-Action/1.0", cap.userAgent)
		}
	})

	t.Run("custom_headers_override_defaults", func(t *testing.T) {
		srv, cap := serve(t, 200, "")
		err := HTTPRequest(ctx, client, map[string]any{
			"url":     srv.URL,
			"body":    `x`,
			"headers": map[string]any{"Content-Type": "text/plain", "X-Custom": 7},
		}, HTTPOptions{DefaultMethod: http.MethodPost})
		if err != nil {
			t.Fatal(err)
		}
		if cap.contentType != "text/plain" {
			t.Errorf("custom Content-Type not honored: %q", cap.contentType)
		}
		if cap.custom != "7" {
			t.Errorf("non-string header value not stringified: %q", cap.custom)
		}
	})

	t.Run("error_status_includes_snippet", func(t *testing.T) {
		srv, _ := serve(t, 502, "upstream down")
		err := HTTPRequest(ctx, client, map[string]any{"url": srv.URL}, HTTPOptions{DefaultMethod: http.MethodGet})
		if err == nil || !strings.Contains(err.Error(), "HTTP 502") || !strings.Contains(err.Error(), "upstream down") {
			t.Errorf("expected HTTP 502 error with body snippet, got %v", err)
		}
	})
}

// TestHTTPRequestGuards pins the pre-flight failures: a missing URL and a
// validator rejection must both error before any network call.
func TestHTTPRequestGuards(t *testing.T) {
	ctx := context.Background()
	client := &http.Client{}

	err := HTTPRequest(ctx, client, map[string]any{}, HTTPOptions{DefaultMethod: http.MethodGet})
	if err == nil || !strings.Contains(err.Error(), "'url' is required") {
		t.Errorf("expected url-required error, got %v", err)
	}

	called := false
	err = HTTPRequest(ctx, client, map[string]any{"url": "http://127.0.0.1:9/admin"}, HTTPOptions{
		DefaultMethod: http.MethodGet,
		ValidateURL: func(string) error {
			called = true
			return fmt.Errorf("resolved to private/internal IP")
		},
	})
	if !called {
		t.Error("ValidateURL was not invoked")
	}
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("expected blocked error, got %v", err)
	}
}

// TestSendEmailArgContract pins the arg shaping shared by both callers: the
// send-arg keys are copied through, action is forced to "send", and unrelated
// params never leak into the tool args.
func TestSendEmailArgContract(t *testing.T) {
	var gotTool string
	var gotArgs map[string]any
	err := SendEmail(map[string]any{
		"to":            "a@b.com",
		"subject":       "hi",
		"template_vars": map[string]any{"name": "Alice"},
		"unrelated":     "leak",
	}, func(toolName string, args map[string]any) error {
		gotTool, gotArgs = toolName, args
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotTool != "manage_email" {
		t.Errorf("dispatched to %q, want manage_email", gotTool)
	}
	if gotArgs["action"] != "send" || gotArgs["to"] != "a@b.com" || gotArgs["subject"] != "hi" {
		t.Errorf("send args not copied through: %v", gotArgs)
	}
	if _, leaked := gotArgs["unrelated"]; leaked {
		t.Errorf("unrelated param leaked into tool args: %v", gotArgs)
	}
	if len(gotArgs) != 4 { // action, to, subject, template_vars
		t.Errorf("unexpected arg set: %v", gotArgs)
	}
}
