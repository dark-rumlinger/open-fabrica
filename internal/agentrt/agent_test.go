package agentrt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"openfabrica/providers/llm"
)

// fakeProvider returns queued responses in order, one per Complete call, and
// records every request it saw so a test can assert what the loop fed back.
type fakeProvider struct {
	responses []*llm.CompletionResponse
	errs      []error
	calls     int
	requests  []llm.CompletionRequest
}

func (f *fakeProvider) Name() string                       { return "fake" }
func (f *fakeProvider) Type() string                       { return "fake" }
func (f *fakeProvider) Ping(context.Context, string) error { return nil }
func (f *fakeProvider) Stream(context.Context, llm.CompletionRequest, llm.StreamCallback) error {
	return errors.New("stream not used in these tests")
}

func (f *fakeProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	i := f.calls
	f.calls++
	f.requests = append(f.requests, req)
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i >= len(f.responses) {
		// Default terminal response so a scripting mistake ends the loop instead
		// of running to the step cap.
		return &llm.CompletionResponse{Content: "(no more scripted responses)"}, nil
	}
	return f.responses[i], nil
}

func textResp(s string, in, out int) *llm.CompletionResponse {
	return &llm.CompletionResponse{Content: s, Usage: llm.Usage{InputTokens: in, OutputTokens: out}}
}

func toolResp(id, name, args string, in, out int) *llm.CompletionResponse {
	return &llm.CompletionResponse{
		ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: args}},
		Usage:     llm.Usage{InputTokens: in, OutputTokens: out},
	}
}

func userMsg(s string) []llm.Message { return []llm.Message{{Role: llm.RoleUser, Content: s}} }

func echoTool(record *string) Tool {
	return Tool{
		Def: llm.ToolDef{Name: "echo", Description: "echo the args", Parameters: map[string]any{"type": "object"}},
		Invoke: func(_ context.Context, args string) (string, error) {
			if record != nil {
				*record = args
			}
			return "echoed:" + args, nil
		},
	}
}

func TestRunAnswersWithoutTools(t *testing.T) {
	prov := &fakeProvider{responses: []*llm.CompletionResponse{textResp("hello", 10, 5)}}
	res, err := Run(context.Background(), Config{Provider: prov, Model: "m"}, userMsg("hi"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopDone {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopDone)
	}
	if res.Text != "hello" {
		t.Errorf("Text = %q, want %q", res.Text, "hello")
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1", res.Steps)
	}
	if res.TotalTokens() != 15 {
		t.Errorf("TotalTokens = %d, want 15", res.TotalTokens())
	}
	if prov.calls != 1 {
		t.Errorf("provider calls = %d, want 1", prov.calls)
	}
}

func TestRunCallsToolThenAnswers(t *testing.T) {
	var gotArgs string
	prov := &fakeProvider{responses: []*llm.CompletionResponse{
		toolResp("c1", "echo", `{"x":1}`, 10, 5),
		textResp("done", 8, 3),
	}}
	res, err := Run(context.Background(), Config{Provider: prov, Model: "m", Tools: []Tool{echoTool(&gotArgs)}}, userMsg("go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopDone || res.Text != "done" || res.Steps != 2 {
		t.Errorf("got stop=%q text=%q steps=%d, want done/\"done\"/2", res.StopReason, res.Text, res.Steps)
	}
	if gotArgs != `{"x":1}` {
		t.Errorf("tool got args %q, want %q", gotArgs, `{"x":1}`)
	}
	if res.TotalTokens() != 26 {
		t.Errorf("TotalTokens = %d, want 26", res.TotalTokens())
	}
	// The first request must expose the tool schema.
	if got := prov.requests[0].Tools; len(got) != 1 || got[0].Name != "echo" {
		t.Errorf("first request tools = %+v, want one tool named echo", got)
	}
	// The second request must carry the assistant tool-call turn followed by the
	// tool result, in that order, so the provider protocol stays valid.
	msgs := prov.requests[1].Messages
	if len(msgs) != 3 {
		t.Fatalf("second request had %d messages, want 3 (user, assistant, tool)", len(msgs))
	}
	if msgs[1].Role != llm.RoleAssistant || len(msgs[1].ToolCalls) != 1 {
		t.Errorf("second msg = %+v, want assistant with one tool call", msgs[1])
	}
	if msgs[2].Role != llm.RoleTool || msgs[2].ToolCallID != "c1" || msgs[2].Content != `echoed:{"x":1}` {
		t.Errorf("third msg = %+v, want tool result for c1", msgs[2])
	}
}

func TestRunStepCap(t *testing.T) {
	var resps []*llm.CompletionResponse
	for i := 0; i < 5; i++ {
		resps = append(resps, toolResp("c", "spin", `{}`, 1, 1))
	}
	prov := &fakeProvider{responses: resps}
	invoked := 0
	spin := Tool{
		Def:    llm.ToolDef{Name: "spin", Parameters: map[string]any{"type": "object"}},
		Invoke: func(context.Context, string) (string, error) { invoked++; return "again", nil },
	}
	res, err := Run(context.Background(), Config{Provider: prov, Model: "m", MaxSteps: 3, Tools: []Tool{spin}}, userMsg("go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopStepCap {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopStepCap)
	}
	if res.Steps != 3 || prov.calls != 3 || invoked != 3 {
		t.Errorf("steps=%d calls=%d invoked=%d, want 3/3/3", res.Steps, prov.calls, invoked)
	}
}

func TestRunUnknownToolRecovers(t *testing.T) {
	prov := &fakeProvider{responses: []*llm.CompletionResponse{
		toolResp("c1", "nope", `{}`, 1, 1),
		textResp("recovered", 1, 1),
	}}
	res, err := Run(context.Background(), Config{Provider: prov, Model: "m"}, userMsg("go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopDone || res.Text != "recovered" {
		t.Errorf("got stop=%q text=%q, want done/\"recovered\"", res.StopReason, res.Text)
	}
	last := prov.requests[1].Messages[len(prov.requests[1].Messages)-1]
	if last.Role != llm.RoleTool || !strings.Contains(last.Content, `unknown tool "nope"`) {
		t.Errorf("tool result = %+v, want an unknown-tool error fed back", last)
	}
}

func TestRunBudgetStops(t *testing.T) {
	prov := &fakeProvider{responses: []*llm.CompletionResponse{
		toolResp("c1", "echo", `{}`, 60, 60), // 120 tokens, over the 100 budget
		textResp("should-not-reach", 1, 1),
	}}
	res, err := Run(context.Background(), Config{
		Provider: prov, Model: "m", Tools: []Tool{echoTool(nil)}, Budget: NewBudget(100),
	}, userMsg("go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopBudget {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopBudget)
	}
	if prov.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (second call must not happen)", prov.calls)
	}
	if res.Text != "" {
		t.Errorf("Text = %q, want empty (never reached the final answer)", res.Text)
	}
}

func TestRunEmitsEventsInOrder(t *testing.T) {
	prov := &fakeProvider{responses: []*llm.CompletionResponse{
		toolResp("c1", "echo", `{}`, 1, 1),
		textResp("done", 1, 1),
	}}
	var kinds []EventKind
	_, err := Run(context.Background(), Config{
		Provider: prov, Model: "m", Tools: []Tool{echoTool(nil)},
		OnEvent: func(e Event) { kinds = append(kinds, e.Kind) },
	}, userMsg("go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []EventKind{EventToolCall, EventToolResult, EventText, EventDone}
	if len(kinds) != len(want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("event[%d] = %q, want %q", i, kinds[i], want[i])
		}
	}
}

func TestRunProviderErrorPropagates(t *testing.T) {
	prov := &fakeProvider{errs: []error{errors.New("boom")}}
	_, err := Run(context.Background(), Config{Provider: prov, Model: "m"}, userMsg("go"))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want one wrapping \"boom\"", err)
	}
}

func TestClampSteps(t *testing.T) {
	cases := map[int]int{0: DefaultMaxSteps, -3: DefaultMaxSteps, 5: 5, MaxStepsCeiling + 10: MaxStepsCeiling}
	for in, want := range cases {
		if got := clampSteps(in); got != want {
			t.Errorf("clampSteps(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestBudget(t *testing.T) {
	var nilB *Budget
	if nilB.Remaining() != 1<<31-1 || nilB.Used() != 0 {
		t.Errorf("nil budget = (%d,%d), want (MaxInt32, 0)", nilB.Remaining(), nilB.Used())
	}
	b := NewBudget(100)
	b.add(30)
	if b.Remaining() != 70 || b.Used() != 30 {
		t.Errorf("after add(30): remaining=%d used=%d, want 70/30", b.Remaining(), b.Used())
	}
	b.add(80)
	if b.Remaining() != 0 {
		t.Errorf("after over-spend: remaining=%d, want 0", b.Remaining())
	}
	if NewBudget(0).Remaining() != 1<<31-1 {
		t.Errorf("zero-limit budget should be unlimited")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 100) != "short" {
		t.Errorf("short string should pass through unchanged")
	}
	long := strings.Repeat("a", 50)
	got := truncate(long, 10)
	if !strings.HasPrefix(got, strings.Repeat("a", 10)) || !strings.Contains(got, "truncated") {
		t.Errorf("truncate did not clip and mark: %q", got)
	}
}

func hasToolDef(defs []llm.ToolDef, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

func TestRunNoSpawnToolWhenDisabled(t *testing.T) {
	prov := &fakeProvider{responses: []*llm.CompletionResponse{textResp("hi", 1, 1)}}
	if _, err := Run(context.Background(), Config{Provider: prov, Model: "m"}, userMsg("go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hasToolDef(prov.requests[0].Tools, "spawn_subagent") {
		t.Error("spawn_subagent offered when MaxSubagents=0")
	}
}

func TestRunSpawnsSubagent(t *testing.T) {
	prov := &fakeProvider{responses: []*llm.CompletionResponse{
		toolResp("s1", "spawn_subagent", `{"task":"do the subtask"}`, 5, 5), // top delegates
		textResp("sub done", 3, 3), // sub-agent answers
		textResp("top done", 2, 2), // top finishes
	}}
	res, err := Run(context.Background(), Config{Provider: prov, Model: "m", MaxSubagents: 2, MaxDepth: 1}, userMsg("go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopDone || res.Text != "top done" {
		t.Errorf("got stop=%q text=%q, want done/\"top done\"", res.StopReason, res.Text)
	}
	if res.Subagents != 1 {
		t.Errorf("Subagents = %d, want 1", res.Subagents)
	}
	if res.TotalTokens() != 20 {
		t.Errorf("TotalTokens = %d, want 20 (parent+sub aggregated)", res.TotalTokens())
	}
	if !hasToolDef(prov.requests[0].Tools, "spawn_subagent") {
		t.Errorf("top request tools = %+v, want spawn_subagent offered", prov.requests[0].Tools)
	}
	// The sub-agent call is seeded with the delegation framing and the task only.
	subReq := prov.requests[1]
	if !strings.Contains(subReq.System, "sub-agent") {
		t.Errorf("sub-agent system = %q, want the delegation preamble", subReq.System)
	}
	if last := subReq.Messages[len(subReq.Messages)-1]; last.Content != "do the subtask" {
		t.Errorf("sub-agent seed = %q, want the task", last.Content)
	}
	// The top agent's next turn carries the sub-agent's answer as the tool result.
	topReq := prov.requests[2]
	if last := topReq.Messages[len(topReq.Messages)-1]; last.Role != llm.RoleTool || !strings.Contains(last.Content, "sub done") {
		t.Errorf("top tool result = %+v, want the sub-agent answer fed back", last)
	}
}

func TestRunSubagentLimit(t *testing.T) {
	prov := &fakeProvider{responses: []*llm.CompletionResponse{
		toolResp("s1", "spawn_subagent", `{"task":"one"}`, 1, 1), // spawn #1 -> runs
		textResp("sub1 done", 1, 1),                              // sub #1 answers
		toolResp("s2", "spawn_subagent", `{"task":"two"}`, 1, 1), // spawn #2 -> refused
		textResp("top done", 1, 1),                               // top finishes
	}}
	res, err := Run(context.Background(), Config{Provider: prov, Model: "m", MaxSubagents: 1, MaxDepth: 1}, userMsg("go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Subagents != 1 {
		t.Errorf("Subagents = %d, want 1 (second spawn refused)", res.Subagents)
	}
	if res.Text != "top done" {
		t.Errorf("text = %q, want \"top done\"", res.Text)
	}
	final := prov.requests[3]
	found := false
	for _, m := range final.Messages {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, "limit is reached") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a 'limit is reached' tool result in the final request; msgs=%+v", final.Messages)
	}
}

func TestSubagentCannotSpawnBeyondDepth(t *testing.T) {
	prov := &fakeProvider{responses: []*llm.CompletionResponse{
		toolResp("s1", "spawn_subagent", `{"task":"sub"}`, 1, 1), // top (depth 0) spawns
		textResp("sub answer", 1, 1),                             // sub (depth 1) answers
		textResp("top done", 1, 1),
	}}
	if _, err := Run(context.Background(), Config{Provider: prov, Model: "m", MaxSubagents: 5, MaxDepth: 1}, userMsg("go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// With MaxDepth 1, the depth-1 sub-agent must NOT be offered spawn_subagent.
	if hasToolDef(prov.requests[1].Tools, "spawn_subagent") {
		t.Error("sub-agent at max depth was offered spawn_subagent")
	}
}
