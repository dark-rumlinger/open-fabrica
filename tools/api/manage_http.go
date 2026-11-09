package api

import "openfabrica/tools"

// manage_http is the one-shot outbound-HTTP escape hatch: a generic
// request action that performs a single HTTP call against an external URL.
// Handler lives in http_handlers.go.

func init() {
	tools.Define(tools.Tool{
		Name: "manage_http",
		// Sized to the tool's own output: the 3000-byte default cut it mid-JSON.
		MaxResult: 12000,
		Category:  "api",
		OneLiner:  "Make one-off outbound HTTP requests with security guardrails.",
		WhenToUse: `Use for truly one-shot external HTTP calls that don't justify a
manage_providers registration. Hard caps: 60s timeout; private IP ranges,
localhost, link-local, and cloud-metadata addresses are blocked before the
request fires. For repeated calls to one API, register it with manage_providers
instead so credentials live in the secrets store and you get shared
rate-limit handling.`,
		Actions: []tools.Action{
			&HTTPRequest{},
		},
	})
}

// HTTPRequest is the lone action on this tool.
type HTTPRequest struct {
	URL            string         `arg:"url,required"   desc:"Target URL."`
	Method         string         `arg:"method"         desc:"HTTP method." enum:"GET,HEAD,POST,PUT,DELETE,PATCH" default:"GET"`
	RequestBody    any            `arg:"request_body"   desc:"Raw string body OR object (JSON-encoded with Content-Type: application/json)."`
	Headers        map[string]any `arg:"headers"        desc:"Extra request headers."`
	TimeoutSeconds int            `arg:"timeout_seconds" desc:"Per-request timeout in seconds (max 60)." default:"30" max:"60"`
	StripHTML      bool           `arg:"strip_html"     desc:"Strip HTML tags and return clean text."`
	ParseFeed      bool           `arg:"parse_feed"     desc:"Parse response as RSS/Atom."`
}

func (a *HTTPRequest) Doc() string {
	return "Perform one HTTP request and return status, headers, and body. The body in the result is capped at 8 KB (body_truncated:true when trimmed); feed parsing and strip_html read up to 100 KB."
}
func (a *HTTPRequest) Example() any {
	return &HTTPRequest{URL: "https://example.com/api/status", Method: "GET"}
}
func (a *HTTPRequest) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return httpExecute(ctx, a.URL, a.Method, a.RequestBody, a.Headers, a.TimeoutSeconds, a.StripHTML, a.ParseFeed)
}
