package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// Test-only tools, registered into testToolRegistry (a clone of the default).

type testEchoAction struct {
	Note string `arg:"note,required" desc:"Text to echo back."`
}

func (a *testEchoAction) ActionName() string { return "echo" }
func (a *testEchoAction) Doc() string        { return "Echo a note back." }
func (a *testEchoAction) Example() any {
	return &testEchoAction{Note: "hello"}
}
func (a *testEchoAction) Run(*tools.ToolContext) (*tools.Result, error) {
	return &tools.Result{Success: true, Data: map[string]any{"note": a.Note}}, nil
}

type testAskAction struct {
	Question string `arg:"question,required" desc:"Question for the owner."`
}

func (a *testAskAction) ActionName() string { return "ask" }
func (a *testAskAction) Doc() string        { return "Ask the owner a blocking question." }
func (a *testAskAction) Example() any {
	return &testAskAction{Question: "which color?"}
}
func (a *testAskAction) Run(*tools.ToolContext) (*tools.Result, error) {
	return &tools.Result{Success: true}, nil
}

// testApproveAction returns a failed result tagged with the approval code, the
// way tools.RequestApproval does for destructive actions. Unlike RequestsPause
// (which fires only on a successful call), the session must recognize this on a
// failed result and still end the segment paused.
type testApproveAction struct{}

func (a *testApproveAction) ActionName() string { return "needs_approval" }
func (a *testApproveAction) Doc() string        { return "Return an approval-required result." }
func (a *testApproveAction) Example() any       { return &testApproveAction{} }
func (a *testApproveAction) Run(*tools.ToolContext) (*tools.Result, error) {
	return tools.FailCode(tools.CodeApprovalRequired, "APPROVAL_REQUIRED: test action"), nil
}

// testToolRegistry is the default catalogue plus the engine's test_probe_tool
// fixture. The fixture lives in a clone so it never leaks into the shared
// default registry, which keeps DefaultRegistry().Validate() assertable.
var testToolRegistry = sync.OnceValue(func() *tools.Registry {
	reg := tools.DefaultRegistry().Clone()
	if err := reg.Define(tools.Tool{
		Name:     "test_probe_tool",
		Category: "test",
		OneLiner: "Engine test tool: echo, blocking ask, approval.",
		Actions:  []tools.Action{&testEchoAction{}, &testAskAction{}, &testApproveAction{}},
		RequestsPause: func(action string, _ map[string]any) bool {
			return action == "ask"
		},
	}); err != nil {
		panic(err)
	}
	return reg
})

// testSession wires a session against temp DBs and a scripted provider.
func testSession(t *testing.T, p *enginetest.Provider) *session {
	t.Helper()
	dir := t.TempDir()
	gdb, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { gdb.Close() })
	mgr := db.NewProjectDBManager(dir)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	deps := &Deps{
		DB:               gdb,
		ProjectDBManager: mgr,
		ToolRegistry:     testToolRegistry(),
		ToolExecutor:     tools.NewExecutor(testToolRegistry()),
	}
	s := newSession(deps, pdb, 1, "run_test", newLLMLimiter(2), nil, nil, sessionEvents{})
	s.resolver = staticProviderSource{rp: resolvedProvider{Provider: p, Model: "fake-model", ProviderID: 1}}
	return s
}

func buildSpecForTest() SessionSpec {
	spec := DefaultSpec(StageBuild)
	spec.System = "You are a test builder."
	spec.Seed = []llm.Message{{Role: llm.RoleUser, Content: "do the task"}}
	spec.PreloadTools = []string{"test_probe_tool"}
	spec.Guard = &RepeatGuard{}
	spec.Snapshot = SnapshotNone // no of_files content in these tests
	spec.Timeout = 5 * time.Second
	spec.MaxIterations = 10
	return spec
}

func echoCall(id, note string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: "test_probe_tool",
		Arguments: fmt.Sprintf(`{"action":"echo","note":%q}`, note)}
}

// TestSessionScriptedToolLoop: tool calls execute, results land in the
// transcript, and a tool-call-free turn ends the session.
func TestSessionScriptedToolLoop(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(echoCall("c1", "first")),
		enginetest.Calls(echoCall("c2", "second")),
		enginetest.Text("all done"),
	}}
	s := testSession(t, p)

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Content != "all done" || res.Ending != EndDone {
		t.Fatalf("content=%q ending=%q", res.Content, res.Ending)
	}
	if res.ToolCalls != 2 || res.Iterations != 3 {
		t.Fatalf("toolCalls=%d iterations=%d", res.ToolCalls, res.Iterations)
	}
	// Transcript shape: seed, assistant+tool x2, then nothing for the final
	// text turn (no tool calls = not appended before return).
	var toolResults int
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool {
			toolResults++
		}
	}
	if toolResults != 2 {
		t.Fatalf("tool result messages = %d, want 2", toolResults)
	}
}

// TestSessionEmptyStreakIsConsecutive: maxEmptyResponseStreak counts CONSECUTIVE
// unproductive turns, so a turn that lands tool calls clears the count. Without
// the reset, two empty turns on opposite ends of a long productive build wedge
// the segment - the streak cap fires on turns that were never consecutive.
func TestSessionEmptyStreakIsConsecutive(t *testing.T) {
	if maxEmptyResponseStreak != 2 {
		t.Fatalf("test assumes maxEmptyResponseStreak==2, got %d", maxEmptyResponseStreak)
	}
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Empty(), // streak 1
		enginetest.Calls(echoCall("c1", "real work")), // productive: clears it
		enginetest.Empty(),                            // streak 1 again, NOT 2
		enginetest.Text("finished"),
	}}
	s := testSession(t, p)

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Ending != EndDone {
		t.Fatalf("end=%q, want %q - the streak cap fired on non-consecutive empties",
			res.Ending, EndDone)
	}
	if res.Content != "finished" {
		t.Fatalf("content=%q, want %q - segment ended before the final turn", res.Content, "finished")
	}
	if res.Iterations != 4 {
		t.Fatalf("iterations=%d, want 4 - segment ended early", res.Iterations)
	}
}

// TestSessionSilentTurnsWedgeRatherThanFinish: a response with no text, no tool call
// and no output tokens is a provider anomaly, not a deliberate stop - a model that is
// finished says so. A silent turn must count toward the empty streak; otherwise it
// falls through to "the model stopped calling tools of its own accord" and BUILD
// reports the checklist finished with nothing built on that turn.
func TestSessionSilentTurnsWedgeRatherThanFinish(t *testing.T) {
	if maxEmptyResponseStreak != 2 {
		t.Fatalf("test assumes maxEmptyResponseStreak==2, got %d", maxEmptyResponseStreak)
	}
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(echoCall("c1", "real work")),
		enginetest.Silent(), // streak 1 - must NOT read as "done"
		enginetest.Silent(), // streak 2 - cap
		enginetest.Text("never reached"),
	}}
	s := testSession(t, p)

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Ending != endWedged.outcome {
		t.Fatalf("end=%q detail=%q, want the wedged outcome %q - a silent turn was read as a clean finish",
			res.Ending, res.EndDetail, endWedged.outcome)
	}
	if res.Iterations != 3 {
		t.Fatalf("iterations=%d, want 3 - the segment did not stop at the streak cap", res.Iterations)
	}
}

// TestSessionEmptyStreakSurvivesProductiveTruncation: a max_tokens turn that produced
// text is productive, so it must clear the streak. If the reset only ran on the
// fall-through path, which a max_tokens turn returns before reaching, a single earlier
// empty turn would stay on the books across a long run of useful truncated turns and
// the NEXT empty one would hit the cap.
func TestSessionEmptyStreakSurvivesProductiveTruncation(t *testing.T) {
	if maxEmptyResponseStreak != 2 {
		t.Fatalf("test assumes maxEmptyResponseStreak==2, got %d", maxEmptyResponseStreak)
	}
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Empty(),                  // streak 1
		enginetest.Truncated("half a body"), // produced text: clears it
		enginetest.Empty(),                  // streak 1 again, NOT 2
		enginetest.Text("finished"),
	}}
	s := testSession(t, p)

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Ending != EndDone {
		t.Fatalf("end=%q, want %q - the streak survived a turn that produced text",
			res.Ending, EndDone)
	}
}

// TestContextTokensAfterCacheHit pins the ratchet a replay-cache hit runs instead of the
// provider's usage figures, which a hit never reports. Rollover and compaction both key on
// this number, so a frozen one silently disables both for the rest of the segment.
func TestContextTokensAfterCacheHit(t *testing.T) {
	cases := []struct {
		name              string
		prev, bytes, want int
	}{
		{"grown transcript raises the estimate", 1000, 40_000, 40_000 / bytesPerTokenEstimate},
		{"never lowers a real call's figure", 50_000, 40_000, 50_000},
		{"equal estimate is not a change", 10_000, 40_000, 10_000},
		{"first hit of a segment arms from zero", 0, 8_000, 2_000},
	}
	for _, tc := range cases {
		if got := contextTokensAfterCacheHit(tc.prev, tc.bytes); got != tc.want {
			t.Errorf("%s: contextTokensAfterCacheHit(%d, %d) = %d, want %d",
				tc.name, tc.prev, tc.bytes, got, tc.want)
		}
	}
}

// TestSessionContinuationDoesNotLeakIntoLaterContent: the content join exists so
// a plan JSON cut across max_tokens reassembles. It must not outlive the cut that
// caused it - once real tool calls land, a later closing summary REPLACES the
// stale fragment instead of being appended to it. Without the reset, one cut
// anywhere in a segment permanently flips res.Content into append mode and the
// owner is shown mid-turn narration glued to the summary.
func TestSessionContinuationDoesNotLeakIntoLaterContent(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Truncated("Here is the first ha"), // cut -> continuation armed
		enginetest.CallsWithText("Working on it. ", echoCall("c1", "real work")),
		enginetest.Text("All finished."),
	}}
	s := testSession(t, p)

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Content != "All finished." {
		t.Fatalf("content=%q, want %q - stale pre-continuation text leaked into the final summary",
			res.Content, "All finished.")
	}
}

// TestSessionPlanContinuationStillJoins: the guard above must not break the case
// the join is FOR. A toolless stage (PLAN) emitting JSON across two max_tokens
// cuts has no productive turn to reset on, so every fragment still concatenates.
func TestSessionPlanContinuationStillJoins(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Truncated(`{"tasks":[`),
		enginetest.Truncated(`{"title":"one"}`),
		enginetest.Text(`]}`),
	}}
	s := testSession(t, p)
	spec := buildSpecForTest()
	spec.PreloadTools = nil // toolless, like PLAN

	res, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := `{"tasks":[{"title":"one"}]}`; res.Content != want {
		t.Fatalf("content=%q, want %q - the multi-cut JSON join broke", res.Content, want)
	}
}

// TestSessionWedgeIsNotDone: a segment that gives up because the provider wedged
// must not be recorded as a clean finish. EndDone is what publishLiveOutcome
// reads to decide whether to tell the owner the change is live.
func TestSessionWedgeIsNotDone(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(echoCall("c1", "real work")),
		enginetest.Empty(),
		enginetest.Empty(), // streak cap -> give up
		enginetest.Text("never reached"),
	}}
	s := testSession(t, p)

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Ending != EndFailed || res.EndDetail != endWedged.detail {
		t.Fatalf("end=%q, want %q - a wedged segment reports a clean finish",
			res.Ending, endWedged.detail)
	}
}

// TestSessionContinuationCapIsNotDone: same contract for the other give-up path.
// A toolless stage that keeps getting cut off never lands a turn; exhausting the
// continuation budget is a failure, not a completion.
func TestSessionContinuationCapIsNotDone(t *testing.T) {
	steps := make([]enginetest.Step, 0, maxContinuations+1)
	for i := 0; i < maxContinuations+1; i++ {
		steps = append(steps, enginetest.Truncated("cut "))
	}
	p := &enginetest.Provider{Steps: steps}
	s := testSession(t, p)
	spec := buildSpecForTest()
	spec.PreloadTools = nil
	spec.MaxIterations = maxContinuations + 2

	res, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Ending != EndFailed || res.EndDetail != endContinuationCap.detail {
		t.Fatalf("end=%q, want %q - an exhausted continuation budget reports a clean finish",
			res.Ending, endContinuationCap.detail)
	}
}

// TestSessionRepeatGuardForceStops: the same exact call issued 4 turns in a
// row is refused and the session force-stops.
func TestSessionRepeatGuardForceStops(t *testing.T) {
	same := echoCall("cx", "same")
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(same), enginetest.Calls(same),
		enginetest.Calls(same), enginetest.Calls(same),
		enginetest.Text("never reached"),
	}}
	s := testSession(t, p)

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The repeat guard stalls the session: a loop that cannot make progress, not
	// a budget that ran out.
	if res.Ending != EndFailed || res.EndDetail != endRepeatStall.detail {
		t.Fatalf("ending=%s/%s, want %s/%s", res.Ending, res.EndDetail, EndFailed, endRepeatStall.detail)
	}
	if p.CallCount() != 4 {
		t.Fatalf("provider calls = %d, want 4", p.CallCount())
	}
	// The 4th turn's tool result must carry the stall refusal.
	last := res.Messages[len(res.Messages)-1]
	if last.Role != llm.RoleTool || !contains(last.Content, "repeated_call_stall") {
		t.Fatalf("expected stall refusal in final tool result, got %q", last.Content)
	}
}

// TestSessionRolloverReseeds: crossing the input-token threshold ends the
// segment and reseeds via spec.Reseed.
func TestSessionRolloverReseeds(t *testing.T) {
	// Segment 1's call reports input tokens over the threshold; segment 2's
	// turns stay under it so the session can finish there.
	small := func(st enginetest.Step) enginetest.Step {
		st.Resp.Usage = llm.Usage{InputTokens: 2, OutputTokens: 1}
		return st
	}
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(echoCall("c1", "fat turn")),          // in=10 > threshold 5
		small(enginetest.Calls(echoCall("c2", "after roll"))), // segment 2
		small(enginetest.Text("done after rollover")),
	}}
	s := testSession(t, p)

	spec := buildSpecForTest()
	spec.RolloverInputTokens = 5
	reseeds := 0
	spec.Reseed = func() ([]llm.Message, error) {
		reseeds++
		return []llm.Message{{Role: llm.RoleUser, Content: "recap: continue the task"}}, nil
	}

	res, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if reseeds != 1 || res.Segments != 2 {
		t.Fatalf("reseeds=%d segments=%d", reseeds, res.Segments)
	}
	if res.Content != "done after rollover" {
		t.Fatalf("content=%q", res.Content)
	}
	// Fresh segment starts from the recap seed.
	if res.Messages[0].Content != "recap: continue the task" {
		t.Fatalf("segment 2 seed = %q", res.Messages[0].Content)
	}
}

// TestFindToolLoadSurvivesRollover: the prompts promise a tool loaded with find_tool
// stays available for the rest of the run. A rollover's fresh segment rebuilds its tool
// set from the preloads, so the load must be carried there, and reported on the result
// for a warm fix-pass - without touching the caller's spec.
func TestFindToolLoadSurvivesRollover(t *testing.T) {
	small := func(st enginetest.Step) enginetest.Step {
		st.Resp.Usage = llm.Usage{InputTokens: 2, OutputTokens: 1}
		return st
	}
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(llm.ToolCall{ID: "f1", Name: "find_tool", Arguments: `{"names":["manage_guides"]}`}), // in=10 > 5
		small(enginetest.Calls(llm.ToolCall{ID: "g1", Name: "manage_guides", Arguments: `{"action":"list"}`})),
		small(enginetest.Text("done")),
	}}
	s := testSession(t, p)
	spec := buildSpecForTest()
	spec.RolloverInputTokens = 5
	spec.Reseed = func() ([]llm.Message, error) {
		return []llm.Message{{Role: llm.RoleUser, Content: "recap"}}, nil
	}
	callerPreloads := spec.PreloadTools

	res, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Segments != 2 {
		t.Fatalf("segments = %d, want a rollover", res.Segments)
	}
	hasTool := func(defs []llm.ToolDef, name string) bool {
		for _, d := range defs {
			if d.Name == name {
				return true
			}
		}
		return false
	}
	if !hasTool(p.Requests[1].Tools, "manage_guides") {
		t.Error("the segment after a rollover lost the tool find_tool loaded before it")
	}
	notes := false
	for _, m := range p.Requests[1].Messages {
		if contains(m.Content, "Notes for the tools you loaded earlier") && contains(m.Content, "manage_guides") {
			notes = true
		}
	}
	if !notes {
		t.Error("the segment after a rollover did not get the routing notes of the tool find_tool loaded before it")
	}
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool && contains(m.Content, "not loaded yet") {
			t.Errorf("call after rollover was refused as unloaded: %s", m.Content)
		}
	}
	found := false
	for _, n := range res.Tools {
		if n == "manage_guides" {
			found = true
		}
	}
	if !found {
		t.Errorf("result tools %v do not carry the find_tool load (warm fix-pass needs it)", res.Tools)
	}
	if len(callerPreloads) != 1 || len(spec.PreloadTools) != 1 {
		t.Errorf("caller's preload slice was mutated: %v", spec.PreloadTools)
	}
}

// TestSessionRolloverKeysOnTotalContext: the rollover/compaction triggers key on
// TOTAL context size (uncached + cache-read + cache-creation), not the uncached
// InputTokens alone. A call whose uncached portion is tiny but whose cached prefix
// is large still crosses the threshold. Keying on the uncached count alone is wrong
// in both directions: a provider cache eviction spikes it and forces a
// cache-busting compaction, while a fully-cached prompt grows unbounded without
// ever tripping rollover.
func TestSessionRolloverKeysOnTotalContext(t *testing.T) {
	bigCached := func(st enginetest.Step) enginetest.Step {
		st.Resp.Usage = llm.Usage{InputTokens: 2, CacheReadInputTokens: 200, OutputTokens: 1}
		return st
	}
	small := func(st enginetest.Step) enginetest.Step {
		st.Resp.Usage = llm.Usage{InputTokens: 2, OutputTokens: 1}
		return st
	}
	p := &enginetest.Provider{Steps: []enginetest.Step{
		bigCached(enginetest.Calls(echoCall("c1", "cached but large"))), // total 202 > threshold 100
		small(enginetest.Calls(echoCall("c2", "after roll"))),           // segment 2
		small(enginetest.Text("done after rollover")),
	}}
	s := testSession(t, p)

	spec := buildSpecForTest()
	spec.RolloverInputTokens = 100
	reseeds := 0
	spec.Reseed = func() ([]llm.Message, error) {
		reseeds++
		return []llm.Message{{Role: llm.RoleUser, Content: "recap: continue the task"}}, nil
	}

	res, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if reseeds != 1 || res.Segments != 2 {
		t.Fatalf("reseeds=%d segments=%d; uncached=2 is below the 100 threshold but total 202 must still roll over", reseeds, res.Segments)
	}
	if res.Content != "done after rollover" {
		t.Fatalf("content=%q", res.Content)
	}
}

// TestSessionPauseOnOwnerQuestion: a RequestsPause tool pauses the session.
func TestSessionPauseOnOwnerQuestion(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(llm.ToolCall{ID: "q1", Name: "test_probe_tool",
			Arguments: `{"action":"ask","question":"blue or green?"}`}),
		enginetest.Text("never reached"),
	}}
	s := testSession(t, p)

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Paused || res.Ending != EndPaused {
		t.Fatalf("paused=%v ending=%q", res.Paused, res.Ending)
	}
	if p.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", p.CallCount())
	}
}

// TestSessionPauseOnApprovalRequired: a failed result tagged with the approval
// code pauses the session, so a destructive action awaiting owner approval
// parks the worker rather than letting the build run on.
func TestSessionPauseOnApprovalRequired(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(llm.ToolCall{ID: "a1", Name: "test_probe_tool",
			Arguments: `{"action":"needs_approval"}`}),
		enginetest.Text("never reached"),
	}}
	s := testSession(t, p)

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Paused || res.Ending != EndPaused {
		t.Fatalf("paused=%v ending=%q", res.Paused, res.Ending)
	}
	if p.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 (segment must end before the next turn)", p.CallCount())
	}
}

// TestCacheRouteKey pins the provider cache-node routing-hint format. The key is stage plus
// a digest of the HEAD of the system prompt, so every request carrying a byte-identical
// cached head routes to the same warm node: a build's segments, its fix-pass runs,
// consecutive live-chat turns, AND separate projects whose prompts differ only in the
// Deferred capabilities tail. Drift here would silently fragment that routing and re-bill
// the shared prefix at full price.
func TestCacheRouteKey(t *testing.T) {
	const sys = "SYSTEM PROMPT BODY"
	got := cacheRouteKey("build", sys)
	if !strings.HasPrefix(got, "build:") {
		t.Fatalf("cacheRouteKey(build) = %q, want a build: prefix", got)
	}
	if got != cacheRouteKey("build", sys) {
		t.Fatal("cacheRouteKey must be deterministic")
	}
	// Stage is part of the key (plan/build/live have different system prompts).
	if a, b := cacheRouteKey("build", sys), cacheRouteKey("live", sys); a == b {
		t.Fatalf("different stages must yield different keys, both = %q", a)
	}
	// A short prompt is keyed whole, so a real difference still splits the shard.
	if a, b := cacheRouteKey("build", sys), cacheRouteKey("build", sys+" extra"); a == b {
		t.Fatalf("different short system prompts must yield different keys, both = %q", a)
	}
	// The point of the head digest: two projects sharing a long head but differing in the
	// tail SHARE a node. Projects share most of the head and diverge only at Deferred
	// capabilities, so hashing the whole prompt would have given each its own shard and
	// shared nothing, which is what the per-project key it replaced already did.
	head := strings.Repeat("shared head ", routeKeyHeadBytes/12+64)
	if a, b := cacheRouteKey("build", head+"tail A"), cacheRouteKey("build", head+"tail B"); a != b {
		t.Fatalf("prompts sharing a head longer than routeKeyHeadBytes must share a key, got %q and %q", a, b)
	}
	// A difference INSIDE the head still splits, so a corpus edit or a Wave bump rotates
	// the key rather than co-locating old and new prefixes forever.
	if a, b := cacheRouteKey("build", "X"+head), cacheRouteKey("build", "Y"+head); a == b {
		t.Fatalf("a difference inside the head must split the key, both = %q", a)
	}
}

// TestReadOnlyStreakProbeIsProgress: an active project probe (manage_testing) is
// forward progress, so an end-of-build verification sweep does NOT advance the
// "only reading, no edit" stall streak the way passive source reads (manage_files
// get) do. Without this, a run of test_page/test_endpoint/test_auth_flow after the
// last write falsely trips the read-only nudge.
func TestReadOnlyStreakProbeIsProgress(t *testing.T) {
	s := testSession(t, &enginetest.Provider{})
	spec := buildSpecForTest() // StageBuild: ReadOnlyNudge and ReadOnlyForceStop set

	getCall := llm.ToolCall{ID: "g", Name: "manage_files", Arguments: `{"action":"get","file_path":"/x"}`}
	probeCall := llm.ToolCall{ID: "p", Name: "manage_testing", Arguments: `{"action":"test_page","page_path":"/"}`}

	// Preconditions: the get classifies read-only (advances the streak) and the
	// probe is a read-only PROBE (the case this fix reclassifies as progress). If
	// either changes, the test below would be meaningless, so assert them.
	var ga, pa map[string]any
	_ = json.Unmarshal([]byte(getCall.Arguments), &ga)
	_ = json.Unmarshal([]byte(probeCall.Arguments), &pa)
	if !s.isReadOnlyToolCall(getCall, ga) {
		t.Fatal("precondition: manage_files get should classify read-only")
	}
	if !s.isReadOnlyToolCall(probeCall, pa) || !isReadOnlyProbe(probeCall.Name) {
		t.Fatal("precondition: manage_testing test_page should be a read-only probe")
	}

	// Passive reads accumulate, and the ReadOnlyNudge-th fires the nudge -
	// the guard still catches a genuine re-reading stall.
	passive := &segState{tr: NewTranscript()}
	for i := 0; i < spec.ReadOnlyNudge; i++ {
		s.updateReadOnlyStreak(&spec, passive, []llm.ToolCall{getCall})
	}
	if passive.readOnlyRun != spec.ReadOnlyNudge {
		t.Fatalf("%d passive reads: readOnlyRun = %d, want the same", spec.ReadOnlyNudge, passive.readOnlyRun)
	}
	if msgs := passive.tr.Messages(); len(msgs) != 1 || !contains(msgs[0].Content, "only reading") {
		t.Fatalf("last passive read should append the read-only nudge, got %d msgs", passive.tr.Len())
	}

	// A probe turn resets the streak instead of pushing it toward the nudge, even
	// after several passive reads, and appends nothing.
	sweep := &segState{tr: NewTranscript()}
	for i := 0; i < spec.ReadOnlyNudge-1; i++ {
		s.updateReadOnlyStreak(&spec, sweep, []llm.ToolCall{getCall})
	}
	s.updateReadOnlyStreak(&spec, sweep, []llm.ToolCall{probeCall})
	if sweep.readOnlyRun != 0 {
		t.Fatalf("a probe turn must reset the read-only streak, got %d", sweep.readOnlyRun)
	}
	if sweep.tr.Len() != 0 {
		t.Fatalf("a probe sweep must not append a nudge, got %d msgs", sweep.tr.Len())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestSessionAcceptDiscardsUnusableResponse: a response the spec's Accept rejects
// is evicted from the replay cache, so the next identical run reaches the provider
// instead of replaying the same unusable bytes forever. A response Accept allows
// stays cached and replays for free.
func TestSessionAcceptDiscardsUnusableResponse(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text("unusable output"), enginetest.Text("fresh output"),
	}}
	s := testSession(t, p)

	spec := buildSpecForTest()
	spec.PreloadTools = nil

	// No Accept: the response is cached and an identical run replays it for free.
	if _, err := s.Run(context.Background(), spec); err != nil {
		t.Fatalf("first run: %v", err)
	}
	replay, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if replay.Content != "unusable output" || p.CallCount() != 1 {
		t.Fatalf("expected a free replay: content=%q calls=%d", replay.Content, p.CallCount())
	}

	// Accept rejects it: the session evicts on its own, with no caller involved,
	// and the same run now reaches the provider.
	spec.Accept = func(content string) error {
		if content == "unusable output" {
			return fmt.Errorf("unparseable")
		}
		return nil
	}
	if _, err := s.Run(context.Background(), spec); err != nil {
		t.Fatalf("run with Accept: %v", err)
	}
	after, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("run after discard: %v", err)
	}
	if after.Content != "fresh output" || p.CallCount() != 2 {
		t.Fatalf("Accept rejection did not force a fresh call: content=%q calls=%d", after.Content, p.CallCount())
	}
}

// A model recorded as unable to call tools cannot be coaxed into a first call, so
// the iteration-0 punt path must end the session instead of re-prompting it.
func TestModelLacksToolsFromCapStore(t *testing.T) {
	p := &enginetest.Provider{}
	k := llm.ModelKey{Provider: p.Name(), Model: "no-tools-model"}
	t.Cleanup(llm.Caps.Reset)
	s := &session{}
	st := &segState{rp: resolvedProvider{Provider: p, Model: "no-tools-model"}}

	if s.modelLacksTools(st) {
		t.Fatal("an unknown capability must be treated as supported")
	}
	llm.Caps.Declare(k, llm.CapTools, llm.No)
	if !s.modelLacksTools(st) {
		t.Fatal("a declared no-tools model must be detected")
	}
	llm.Caps.Declare(k, llm.CapTools, llm.Yes)
	if s.modelLacksTools(st) {
		t.Fatal("a tool-capable model must not be flagged")
	}
}

func askCall(id, question string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: "manage_chat",
		Arguments: fmt.Sprintf(`{"action":"ask","question":%q}`, question)}
}

func pendingQuestionTotal(t *testing.T, s *session) int {
	t.Helper()
	var n int
	if err := s.pdb.QueryRow("SELECT COUNT(*) FROM of_questions WHERE status = 'pending'").Scan(&n); err != nil {
		t.Fatalf("count questions: %v", err)
	}
	return n
}

// TestSessionBatchesAsksInOneTurn: several manage_chat ask calls in one turn all
// reach the owner (one grouped form), while a non-pause call queued after the
// first question is still skipped.
func TestSessionBatchesAsksInOneTurn(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(
			askCall("q1", "Which palette?"),
			askCall("q2", "Which font?"),
			echoCall("e1", "should be skipped"),
			askCall("q3", "Which logo?"),
		),
		enginetest.Text("never reached"),
	}}
	s := testSession(t, p)
	spec := buildSpecForTest()
	spec.PreloadTools = append(spec.PreloadTools, "manage_chat")

	res, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Paused {
		t.Fatalf("session must pause after the asks, ending=%q", res.Ending)
	}
	if got := pendingQuestionTotal(t, s); got != 3 {
		t.Fatalf("pending questions = %d, want all 3 asks queued", got)
	}
	if p.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 (no turn after the pause)", p.CallCount())
	}
}

// TestSessionCapsAsksPerTurn: a runaway turn cannot flood the owner.
func TestSessionCapsAsksPerTurn(t *testing.T) {
	var calls []llm.ToolCall
	for i := 0; i < maxAsksPerTurn+4; i++ {
		calls = append(calls, askCall(fmt.Sprintf("q%d", i), fmt.Sprintf("Question number %d?", i)))
	}
	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Calls(calls...), enginetest.Text("never reached")}}
	s := testSession(t, p)
	spec := buildSpecForTest()
	spec.PreloadTools = append(spec.PreloadTools, "manage_chat")

	if _, err := s.Run(context.Background(), spec); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := pendingQuestionTotal(t, s); got != maxAsksPerTurn {
		t.Fatalf("pending questions = %d, want the cap %d", got, maxAsksPerTurn)
	}
}

// A structured response that runs to the output cap in a runaway whitespace run (a
// strict-schema decoder allows padding forever) keeps only its real JSON prefix. The
// padding must not be re-sent in the continuation request, joined into the result, or
// asked to be "continued", and the retry asks for compact output.
func TestSessionPlanRunawayWhitespaceIsTrimmedBeforeContinuation(t *testing.T) {
	padding := strings.Repeat("\t \n", 400)
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Truncated(`{"tasks":[` + padding),
		enginetest.Text(`]}`),
	}}
	s := testSession(t, p)
	spec := buildSpecForTest()
	spec.PreloadTools = nil // toolless, like PLAN

	res, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := `{"tasks":[]}`; res.Content != want {
		t.Fatalf("content=%q, want %q - padding leaked into the joined JSON", res.Content, want)
	}
	if len(p.Requests) != 2 {
		t.Fatalf("requests=%d, want 2", len(p.Requests))
	}
	second := p.Requests[1]
	var sent strings.Builder
	for _, m := range second.Messages {
		sent.WriteString(m.Content)
		sent.WriteByte('\n')
	}
	if strings.Contains(sent.String(), strings.Repeat("\t \n", 20)) {
		t.Fatal("the continuation request re-sent the runaway padding")
	}
	if !strings.Contains(sent.String(), "compactly") {
		t.Fatalf("continuation prompt does not ask for compact output:\n%s", sent.String())
	}
}

func TestTrimRunawayWhitespace(t *testing.T) {
	long := strings.Repeat(" ", runawayWhitespaceRun)
	for _, tc := range []struct {
		name, in, want string
		trimmed        bool
	}{
		{"short pretty tail is kept", "{\n  \"a\": 1\n}\n\n", "{\n  \"a\": 1\n}\n\n", false},
		{"long tail is cut", `{"a":` + long, `{"a":`, true},
		{"only padding", long, "", true},
		{"interior padding is not touched", `{"a":"` + long + `"}`, `{"a":"` + long + `"}`, false},
	} {
		got, ok := trimRunawayWhitespace(tc.in)
		if got != tc.want || ok != tc.trimmed {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, got, ok, tc.want, tc.trimmed)
		}
	}
}

// A probe's read-only classification follows what the call does. test_endpoint and
// test_url with a write method change project data, and test_auth_flow registers a
// probe user; none of them may count as a read in the loop's write accounting.
func TestProbeReadOnlyFollowsTheCall(t *testing.T) {
	s := testSession(t, &enginetest.Provider{})
	for _, tc := range []struct {
		name string
		args string
		want bool
	}{
		{"test_endpoint", `{"action":"test_endpoint","endpoint_path":"posts"}`, true},
		{"test_endpoint", `{"action":"test_endpoint","endpoint_path":"posts","method":"GET"}`, true},
		{"test_endpoint", `{"action":"test_endpoint","endpoint_path":"posts","method":"post"}`, false},
		{"test_url", `{"action":"test_url","url_path":"/","method":"HEAD"}`, true},
		{"test_url", `{"action":"test_url","url_path":"/api/x","method":"DELETE"}`, false},
		{"test_auth_flow", `{"action":"test_auth_flow","endpoint_path":"auth"}`, false},
		{"test_page", `{"action":"test_page","page_path":"/"}`, true},
	} {
		var args map[string]any
		_ = json.Unmarshal([]byte(tc.args), &args)
		call := llm.ToolCall{ID: "x", Name: "manage_testing", Arguments: tc.args}
		if got := s.isReadOnlyToolCall(call, args); got != tc.want {
			t.Errorf("%s %s: read-only = %v, want %v", tc.name, tc.args, got, tc.want)
		}
	}
}
