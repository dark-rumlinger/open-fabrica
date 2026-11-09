package openai

import (
	"testing"

	"openfabrica/providers/llm"
)

// parallel_tool_calls is requested whenever tools are attached - several
// OpenAI-compatible gateways default it OFF, which forces an agent into
// one-tool-call-per-turn micro-iterations - and is omitted entirely on a
// tool-less request (some strict endpoints 400 on the field without tools).
func TestBuildRequestBody_ParallelToolCalls(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	c := &Client{name: "parallel-test"}
	withTools := llm.CompletionRequest{
		Model:    "m",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Tools: []llm.ToolDef{{
			Name: "manage_files", Description: "files",
			Parameters: map[string]any{"type": "object"},
		}},
	}
	body := c.buildRequestBody(withTools, false)
	if v, ok := body["parallel_tool_calls"].(bool); !ok || !v {
		t.Fatalf("parallel_tool_calls = %v, want true when tools attached", body["parallel_tool_calls"])
	}

	noTools := llm.CompletionRequest{Model: "m", Messages: withTools.Messages}
	if _, ok := c.buildRequestBody(noTools, false)["parallel_tool_calls"]; ok {
		t.Fatal("parallel_tool_calls must be omitted without tools")
	}

	// Demoted after an endpoint rejection: the field never reappears.
	llm.Caps.Learn(c.CapKey("m"), llm.CapParallelToolCalls, llm.No)
	if _, ok := c.buildRequestBody(withTools, false)["parallel_tool_calls"]; ok {
		t.Fatal("parallel_tool_calls must be omitted once the endpoint has rejected it")
	}
}

// The matcher is gated on a typed CodeBadRequest. An untyped error or a 500
// whose text merely echoes the request must never latch the field off.
func TestMatchesParallelToolCalls(t *testing.T) {
	rejection := &llm.Error{
		Code:    llm.CodeBadRequest,
		Status:  400,
		Message: `openai-chat: API error 400: {"error":{"message":"Unknown parameter: parallel_tool_calls"}}`,
	}
	if !matchesParallelToolCalls(rejection) {
		t.Fatal("rejection naming the field not detected")
	}
	if matchesParallelToolCalls(&llm.Error{Code: llm.CodeRateLimit, Message: "rate limited"}) {
		t.Error("unrelated error misdetected as a parallel_tool_calls rejection")
	}
	if matchesParallelToolCalls(&llm.Error{Code: llm.CodeServer, Message: "parallel_tool_calls"}) {
		t.Error("a 500 echoing the field name was treated as a rejection")
	}
}
