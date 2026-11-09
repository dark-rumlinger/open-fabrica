package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/providers/llm"
)

// The real body Z.ai returns once an endpoint forces thinking on. Kept verbatim so
// the matcher is pinned against the wire format rather than a paraphrase of it.
const forcedThinkingBody = `{"error":{"code":"1210","message":"This model always engages in thinking and cannot be disabled; please use low, high, or max"}}`

func thinkingReq(model string, thinking bool) llm.CompletionRequest {
	return llm.CompletionRequest{
		Model:     model,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		MaxTokens: 8192,
		// The pipeline sends "medium" on every stage, an illegal level on the
		// strict reasoning-effort set, so it must never be forwarded as-is.
		Effort:   "medium",
		Thinking: &thinking,
	}
}

// classify400 builds the typed error the client would see, going through
// ClassifyHTTPError so tests exercise the real wrapping and code assignment.
func classify400(t *testing.T, body string) *llm.Error {
	t.Helper()
	return llm.ClassifyHTTPError("openai-chat", &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	})
}

// The two thinking keys are alternate spellings of one intent: Z.ai reads the
// canonical object, vLLM and OpenRouter passthroughs read the flat bool. A body
// setting one without the other means different things to different gateways.
func TestBuildRequestBody_ThinkingKeysAgree(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	c := &Client{name: "keys-provider"}

	off := c.buildRequestBody(thinkingReq("keys-off", false), false)
	if got := off["thinking"]; !equalThinking(got, "disabled") {
		t.Fatalf("thinking = %v, want {type: disabled}", got)
	}
	if got, ok := off["enable_thinking"].(bool); !ok || got {
		t.Fatalf("enable_thinking = %v, want false alongside disabled", off["enable_thinking"])
	}
	if _, ok := off["reasoning_effort"]; ok {
		t.Fatal(`reasoning_effort must not be sent on the ordinary path: the pipeline's "medium" is illegal on the strict set`)
	}

	on := c.buildRequestBody(thinkingReq("keys-on", true), false)
	if got := on["thinking"]; !equalThinking(got, "enabled") {
		t.Fatalf("thinking = %v, want {type: enabled}", got)
	}
	if got, ok := on["enable_thinking"].(bool); !ok || !got {
		t.Fatalf("enable_thinking = %v, want true alongside enabled", on["enable_thinking"])
	}

	// nil means no opinion and must leave provider defaults alone: non-reasoning
	// models on this transport 400 on the field.
	unset := c.buildRequestBody(llm.CompletionRequest{
		Model: "keys-nil", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, false)
	if _, ok := unset["thinking"]; ok {
		t.Fatal("thinking must be omitted when the caller expressed no preference")
	}
	if _, ok := unset["enable_thinking"]; ok {
		t.Fatal("enable_thinking must be omitted when the caller expressed no preference")
	}
}

// Once an endpoint refuses to let thinking be turned off, the caller's "off"
// cannot be honoured, so the request asks for the cheapest legal level. The
// request still says Thinking=false and Effort="medium": the store overrides
// both, because "medium" is exactly the value the strict set rejects.
func TestBuildRequestBody_ForcedThinkingRendersEnabledLow(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	c := &Client{name: "forced-provider"}
	llm.Caps.Learn(c.CapKey("forced-low"), llm.CapThinkingOff, llm.No)

	body := c.buildRequestBody(thinkingReq("forced-low", false), false)
	if got := body["thinking"]; !equalThinking(got, "enabled") {
		t.Fatalf("thinking = %v, want {type: enabled} once the endpoint forces it", got)
	}
	if got, ok := body["enable_thinking"].(bool); !ok || !got {
		t.Fatalf("enable_thinking = %v, want true", body["enable_thinking"])
	}
	if got := body["reasoning_effort"]; got != "low" {
		t.Fatalf(`reasoning_effort = %v, want low (cheapest legal level, not the request's "medium")`, got)
	}

	// Second rung: an endpoint that forces thinking but has no effort field.
	llm.Caps.Learn(c.CapKey("forced-plain"), llm.CapThinkingOff, llm.No)
	llm.Caps.Learn(c.CapKey("forced-plain"), llm.CapReasoningEffort, llm.No)
	plain := c.buildRequestBody(thinkingReq("forced-plain", false), false)
	if got := plain["thinking"]; !equalThinking(got, "enabled") {
		t.Fatalf("thinking = %v, want {type: enabled}", got)
	}
	if _, ok := plain["reasoning_effort"]; ok {
		t.Fatal("reasoning_effort must be dropped once the endpoint has rejected the field itself")
	}
}

// A strict endpoint that rejects the toggle field itself (OpenAI-style "Unknown
// parameter") is learned as CapThinkingField=No, after which the body omits both
// keys in BOTH directions and the call rides provider defaults instead of
// 400-looping.
func TestBuildRequestBody_UnknownThinkingFieldOmitsKeys(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	c := &Client{name: "strict-provider"}
	llm.Caps.Learn(c.CapKey("strict-model"), llm.CapThinkingField, llm.No)

	for _, want := range []bool{true, false} {
		body := c.buildRequestBody(thinkingReq("strict-model", want), false)
		if _, ok := body["thinking"]; ok {
			t.Fatalf("thinking=%v: the toggle object must be omitted once the field is learned unknown", want)
		}
		if _, ok := body["enable_thinking"]; ok {
			t.Fatalf("thinking=%v: enable_thinking must be omitted once the field is learned unknown", want)
		}
	}
}

// The unknown-field matcher fires on strict-endpoint rejections of the field
// itself and never on the forced-thinking rejection, which is the other rule's
// signature (they set different capabilities and must not race).
func TestMatchesUnknownThinkingField(t *testing.T) {
	real := classify400(t, `{"error":{"message":"Unknown parameter: 'thinking'.","type":"invalid_request_error"}}`)
	if !matchesUnknownThinkingField(real) {
		t.Fatal("OpenAI unknown-parameter rejection not detected")
	}
	pydantic := classify400(t, `{"error":{"message":"1 validation error for ChatCompletionRequest\nthinking\n  extra fields not permitted (type=value_error.extra)"}}`)
	if !matchesUnknownThinkingField(pydantic) {
		t.Fatal("pydantic extra-fields rejection not detected")
	}
	if matchesUnknownThinkingField(classify400(t, forcedThinkingBody)) {
		t.Fatal("a forced-thinking rejection must stay with the CapThinkingOff rule")
	}
	if matchesUnknownThinkingField(classify400(t, `{"error":{"message":"Unknown parameter: 'frobnicate'."}}`)) {
		t.Fatal("an unknown-parameter complaint about a different field must not fire")
	}
	// Error messages carry the provider tag; a provider NAMED with "thinking"
	// must not convert an unrelated field complaint into a demotion.
	tagged := &llm.Error{Code: llm.CodeBadRequest,
		Message: "thinking-two-rung: Unrecognized request argument supplied: reasoning_effort"}
	if matchesUnknownThinkingField(tagged) {
		t.Fatal("a provider tag containing 'thinking' must not satisfy the field-name anchor")
	}
	if matchesUnknownThinkingField(&llm.Error{Code: llm.CodeServer, Message: "unknown parameter: 'thinking'"}) {
		t.Fatal("a 500 echoing the wording must not latch the capability off")
	}
}

// Scoping is per (provider row, model). One client serves every model of a row,
// and the same model name under two rows is two deployments, so neither a
// per-client nor a bare-model-name latch would hold.
func TestBuildRequestBody_ForcedThinkingIsPerProviderAndModel(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	cA := &Client{name: "row-a"}
	cB := &Client{name: "row-b"}
	llm.Caps.Learn(cA.CapKey("scoped-model"), llm.CapThinkingOff, llm.No)

	if got := cA.buildRequestBody(thinkingReq("scoped-other", false), false)["thinking"]; !equalThinking(got, "disabled") {
		t.Fatalf("sibling model on the same row: thinking = %v, want {type: disabled}", got)
	}
	if got := cB.buildRequestBody(thinkingReq("scoped-model", false), false)["thinking"]; !equalThinking(got, "disabled") {
		t.Fatalf("same model on a different row: thinking = %v, want {type: disabled}", got)
	}
	if got := cA.buildRequestBody(thinkingReq("scoped-model", false), false)["thinking"]; !equalThinking(got, "enabled") {
		t.Fatalf("demoted pair: thinking = %v, want {type: enabled}", got)
	}
}

// The matcher must fire on the real rejection and stay off everything else: a
// false positive silently turns reasoning ON for a model the caller wanted it
// off for, billing hidden chain-of-thought against the output budget.
func TestMatchesForcedThinking(t *testing.T) {
	// Also pins that this body classifies CodeBadRequest, not CodeSchemaUnsupported:
	// it names no schema field, so it must not be routed into the json_schema ladder.
	real := classify400(t, forcedThinkingBody)
	if real.Code != llm.CodeBadRequest {
		t.Fatalf("1210 body classified as %q, want CodeBadRequest", real.Code)
	}
	if !matchesForcedThinking(real) {
		t.Fatal("the real Z.ai 1210 body was not detected as a forced-thinking rejection")
	}

	negatives := []struct {
		name string
		err  *llm.Error
	}{
		{"rate limit", &llm.Error{Code: llm.CodeRateLimit, Message: forcedThinkingBody}},
		{"server error echoing the body", &llm.Error{Code: llm.CodeServer, Message: forcedThinkingBody}},
		{"unrelated 400", &llm.Error{Code: llm.CodeBadRequest, Message: "Unknown parameter: frobnicate"}},
		// Names thinking but is a different complaint entirely.
		{"thinking without disable wording", &llm.Error{Code: llm.CodeBadRequest, Message: "thinking budget must be a positive integer"}},
	}
	for _, tc := range negatives {
		if matchesForcedThinking(tc.err) {
			t.Errorf("%s: misdetected as a forced-thinking rejection", tc.name)
		}
	}
}

// recordingServer captures every request body and replies with the caller's script.
func recordingServer(t *testing.T, reply func(n int, body map[string]any) (int, string)) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request %d: %v", len(bodies)+1, err)
		}
		bodies = append(bodies, body)
		status, payload := reply(len(bodies), body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

const okCompletion = `{"id":"1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

// The end-to-end contract: a run must not die on this 400. Before the learner
// existed it surfaced as CodeBadRequest, which the engine treats as fatal,
// pausing the pipeline on its very first call.
func TestComplete_LearnsForcedThinkingAndRetries(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	srv, bodies := recordingServer(t, func(n int, body map[string]any) (int, string) {
		if thinkingTypeOf(body) == "disabled" {
			return http.StatusBadRequest, forcedThinkingBody
		}
		return http.StatusOK, okCompletion
	})
	c := New(llm.Options{Name: "thinking-complete", APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()})

	resp, err := c.Complete(context.Background(), thinkingReq("complete-forced", false))
	if err != nil {
		t.Fatalf("Complete returned %v, want the retry to succeed", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("Content = %q, want ok", resp.Content)
	}
	if len(*bodies) != 2 {
		t.Fatalf("%d requests, want 2 (one rejected, one retried)", len(*bodies))
	}
	retry := (*bodies)[1]
	if thinkingTypeOf(retry) != "enabled" {
		t.Fatalf("retry thinking = %v, want enabled", retry["thinking"])
	}
	if retry["reasoning_effort"] != "low" {
		t.Fatalf("retry reasoning_effort = %v, want low", retry["reasoning_effort"])
	}

	// Learned: the next call must not re-pay the rejected request.
	if _, err := c.Complete(context.Background(), thinkingReq("complete-forced", false)); err != nil {
		t.Fatalf("second Complete returned %v", err)
	}
	if len(*bodies) != 3 {
		t.Fatalf("%d requests total, want 3 - the second call should cost exactly one", len(*bodies))
	}
	if thinkingTypeOf((*bodies)[2]) != "enabled" {
		t.Fatal("second call re-sent disabled thinking: the store did not hold")
	}
}

// Every built app's LLM proxy (server/public/llm_endpoint.go) sends thinking off
// and calls Stream, so the streaming path needs the same recovery.
func TestStream_LearnsForcedThinkingAndRetries(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	const sse = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	srv, bodies := recordingServer(t, func(n int, body map[string]any) (int, string) {
		if thinkingTypeOf(body) == "disabled" {
			return http.StatusBadRequest, forcedThinkingBody
		}
		return http.StatusOK, sse
	})
	c := New(llm.Options{Name: "thinking-stream", APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()})

	var text strings.Builder
	err := c.Stream(context.Background(), thinkingReq("stream-forced", false), func(ch llm.StreamChunk) {
		text.WriteString(ch.Delta)
	})
	if err != nil {
		t.Fatalf("Stream returned %v, want the retry to succeed", err)
	}
	if text.String() != "hi" {
		t.Fatalf("streamed content = %q, want hi", text.String())
	}
	if len(*bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(*bodies))
	}
	if thinkingTypeOf((*bodies)[1]) != "enabled" {
		t.Fatalf("retry thinking = %v, want enabled", (*bodies)[1]["thinking"])
	}
}

// Some deployments force thinking on but have no reasoning_effort field. The
// second rung keeps thinking enabled and drops the level.
func TestComplete_RetryDropsEffortOnSecondRejection(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	srv, bodies := recordingServer(t, func(n int, body map[string]any) (int, string) {
		if thinkingTypeOf(body) == "disabled" {
			return http.StatusBadRequest, forcedThinkingBody
		}
		if _, ok := body["reasoning_effort"]; ok {
			return http.StatusBadRequest, `{"error":{"message":"Unrecognized request argument supplied: reasoning_effort"}}`
		}
		return http.StatusOK, okCompletion
	})
	c := New(llm.Options{Name: "thinking-two-rung", APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()})

	if _, err := c.Complete(context.Background(), thinkingReq("two-rung", false)); err != nil {
		t.Fatalf("Complete returned %v, want both rungs to be walked", err)
	}
	if len(*bodies) != 3 {
		t.Fatalf("%d requests, want 3 (disabled -> enabled+effort -> enabled)", len(*bodies))
	}
	final := (*bodies)[2]
	if thinkingTypeOf(final) != "enabled" {
		t.Fatalf("final thinking = %v, want enabled", final["thinking"])
	}
	if _, ok := final["reasoning_effort"]; ok {
		t.Fatal("final request still carried reasoning_effort after the endpoint rejected it")
	}
}

// equalThinking reports whether a rendered `thinking` value is {"type": want}.
func equalThinking(v any, want string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	return m["type"] == want
}

func thinkingTypeOf(body map[string]any) string {
	m, ok := body["thinking"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["type"].(string)
	return s
}
