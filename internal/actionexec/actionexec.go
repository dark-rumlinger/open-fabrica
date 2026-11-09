// Package actionexec holds the shared bodies of the generic outbound actions
// (send_email, http_request) that both the job worker and the event trigger
// runner execute, so the request shaping, SSRF pre-flight, and body/header rules
// cannot drift between the two call sites. Caller-specific seams are injected.
package actionexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// emailArgKeys are the manage_email send args copied from the action params.
// The list is the contract between the enqueue side (trigger configs, of_jobs
// payloads) and the manage_email tool's send action.
var emailArgKeys = [...]string{"to", "subject", "body_html", "body_text", "template_name", "template_vars"}

// SendEmail executes a send_email action: it shapes the manage_email "send" args
// from params and dispatches them through run, the caller's headless
// tool-execution seam. Only the arg contract is shared; the rest stays with each caller.
func SendEmail(params map[string]any, run func(toolName string, args map[string]any) error) error {
	args := map[string]any{"action": "send"}
	for _, k := range emailArgKeys {
		if v, ok := params[k]; ok {
			args[k] = v
		}
	}
	return run("manage_email", args)
}

// HTTPOptions carries the per-caller policy knobs for HTTPRequest.
type HTTPOptions struct {
	// DefaultMethod is used when params carries no "method". The job worker
	// defaults to GET and the trigger runner to POST; each caller passes its own
	// default so stored jobs and triggers keep their behavior.
	DefaultMethod string
	// ValidateURL is the SSRF pre-flight (netguard.ValidateExternalURL in
	// production). It is injected rather than imported because the trigger
	// runner's allowPrivateHosts mode must be able to skip it (nil = skip);
	// the job worker always validates.
	ValidateURL func(string) error
}

// HTTPRequest executes an http_request action or job against an external URL.
// params: {"url":"https://...", "method":"POST", "headers":{"X-Key":"val"}, "body":...}
// client must be the caller's SSRF-hardened client; ctx is the caller's
// lifecycle context so in-flight requests abort at shutdown.
//
// Shared behavior contract: a string body is sent as-is, any other non-nil value
// is JSON-marshaled; custom headers override the defaults; Content-Type defaults
// to application/json only when a body is present; User-Agent is
// OpenFabrica-Action/1.0; a >=400 response error includes a bounded body
// snippet, and the body is always drained so the connection can be reused.
func HTTPRequest(ctx context.Context, client *http.Client, params map[string]any, opts HTTPOptions) error {
	url, _ := params["url"].(string)
	if url == "" {
		return fmt.Errorf("http_request: 'url' is required")
	}
	if opts.ValidateURL != nil {
		if err := opts.ValidateURL(url); err != nil {
			return fmt.Errorf("http_request blocked: %w", err)
		}
	}
	method, _ := params["method"].(string)
	if method == "" {
		method = opts.DefaultMethod
	}

	var bodyReader io.Reader
	hasBody := false
	if raw, ok := params["body"]; ok && raw != nil {
		if s, isStr := raw.(string); isStr {
			if s != "" {
				bodyReader = strings.NewReader(s)
				hasBody = true
			}
		} else {
			data, err := json.Marshal(raw)
			if err != nil {
				return fmt.Errorf("http_request: marshaling body: %w", err)
			}
			bodyReader = bytes.NewReader(data)
			hasBody = true
		}
	}

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), url, bodyReader)
	if err != nil {
		return fmt.Errorf("http_request: %w", err)
	}
	req.Header.Set("User-Agent", "OpenFabrica-Action/1.0")
	if headers, ok := params["headers"].(map[string]any); ok {
		for k, v := range headers {
			req.Header.Set(k, fmt.Sprintf("%v", v))
		}
	}
	if hasBody && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http_request: %w", err)
	}
	defer resp.Body.Close()
	// Drain (bounded) so the connection can be reused; on failure the prefix
	// doubles as the error detail.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 400 {
		return fmt.Errorf("http_request: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
