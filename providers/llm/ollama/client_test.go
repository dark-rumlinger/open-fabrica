package ollama

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

// A thinking-controlled call that ALSO carries tools must route to the native
// /api/chat path (so the reasoning toggle is honoured), forward the tools and the
// tool-call history, send think=false, and parse tool calls back out of the
// reply. Falling back to the compat path drops the toggle, leaving a reasoning
// model to think on every turn.
func TestComplete_ThinkingWithToolsUsesNativeAndForwardsTools(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, `{"model":"qwen3.5:9b","message":{"role":"assistant","content":"",`+
			`"tool_calls":[{"function":{"name":"manage_files","arguments":{"action":"save","file_path":"/"}}}]},`+
			`"done":true,"done_reason":"stop","prompt_eval_count":100,"eval_count":20}`)
	}))
	defer srv.Close()

	c := New(llm.Options{Name: "ollama", BaseURL: srv.URL + "/v1", HTTPClient: srv.Client()})
	thinkOff := false
	resp, err := c.Complete(context.Background(), llm.CompletionRequest{
		Model:    "qwen3.5:9b",
		System:   "sys",
		Thinking: &thinkOff,
		Tools: []llm.ToolDef{{
			Name:        "manage_files",
			Description: "files",
			Parameters:  map[string]any{"type": "object"},
		}},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "build it"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{Name: "manage_files", Arguments: `{"action":"get","file_path":"/"}`},
			}},
			{Role: llm.RoleTool, Content: `{"ok":true}`},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Routed to the native endpoint, not the compat one.
	if gotPath != "/api/chat" {
		t.Fatalf("expected native /api/chat, hit %q (compat path drops the reasoning toggle)", gotPath)
	}
	// Reasoning explicitly disabled.
	if v, ok := gotBody["think"].(bool); !ok || v != false {
		t.Errorf("think must be sent as false, got %v", gotBody["think"])
	}
	// Tools forwarded.
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool forwarded, got %v", gotBody["tools"])
	}
	// History preserved: assistant tool_calls (arguments as an OBJECT) and a tool result.
	msgs, _ := gotBody["messages"].([]any)
	var sawAssistantToolCall, sawToolResult bool
	for _, mi := range msgs {
		m, _ := mi.(map[string]any)
		if m["role"] == "assistant" {
			if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
				tc := tcs[0].(map[string]any)["function"].(map[string]any)
				if _, isObj := tc["arguments"].(map[string]any); isObj {
					sawAssistantToolCall = true
				}
			}
		}
		if m["role"] == "tool" {
			sawToolResult = true
		}
	}
	if !sawAssistantToolCall {
		t.Error("assistant tool_calls not forwarded with object-form arguments")
	}
	if !sawToolResult {
		t.Error("tool-result message not forwarded")
	}

	// Reply tool call parsed back into a JSON-string-argument ToolCall.
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "manage_files" {
		t.Fatalf("expected one parsed tool call, got %+v", resp.ToolCalls)
	}
	if !strings.Contains(resp.ToolCalls[0].Arguments, `"file_path":"/"`) {
		t.Errorf("tool-call arguments not re-encoded to a JSON string: %q", resp.ToolCalls[0].Arguments)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("a tool-call reply must report stop_reason=tool_use, got %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 20 {
		t.Errorf("usage not mapped: %+v", resp.Usage)
	}
}

// A plain tool call with no reasoning toggle and no schema stays on the compat
// path (unchanged behaviour) - only schema or thinking control routes to native.
func TestComplete_NoThinkingNoSchemaStaysCompat(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c := New(llm.Options{Name: "ollama", BaseURL: srv.URL + "/v1", HTTPClient: srv.Client()})
	_, err := c.Complete(context.Background(), llm.CompletionRequest{
		Model:    "qwen3.5:9b",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("a no-thinking no-schema call must stay on the compat path, hit %q", gotPath)
	}
}

// keepAliveValue must emit an integer keep_alive (-1/0/seconds) as a JSON NUMBER,
// not a string. Ollama parses a string keep_alive with time.ParseDuration, which
// rejects a unit-less "-1", so the documented "resident indefinitely" sentinel
// (README) silently failed when sent as a string. A real duration like "30m" stays
// a string.
func TestKeepAliveValue(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"-1", -1},
		{"0", 0},
		{"300", 300},
		{"30m", "30m"},
		{"1h", "1h"},
	}
	for _, tc := range cases {
		got := keepAliveValue(tc.in)
		if got != tc.want {
			t.Errorf("keepAliveValue(%q) = %#v (%T), want %#v (%T)", tc.in, got, got, tc.want, tc.want)
		}
	}
}

// The keep_alive=-1 sentinel must reach the wire as the JSON number -1, so the
// daemon keeps the model resident indefinitely instead of rejecting a unit-less
// duration string and falling back to its default.
func TestComplete_KeepAliveMinusOneSentAsNumber(t *testing.T) {
	t.Setenv("OPENFABRICA_OLLAMA_KEEP_ALIVE", "-1")
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, `{"model":"qwen3.5:9b","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop"}`)
	}))
	defer srv.Close()

	c := New(llm.Options{Name: "ollama", BaseURL: srv.URL + "/v1", HTTPClient: srv.Client()})
	thinkOff := false
	if _, err := c.Complete(context.Background(), llm.CompletionRequest{
		Model:    "qwen3.5:9b",
		Thinking: &thinkOff,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// JSON unmarshals numbers into float64; a string would decode as string.
	v, ok := gotBody["keep_alive"].(float64)
	if !ok {
		t.Fatalf("keep_alive must be a JSON number, got %#v (%T)", gotBody["keep_alive"], gotBody["keep_alive"])
	}
	if v != -1 {
		t.Errorf("keep_alive = %v, want -1", v)
	}
}

// A configured num_ctx (OPENFABRICA_OLLAMA_NUM_CTX) must force the native path even
// for a plain call, so the context-window size is never silently dropped on the
// compat path (which would evict the daemon's cached prefix).
func TestComplete_NumCtxForcesNative(t *testing.T) {
	t.Setenv("OPENFABRICA_OLLAMA_NUM_CTX", "16384")
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, `{"model":"qwen3.5:9b","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop"}`)
	}))
	defer srv.Close()

	c := New(llm.Options{Name: "ollama", BaseURL: srv.URL + "/v1", HTTPClient: srv.Client()})
	if _, err := c.Complete(context.Background(), llm.CompletionRequest{
		Model:    "qwen3.5:9b",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/api/chat" {
		t.Fatalf("a configured num_ctx must force the native path, hit %q (compat drops num_ctx)", gotPath)
	}
	opts, _ := gotBody["options"].(map[string]any)
	if v, _ := opts["num_ctx"].(float64); v != 16384 {
		t.Errorf("num_ctx not forwarded to native options: %#v", gotBody["options"])
	}
}
