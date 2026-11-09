// Package agentrt runs a bounded, multi-step tool-calling loop over a set of
// caller-supplied tools, against any llm.Provider. It is the runtime behind
// hosted agent endpoints (manage_agent): the model plans, calls the app's own
// tools, reads the results, and loops until it answers or a cap is hit.
//
// It also orchestrates SUB-AGENTS: when the endpoint allows it, a built-in
// spawn_subagent tool lets the model delegate a self-contained subtask to a
// fresh-context nested run that shares the parent's token budget and the run's
// spawn allowance, bounded by a recursion depth and a total-spawn cap.
//
// It is a leaf package: it depends only on the provider layer, so the caller
// decides what each tool actually does (a server function, a data read) without
// dragging the engine or the tool registry in here. That keeps the loop trivially
// testable with a fake provider and avoids the engine->tools import direction that
// rules out hosting this under tools/.
package agentrt

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"openfabrica/internal/stringutil"
	"openfabrica/providers/llm"
)

const (
	// DefaultMaxSteps bounds the tool-calling loop when the caller sets none.
	DefaultMaxSteps = 8
	// MaxStepsCeiling caps MaxSteps whatever an endpoint configures, so a
	// misconfigured endpoint cannot spin the model without bound.
	MaxStepsCeiling = 40
	// MaxDepthCeiling caps how deep sub-agents may nest, whatever an endpoint sets.
	MaxDepthCeiling = 3
	// MaxSubagentsCeiling caps total sub-agent spawns per run, whatever an endpoint sets.
	MaxSubagentsCeiling = 32
	// toolResultCap truncates a single tool result before it returns to the
	// model, so one chatty tool cannot blow the context (and the bill) open.
	toolResultCap = 8 * 1024
	// spawnToolName is the built-in tool the model calls to delegate a subtask.
	spawnToolName = "spawn_subagent"
)

// Stop reasons a run ends with. Reported on Result so the endpoint can tell the
// caller whether the agent actually converged.
const (
	StopDone    = "done"     // the model answered with no further tool call
	StopStepCap = "step_cap" // hit MaxSteps while still calling tools
	StopBudget  = "budget"   // the run token budget was spent
)

// thinkingOff is the shared, addressable false the loop points every call's
// Thinking at, matching the LLM proxy and every operational engine turn: hidden
// chain-of-thought bills against the output budget and never surfaces here.
var thinkingOff = false

// Tool is one capability the agent may call. Def is the wire schema shown to the
// model; Invoke runs the call and returns a string result fed back as the tool
// message. Invoke returns an error only for an execution fault; a business
// failure the model should see and react to belongs in the returned string.
type Tool struct {
	Def    llm.ToolDef
	Invoke func(ctx context.Context, argsJSON string) (string, error)
}

// EventKind labels a step event emitted during a run.
type EventKind string

const (
	EventToolCall   EventKind = "tool_call"
	EventToolResult EventKind = "tool_result"
	EventText       EventKind = "text"
	EventDone       EventKind = "done"
	EventError      EventKind = "error"
)

// Event is one observable step in a run. The endpoint turns these into SSE
// frames and/or a transcript; a nil OnEvent disables emission. Depth is 0 for the
// top-level agent and >0 for a sub-agent at that nesting level.
type Event struct {
	Kind  EventKind
	Step  int
	Depth int
	Tool  string // tool name, for tool_call / tool_result
	Text  string // assistant text, tool args, tool result, or error message
}

// Config parameterises one run. Tools is the allowlisted capability set; an
// empty set is allowed (the model just answers from the prompt). Budget, when
// non-nil, is decremented by every call's token usage and shared across any
// sub-agents so a whole run, not just one call, is bounded.
type Config struct {
	Provider    llm.Provider
	Model       string
	System      string
	MaxTokens   int      // per-call output cap; 0 lets the provider default stand
	Temperature *float64 // nil = provider default
	MaxSteps    int      // tool-calling iterations; clamped to [1, MaxStepsCeiling]
	Tools       []Tool
	Budget      *Budget
	OnEvent     func(Event)
	// MaxSubagents is the total number of sub-agents the whole run may spawn; 0
	// disables sub-agents (no spawn_subagent tool is offered). MaxDepth is how
	// deep they may nest; 0 with MaxSubagents>0 means one level.
	MaxSubagents int
	MaxDepth     int
}

// Result is the outcome of a run. InputTokens/OutputTokens and Subagents are
// aggregates across the top-level agent and every sub-agent it spawned.
type Result struct {
	Text         string // the top-level agent's final answer
	Steps        int    // top-level tool-calling iterations actually run
	InputTokens  int
	OutputTokens int
	Subagents    int    // total sub-agents spawned during the run
	StopReason   string // one of the Stop* constants (top-level)
}

// TotalTokens is what the endpoint charges against the daily budget, matching
// the LLM proxy's input+output accounting.
func (r Result) TotalTokens() int { return r.InputTokens + r.OutputTokens }

// runState is the per-run bookkeeping threaded through the top-level agent and
// every sub-agent: the shared spawn allowance and aggregate token usage across
// all depths. Guarded because sub-agents may, in a later phase, run concurrently.
type runState struct {
	mu        sync.Mutex
	spawns    int
	maxSpawns int
	inTok     int
	outTok    int
}

func (s *runState) trySpawn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spawns >= s.maxSpawns {
		return false
	}
	s.spawns++
	return true
}

func (s *runState) addTokens(in, out int) {
	s.mu.Lock()
	s.inTok += in
	s.outTok += out
	s.mu.Unlock()
}

func (s *runState) snapshot() (spawns, in, out int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawns, s.inTok, s.outTok
}

// Run executes the top-level loop and returns its aggregate result. input is the
// caller-supplied conversation; the last turn must be a user turn (the endpoint
// validates that, mirroring the LLM proxy).
func Run(ctx context.Context, cfg Config, input []llm.Message) (Result, error) {
	state := &runState{maxSpawns: clampSubagents(cfg.MaxSubagents)}
	res, err := run(ctx, cfg, input, 0, state)
	spawns, in, out := state.snapshot()
	res.InputTokens, res.OutputTokens, res.Subagents = in, out, spawns
	return res, err
}

// run is one agent at nesting depth `depth`. The top-level call is depth 0; the
// spawn_subagent tool recurses with depth+1. Token usage and the spawn count go
// to the shared state so Run can report run-wide totals.
func run(ctx context.Context, cfg Config, input []llm.Message, depth int, state *runState) (Result, error) {
	steps := clampSteps(cfg.MaxSteps)

	byName := make(map[string]Tool, len(cfg.Tools)+1)
	defs := make([]llm.ToolDef, 0, len(cfg.Tools)+1)
	for _, t := range cfg.Tools {
		byName[t.Def.Name] = t
		defs = append(defs, t.Def)
	}
	// Offer the sub-agent tool while there is depth and spawn budget left.
	if state.maxSpawns > 0 && depth < clampDepth(cfg.MaxDepth) {
		st := spawnTool(cfg, depth, state)
		byName[st.Def.Name] = st
		defs = append(defs, st.Def)
	}

	msgs := append([]llm.Message(nil), input...)
	var res Result

	for step := 0; step < steps; step++ {
		res.Steps = step + 1

		if cfg.Budget.Remaining() <= 0 {
			res.StopReason = StopBudget
			emit(cfg, Event{Kind: EventError, Step: step, Depth: depth, Text: "run token budget exhausted"})
			return res, nil
		}

		req := llm.CompletionRequest{
			Model:       cfg.Model,
			System:      cfg.System,
			Messages:    msgs,
			Tools:       defs,
			MaxTokens:   cfg.MaxTokens,
			Temperature: cfg.Temperature,
			Thinking:    &thinkingOff,
		}
		resp, err := cfg.Provider.Complete(ctx, req)
		if err != nil {
			return res, fmt.Errorf("agent step %d: %w", step, err)
		}
		cfg.Budget.add(resp.Usage.InputTokens + resp.Usage.OutputTokens)
		state.addTokens(resp.Usage.InputTokens, resp.Usage.OutputTokens)

		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})

		if len(resp.ToolCalls) == 0 {
			res.Text = resp.Content
			res.StopReason = StopDone
			if resp.Content != "" {
				emit(cfg, Event{Kind: EventText, Step: step, Depth: depth, Text: resp.Content})
			}
			emit(cfg, Event{Kind: EventDone, Step: step, Depth: depth})
			return res, nil
		}

		for _, call := range resp.ToolCalls {
			emit(cfg, Event{Kind: EventToolCall, Step: step, Depth: depth, Tool: call.Name, Text: call.Arguments})
			out := truncate(dispatch(ctx, byName, call), toolResultCap)
			emit(cfg, Event{Kind: EventToolResult, Step: step, Depth: depth, Tool: call.Name, Text: out})
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Content: out})
		}
	}

	res.StopReason = StopStepCap
	emit(cfg, Event{Kind: EventError, Step: res.Steps, Depth: depth, Text: "reached max steps without a final answer"})
	return res, nil
}

// spawnTool is the built-in delegation tool. It runs a nested agent on the given
// task with a subset of this agent's tools, sharing the run's budget and spawn
// allowance, and hands the sub-agent's answer back as the tool result.
func spawnTool(cfg Config, depth int, state *runState) Tool {
	return Tool{
		Def: llm.ToolDef{
			Name: spawnToolName,
			Description: "Delegate a self-contained subtask to a fresh sub-agent with its own context. " +
				"It runs your tools (or the subset you name), then returns its final answer. Use it for a " +
				"chunk of work you can state in one task string - a focused lookup, a piece of research, drafting " +
				"one section - so your own context stays clean. The sub-agent cannot ask you questions, so put " +
				"everything it needs in `task`.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task": map[string]any{
						"type":        "string",
						"description": "The COMPLETE instruction for the sub-agent. It sees only this, not your conversation.",
					},
					"tools": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Optional names of the tools the sub-agent may use (a subset of yours). Omit to give it the same tools you have.",
					},
				},
				"required": []any{"task"},
			},
		},
		Invoke: func(ctx context.Context, argsJSON string) (string, error) {
			if !state.trySpawn() {
				return "error: this run's sub-agent limit is reached; do the remaining work yourself", nil
			}
			var a struct {
				Task  string   `json:"task"`
				Tools []string `json:"tools"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "error: invalid tool arguments", nil
			}
			if strings.TrimSpace(a.Task) == "" {
				return "error: task is required", nil
			}
			sub := cfg
			sub.Tools = filterTools(cfg.Tools, a.Tools)
			sub.System = subAgentSystem(cfg.System)
			subRes, err := run(ctx, sub, []llm.Message{{Role: llm.RoleUser, Content: a.Task}}, depth+1, state)
			if err != nil {
				return "error: the sub-agent failed", nil
			}
			switch subRes.StopReason {
			case StopStepCap:
				return "The sub-agent hit its step limit before finishing. Partial result:\n" + subRes.Text, nil
			case StopBudget:
				return "The run's token budget ran out during the sub-agent. Partial result:\n" + subRes.Text, nil
			default:
				return subRes.Text, nil
			}
		},
	}
}

// dispatch runs one tool call, turning an unknown tool or an Invoke fault into a
// string the model can read and recover from. One bad call does not fail the
// run, exactly as the engine's dispatcher hands tool errors back in-band.
func dispatch(ctx context.Context, byName map[string]Tool, call llm.ToolCall) string {
	t, ok := byName[call.Name]
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", call.Name)
	}
	out, err := t.Invoke(ctx, call.Arguments)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

// filterTools returns the subset of tools whose names are listed, or all of them
// when the list is empty.
func filterTools(tools []Tool, names []string) []Tool {
	if len(names) == 0 {
		return tools
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	out := make([]Tool, 0, len(names))
	for _, t := range tools {
		if want[t.Def.Name] {
			out = append(out, t)
		}
	}
	return out
}

// subAgentSystem frames a sub-agent's system prompt: a delegation preamble plus
// the parent's own system as app context, so the sub-agent knows the platform it
// operates in without inheriting the parent's conversation.
func subAgentSystem(parent string) string {
	const preamble = "You are a sub-agent handling ONE delegated task. Use your tools to complete it, " +
		"then reply with a concise, complete answer the delegating agent can use directly. You cannot ask " +
		"questions - the delegator is not present - so work with what the task gives you."
	if strings.TrimSpace(parent) == "" {
		return preamble
	}
	return preamble + "\n\nContext about the app you operate in:\n" + parent
}

func clampSteps(n int) int {
	switch {
	case n <= 0:
		return DefaultMaxSteps
	case n > MaxStepsCeiling:
		return MaxStepsCeiling
	default:
		return n
	}
}

func clampDepth(n int) int {
	switch {
	case n <= 0:
		return 1
	case n > MaxDepthCeiling:
		return MaxDepthCeiling
	default:
		return n
	}
}

func clampSubagents(n int) int {
	switch {
	case n < 0:
		return 0
	case n > MaxSubagentsCeiling:
		return MaxSubagentsCeiling
	default:
		return n
	}
}

// truncate clips s to at most max runes (rune-safe, so the model never
// receives a split rune) and marks the cut so it knows the result is partial.
func truncate(s string, max int) string {
	clipped := stringutil.Truncate(s, max)
	if clipped != s {
		clipped += "[truncated]"
	}
	return clipped
}

func emit(cfg Config, e Event) {
	if cfg.OnEvent != nil {
		cfg.OnEvent(e)
	}
}
