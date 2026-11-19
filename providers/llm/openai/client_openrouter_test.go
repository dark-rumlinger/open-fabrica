package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/internal/providercfg"
	"openfabrica/providers/llm"
)

func TestBuildRequestBody_OpenRouterEffort(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	c := &Client{
		name: "openrouter-provider",
		cfg:  providercfg.Config{ModelsShape: providercfg.ShapeOpenRouter},
	}

	// Thinking=true + Effort="high" -> {"enabled": true, "effort": "high"}
	reqOn := thinkingReq("or-effort-on", true)
	reqOn.Effort = "high"
	on := c.buildRequestBody(reqOn, false)
	if r, ok := on["reasoning"].(map[string]any); !ok || r["enabled"] != true || r["effort"] != "high" {
		t.Fatalf("reasoning = %v, want {enabled: true, effort: high}", on["reasoning"])
	}

	// Thinking=nil + Effort="medium" -> {"effort": "medium"}
	reqNil := llm.CompletionRequest{
		Model:    "or-effort-nil",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Effort:   "medium",
	}
	nilBody := c.buildRequestBody(reqNil, false)
	if r, ok := nilBody["reasoning"].(map[string]any); !ok || r["effort"] != "medium" {
		t.Fatalf("reasoning = %v, want {effort: medium}", nilBody["reasoning"])
	}
	if _, hasEnabled := nilBody["reasoning"].(map[string]any)["enabled"]; hasEnabled {
		t.Fatalf("reasoning should not set enabled when Thinking is nil, got %v", nilBody["reasoning"])
	}

	// Thinking=false ignores Effort (stays disabled).
	reqOff := thinkingReq("or-effort-off", false)
	reqOff.Effort = "high"
	off := c.buildRequestBody(reqOff, false)
	if r, ok := off["reasoning"].(map[string]any); !ok || r["enabled"] != false || r["effort"] != nil {
		t.Fatalf("reasoning = %v, want {enabled: false} without effort", off["reasoning"])
	}
}

func TestBuildRequestBody_OpenRouterWebSearchAndRequireParameters(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	orClient := &Client{
		name: "openrouter-provider",
		cfg:  providercfg.Config{ModelsShape: providercfg.ShapeOpenRouter},
	}

	// OpenRouter emits openrouter:web_search inside tools[] and sets provider.require_parameters=true.
	orBody := orClient.buildRequestBody(llm.CompletionRequest{
		Model:    "openai/gpt-4o",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "search"}},
		Tools:    []llm.ToolDef{{Type: "web_search"}},
	}, false)
	if _, hasWSOpts := orBody["web_search_options"]; hasWSOpts {
		t.Fatalf("openrouter must not emit web_search_options, got %v", orBody["web_search_options"])
	}
	tools, ok := orBody["tools"].([]map[string]any)
	if !ok || len(tools) != 1 || tools[0]["type"] != "openrouter:web_search" {
		t.Fatalf("tools = %v, want [{type: openrouter:web_search}]", orBody["tools"])
	}
	prov, ok := orBody["provider"].(map[string]any)
	if !ok || prov["require_parameters"] != true {
		t.Fatalf("provider = %v, want {require_parameters: true}", orBody["provider"])
	}

	// Plain OpenRouter request without tools/response_format/reasoning omits provider.require_parameters.
	plainBody := orClient.buildRequestBody(llm.CompletionRequest{
		Model:    "openai/gpt-4o",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, false)
	if _, hasProv := plainBody["provider"]; hasProv {
		t.Fatalf("plain openrouter request must not emit provider, got %v", plainBody["provider"])
	}

	// Non-OpenRouter OpenAI client keeps web_search_options and omits provider.
	oaiClient := &Client{name: "openai-provider"}
	oaiBody := oaiClient.buildRequestBody(llm.CompletionRequest{
		Model:    "gpt-4o-search-preview",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "search"}},
		Tools:    []llm.ToolDef{{Type: "web_search"}},
	}, false)
	if _, hasWSOpts := oaiBody["web_search_options"]; !hasWSOpts {
		t.Fatal("non-openrouter client should emit web_search_options")
	}
	if _, hasProv := oaiBody["provider"]; hasProv {
		t.Fatalf("non-openrouter client must not emit provider, got %v", oaiBody["provider"])
	}
}

func TestParseSSEStream_OpenRouterReasoningDetails(t *testing.T) {
	c := &Client{name: "openrouter-provider"}
	sse := strings.Join([]string{
		": OPENROUTER PROCESSING",
		`data: {"id":"1","choices":[{"index":0,"delta":{"reasoning":"thinking...","reasoning_details":[{"type":"reasoning.encrypted","data":"abc"}]}}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		"data: [DONE]",
	}, "\n") + "\n"

	var chunks []llm.StreamChunk
	err := c.parseSSEStream(strings.NewReader(sse), func(ch llm.StreamChunk) {
		chunks = append(chunks, ch)
	})
	if err != nil {
		t.Fatalf("parseSSEStream returned %v", err)
	}

	var gotDetails json.RawMessage
	for _, ch := range chunks {
		if len(ch.ReasoningDetails) > 0 {
			gotDetails = ch.ReasoningDetails
		}
	}
	want := `[{"type":"reasoning.encrypted","data":"abc"}]`
	if string(gotDetails) != want {
		t.Fatalf("ReasoningDetails = %s, want %s", string(gotDetails), want)
	}
}

func TestParseSSEStream_OpenRouterTypedMidStreamError(t *testing.T) {
	c := &Client{name: "openrouter-provider"}
	sse := `data: {"id":"1","error":{"code":429,"message":"Rate limit exceeded"},"choices":[{"index":0,"delta":{},"finish_reason":"error"}]}` + "\n"

	var cbErr error
	err := c.parseSSEStream(strings.NewReader(sse), func(ch llm.StreamChunk) {
		if ch.Error != nil {
			cbErr = ch.Error
		}
	})
	if err == nil || cbErr == nil {
		t.Fatalf("expected error from parseSSEStream, got err=%v cbErr=%v", err, cbErr)
	}
	lerr, ok := llm.AsLLMError(err)
	if !ok {
		t.Fatalf("expected *llm.Error, got %T (%v)", err, err)
	}
	if lerr.Code != llm.CodeRateLimit || lerr.Status != 429 {
		t.Fatalf("got Code=%q Status=%d, want Code=%q Status=429", lerr.Code, lerr.Status, llm.CodeRateLimit)
	}
}
