// Package httpclient is the shared outbound-HTTP helper used by messaging and
// payments tools when calling third-party providers. It bundles request build,
// credential injection, bounded response read, and HTTP-status classification
// so action handlers stay focused on payload shape and response parsing.
package httpclient

import (
	"io"
	"net/http"
	"strings"
	"time"

	"openfabrica/tools"
	"openfabrica/tools/internal/provider"
)

// defaultTimeout bounds each outbound provider call. The shared value keeps
// payments and messaging requests on identical timeout behavior.
const defaultTimeout = 30 * time.Second

// maxResponseBody bounds the response body read so a chatty provider can't
// pin tool memory. 16 KiB matches the payments limit and comfortably contains
// messaging's smaller 8 KiB need.
const maxResponseBody = 16384

// PostWithAuth issues a POST to url with the given body+contentType, injecting
// the provider's Authorization-style header before send. basicBase is forwarded
// to provider.InjectAuth - pass "" for payments-style providers and the
// account SID (or equivalent) for Twilio-style HTTP Basic providers.
//
// Returns the response body bytes and HTTP status on success. On transport
// errors or HTTP status >= 400 the *tools.Result carries a Fail message the
// caller can return to the LLM unchanged; body and status are zero-valued in
// that case (except when the failure is a non-2xx status, in which case the
// upstream body is included in the Fail message).
func PostWithAuth(ctx *tools.ToolContext, c *provider.Creds, url, body, contentType, basicBase string) ([]byte, int, *tools.Result) {
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return nil, 0, tools.Fail("creating request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	return DoWithAuth(ctx, req, c, basicBase)
}

// DoWithAuth handles the (inject auth -> send -> bounded read -> classify)
// path for any caller-built *http.Request. Use this for verbs other than POST
// (DELETE, GET) where the caller has set the method, URL, headers, and body
// itself. Returns identical (body, status, *tools.Result) tuple semantics as
// PostWithAuth.
func DoWithAuth(ctx *tools.ToolContext, req *http.Request, c *provider.Creds, basicBase string) ([]byte, int, *tools.Result) {
	if r := provider.InjectAuth(ctx, req, c, basicBase); r != nil {
		return nil, 0, r
	}
	client := &http.Client{Timeout: defaultTimeout}
	resp, err := client.Do(req) // #nosec G704 -- req targets a first-party provider's canonical API endpoint from validated creds, not a user-supplied URL
	if err != nil {
		return nil, 0, tools.Fail("calling provider: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, 0, tools.Fail("reading provider response: %v", err)
	}
	if resp.StatusCode >= 400 {
		return respBody, resp.StatusCode, tools.Fail("provider returned %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, resp.StatusCode, nil
}
