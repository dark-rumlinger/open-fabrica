package llm

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// makeResp builds a minimal *http.Response with the given status and body for
// feeding ClassifyHTTPError.
func makeResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Header:     make(http.Header),
	}
}

// CodeSchemaUnsupported and CodeBadRequest route very differently: the first
// drives the json_schema -> json_object -> none ladder and is the ONLY fatal
// code the engine's provider fallback still honours, while the second is what
// every provider-quirk learner is gated on. A 400 that merely mentions a schema
// must not be mistaken for the endpoint refusing structured output.
func TestClassifyHTTPError_SchemaRejectionNamesTheField(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "response_format refusal is a schema rejection",
			body: `{"error":{"message":"Invalid parameter: 'response_format' of type 'json_schema' is not supported"}}`,
			want: CodeSchemaUnsupported,
		},
		{
			name: "json_schema refusal is a schema rejection",
			body: `{"error":{"message":"json_schema is not supported with this model"}}`,
			want: CodeSchemaUnsupported,
		},
		{
			// The exact Anthropic body behind sanitizeToolInputSchema. It complains
			// about the schema's CONTENT; the fix is to reshape the request, not to
			// degrade the response format or fail over to another provider.
			name: "anthropic top-level combinator complaint stays bad_request",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"tools.0.custom.input_schema: input_schema does not support oneOf, allOf, or anyOf at the top level"}}`,
			want: CodeBadRequest,
		},
		{
			name: "unrelated 400 naming a schema field stays bad_request",
			body: `{"error":{"message":"schema validation failed for argument 'city'"}}`,
			want: CodeBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyHTTPError("test", makeResp(http.StatusBadRequest, tc.body))
			if got.Code != tc.want {
				t.Errorf("Code = %q, want %q", got.Code, tc.want)
			}
		})
	}
}

func TestClassifyHTTPError_QuotaVsRateLimit(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "account quota exceeded is quota, not rate limit",
			body: `{"error":{"code":"AccountQuotaExceeded","message":"You have exceeded the 5-hour usage quota. It will reset at 2026-06-10 18:42:05 +0800 CST.","type":"TooManyRequests"}}`,
			want: CodeQuotaExceeded,
		},
		{
			name: "insufficient_quota is quota",
			body: `{"error":{"type":"insufficient_quota","message":"You exceeded your current quota"}}`,
			want: CodeQuotaExceeded,
		},
		{
			name: "plain per-minute throttle stays rate_limit",
			body: `{"error":{"message":"Rate limit reached for requests","type":"requests"}}`,
			want: CodeRateLimit,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := ClassifyHTTPError("openai-chat", makeResp(http.StatusTooManyRequests, tc.body))
			if e.Code != tc.want {
				t.Fatalf("code = %q, want %q", e.Code, tc.want)
			}
		})
	}
}

func TestClassifyHTTPError_QuotaResetParsed(t *testing.T) {
	// A reset comfortably in the future should yield a positive RetryAfter
	// uncapped by the 5-minute Retry-After header ceiling.
	future := time.Now().Add(3 * time.Hour).Format("2006-01-02 15:04:05 -0700")
	body := `{"error":{"code":"AccountQuotaExceeded","message":"quota exhausted. It will reset at ` + future + `."}}`
	e := ClassifyHTTPError("openai-chat", makeResp(http.StatusTooManyRequests, body))
	if e.Code != CodeQuotaExceeded {
		t.Fatalf("code = %q, want %q", e.Code, CodeQuotaExceeded)
	}
	if e.RetryAfter < 2*time.Hour || e.RetryAfter > 3*time.Hour+time.Minute {
		t.Fatalf("RetryAfter = %v, want ~3h", e.RetryAfter)
	}
}

func TestParseQuotaReset_NoTimestamp(t *testing.T) {
	if d := parseQuotaReset("quota exceeded, try later"); d != 0 {
		t.Fatalf("want 0 for missing timestamp, got %v", d)
	}
	// A reset already in the past collapses to 0 rather than a negative span.
	past := time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05 -0700")
	if d := parseQuotaReset("reset at " + past); d != 0 {
		t.Fatalf("want 0 for past timestamp, got %v", d)
	}
}

func TestReadErrorBody(t *testing.T) {
	// A normal small body is returned verbatim.
	if got := ReadErrorBody(makeResp(http.StatusBadRequest, "boom")); got != "boom" {
		t.Fatalf("small body = %q, want %q", got, "boom")
	}

	// An oversized body is bounded to maxErrorBody, not read whole.
	big := strings.Repeat("x", maxErrorBody+4096)
	got := ReadErrorBody(makeResp(http.StatusInternalServerError, big))
	if len(got) != maxErrorBody {
		t.Fatalf("oversized body length = %d, want %d (capped)", len(got), maxErrorBody)
	}

	// nil response and nil body are safe and yield "".
	if got := ReadErrorBody(nil); got != "" {
		t.Fatalf("nil resp = %q, want empty", got)
	}
	if got := ReadErrorBody(&http.Response{}); got != "" {
		t.Fatalf("nil body = %q, want empty", got)
	}
}

func TestClassifyHTTPError_PermanentStatusesAreNotRetriedAsServerFaults(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"402 out of credits", http.StatusPaymentRequired, `{"error":{"message":"Insufficient credits. Add more at openrouter.ai/credits"}}`, CodeQuotaExceeded},
		{"404 unknown model", http.StatusNotFound, `{"error":{"message":"model 'x' not found"}}`, CodeBadRequest},
		{"413 body too large", http.StatusRequestEntityTooLarge, `{"error":{"message":"request too large"}}`, CodeBadRequest},
		{"422 rejected field", http.StatusUnprocessableEntity, `{"detail":[{"msg":"extra fields not permitted"}]}`, CodeBadRequest},
		{"408 request timeout", http.StatusRequestTimeout, `timeout`, CodeTimeout},
		{"500 stays server", http.StatusInternalServerError, `oops`, CodeServer},
	}
	for _, tc := range cases {
		if got := ClassifyHTTPError("openai-chat", makeResp(tc.status, tc.body)); got.Code != tc.want {
			t.Errorf("%s: code = %q, want %q", tc.name, got.Code, tc.want)
		}
	}
}

func TestClassifyHTTPError_QuotaWordingAcrossProviders(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"zai insufficient balance", `{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`, CodeQuotaExceeded},
		{"usage limit reached", `{"error":{"message":"Usage limit reached for this plan"}}`, CodeQuotaExceeded},
		{"gemini per-minute quota is a throttle", `{"error":{"code":429,"message":"Quota exceeded for metric generate_content_requests_per_minute","details":[{"retryDelay":"23s"}]}}`, CodeRateLimit},
		{"gemini daily quota is a lockout", `{"error":{"code":429,"message":"Quota exceeded for metric GenerateRequestsPerDayPerProjectPerModel"}}`, CodeQuotaExceeded},
	}
	for _, tc := range cases {
		if got := ClassifyHTTPError("openai-chat", makeResp(http.StatusTooManyRequests, tc.body)); got.Code != tc.want {
			t.Errorf("%s: code = %q, want %q", tc.name, got.Code, tc.want)
		}
	}
}

// A prompt that does not fit the model's window is its own code: the remedy is a
// smaller context (a rollover), unlike every other bad request. It must not be
// swallowed by the generic bad-request path (fatal, pauses the build), and request-body
// or file-size rejections that merely say "too large" must stay bad requests.
func TestClassifyHTTPError_ContextOverflow(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"openai", 400, `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 128000 tokens."}}`, CodeContextOverflow},
		{"anthropic", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 213000 tokens > 200000 maximum"}}`, CodeContextOverflow},
		{"openrouter 413", 413, `{"error":{"message":"input is too long for this model"}}`, CodeContextOverflow},
		{"plain bad request", 400, `{"error":{"message":"unknown parameter: top_k"}}`, CodeBadRequest},
		{"body too large", 413, `{"error":{"message":"request entity too large"}}`, CodeBadRequest},
		{"unknown model", 404, `{"error":{"message":"model not found"}}`, CodeBadRequest},
	}
	for _, tc := range cases {
		if got := ClassifyHTTPError("test", makeResp(tc.status, tc.body)).Code; got != tc.want {
			t.Errorf("%s: code = %q, want %q", tc.name, got, tc.want)
		}
	}
}
