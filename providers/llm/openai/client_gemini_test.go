package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/internal/providercfg"
	"openfabrica/providers/llm"
)

func TestGeminiReasoningMode(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	c := &Client{
		name: "Google Gemini",
		cfg:  providercfg.Config{ModelsShape: providercfg.ShapeGemini},
	}

	on := true
	off := false

	// 1. thinking=true with effort -> reasoning_effort set, no Z.ai/DeepSeek thinking fields.
	body := c.buildRequestBody(llm.CompletionRequest{
		Model:    "gemini-4-neon",
		Thinking: &on,
		Effort:   "high",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, false)
	if body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", body["reasoning_effort"])
	}
	if _, has := body["thinking"]; has {
		t.Fatalf("gemini shape must not emit thinking field: %v", body["thinking"])
	}
	if _, has := body["enable_thinking"]; has {
		t.Fatalf("gemini shape must not emit enable_thinking field: %v", body["enable_thinking"])
	}

	// 2. thinking=false when CapThinkingOff is allowed -> reasoning_effort = "none".
	offBody := c.buildRequestBody(llm.CompletionRequest{
		Model:    "gemini-2.5-flash",
		Thinking: &off,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, false)
	if offBody["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort = %v, want none", offBody["reasoning_effort"])
	}

	// 3. thinking=false when CapThinkingOff=No (Gemini 4 Neon / 3.x / 2.5 Pro) -> forcedReasoningEffort ("low").
	llm.Caps.Learn(c.CapKey("gemini-4-neon"), llm.CapThinkingOff, llm.No)
	forcedBody := c.buildRequestBody(llm.CompletionRequest{
		Model:    "gemini-4-neon",
		Thinking: &off,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, false)
	if forcedBody["reasoning_effort"] != forcedReasoningEffort {
		t.Fatalf("reasoning_effort = %v, want %q", forcedBody["reasoning_effort"], forcedReasoningEffort)
	}
}

func TestToolCallExtraContent_RoundTripNonStreamingAndStreaming(t *testing.T) {
	c := &Client{
		name: "Google Gemini",
		cfg:  providercfg.Config{ModelsShape: providercfg.ShapeGemini},
	}
	wantExtra := `{"google":{"thought_signature":"CvcQAdHtim/sig=="}}`

	// 1. Non-streaming parseResponse captures extra_content.
	var resp apiResponse
	rawResp := `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"},"extra_content":` + wantExtra + `}]}}]}`
	if err := json.Unmarshal([]byte(rawResp), &resp); err != nil {
		t.Fatal(err)
	}
	parsed := c.parseResponse(&resp)
	if len(parsed.ToolCalls) != 1 || string(parsed.ToolCalls[0].ExtraContent) != wantExtra {
		t.Fatalf("non-streaming ExtraContent = %s, want %s", string(parsed.ToolCalls[0].ExtraContent), wantExtra)
	}

	// 2. Streaming parseSSEStream captures extra_content across delta chunks.
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{\"path\":"},"extra_content":` + wantExtra + `}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"b.go\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	var streamed []llm.ToolCall
	err := c.parseSSEStream(strings.NewReader(sse), func(ch llm.StreamChunk) {
		if ch.ToolCall != nil {
			streamed = append(streamed, *ch.ToolCall)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 1 || string(streamed[0].ExtraContent) != wantExtra {
		t.Fatalf("streaming ExtraContent = %s, want %s", string(streamed[0].ExtraContent), wantExtra)
	}

	// 3. buildMessages echoes extra_content back verbatim on the follow-up turn.
	msgs := c.buildMessages(llm.CompletionRequest{
		Model: "gemini-4-neon",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "read b.go"},
			{Role: llm.RoleAssistant, ToolCalls: streamed},
			{Role: llm.RoleTool, ToolCallID: "call_2", Content: "file contents"},
		},
	})
	wire, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"extra_content":`+wantExtra) {
		t.Fatalf("expected extra_content echoed in follow-up request, got: %s", string(wire))
	}
}
