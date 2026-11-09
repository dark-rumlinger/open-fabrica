package openai

import (
	"context"
	"net/http"
	"testing"

	"openfabrica/providers/llm"
)

// Bodies as OpenAI returns them for a reasoning model, kept verbatim so the
// matchers are pinned against the wire format.
const (
	maxTokensRejected   = `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`
	temperatureRejected = `{"error":{"message":"Unsupported value: 'temperature' does not support 0.2 with this model. Only the default (1) value is supported.","type":"invalid_request_error","param":"temperature","code":"unsupported_value"}}`
)

func reasoningModelServer(t *testing.T) (*Client, *[]map[string]any) {
	t.Helper()
	srv, bodies := recordingServer(t, func(n int, body map[string]any) (int, string) {
		if _, ok := body["max_tokens"]; ok {
			return http.StatusBadRequest, maxTokensRejected
		}
		if _, ok := body["temperature"]; ok {
			return http.StatusBadRequest, temperatureRejected
		}
		return http.StatusOK, okCompletion
	})
	return New(llm.Options{Name: "reasoning-params", APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()}), bodies
}

// The run must not die on a reasoning model's first call: both fields are learned
// off from the 400s and the request is re-sent in the shape the model accepts.
func TestComplete_LearnsReasoningModelParams(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	c, bodies := reasoningModelServer(t)
	temp := 0.2
	resp, err := c.Complete(context.Background(), llm.CompletionRequest{
		Model:       "o-reasoner",
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		MaxTokens:   512,
		Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	final := (*bodies)[len(*bodies)-1]
	if final["max_completion_tokens"] != float64(512) {
		t.Errorf("final body max_completion_tokens = %v, want 512", final["max_completion_tokens"])
	}
	if _, ok := final["max_tokens"]; ok {
		t.Error("final body must not carry max_tokens")
	}
	if _, ok := final["temperature"]; ok {
		t.Error("final body must not carry temperature")
	}

	// Learned per model: the next call is right the first time.
	before := len(*bodies)
	if _, err := c.Complete(context.Background(), llm.CompletionRequest{
		Model:       "o-reasoner",
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "again"}},
		MaxTokens:   512,
		Temperature: &temp,
	}); err != nil {
		t.Fatal(err)
	}
	if len(*bodies)-before != 1 {
		t.Errorf("second call took %d requests, want 1", len(*bodies)-before)
	}
}

// An ordinary chat model keeps the classic fields.
func TestBuildRequestBody_ClassicModelKeepsMaxTokens(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	c := &Client{name: "classic"}
	temp := 0.5
	body := c.buildRequestBody(llm.CompletionRequest{
		Model: "gpt-classic", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		MaxTokens: 100, Temperature: &temp,
	}, false)
	if body["max_tokens"] != 100 || body["temperature"] != 0.5 {
		t.Errorf("classic body = %v", body)
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Error("classic model must not get max_completion_tokens")
	}
}

func TestQuirkMatchersAreNarrow(t *testing.T) {
	cases := []struct {
		name string
		body string
		fn   func(*llm.Error) bool
		want bool
	}{
		{"max_tokens", maxTokensRejected, matchesMaxTokensField, true},
		{"max_tokens vs temperature body", temperatureRejected, matchesMaxTokensField, false},
		{"temperature", temperatureRejected, matchesSamplingParams, true},
		{"temperature vs max_tokens body", maxTokensRejected, matchesSamplingParams, false},
		{"unrelated 400", `{"error":{"message":"messages: invalid role"}}`, matchesSamplingParams, false},
	}
	for _, tc := range cases {
		if got := tc.fn(classify400(t, tc.body)); got != tc.want {
			t.Errorf("%s: match = %v, want %v", tc.name, got, tc.want)
		}
	}
}
