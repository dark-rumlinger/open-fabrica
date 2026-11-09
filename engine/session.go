package engine

// The unified session loop: ONE implementation runs every stage. A session is a
// sequence of SEGMENTS, each one append-only transcript; context overflow ends a
// segment and the next reseeds deterministically from DB state via spec.Reseed
// (also the crash-resume mechanism). Within a segment everything appends and the
// cached prefix is never rewritten in place. Context growth is bounded by the read
// tracker (a repeat read of unchanged bytes returns a stub), the project_state
// snapshot's baseline/delta with a periodic re-baseline, and the rollover: once a
// call's total context passes the threshold the segment ends and the next reseeds
// cold from DB.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"crypto/sha256"
	"encoding/json"

	"openfabrica/db"
	"openfabrica/engine/snapshotbuild"
	"openfabrica/internal/sqliteretry"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// sessionEvents are the optional UI/chat hooks a host wires in. All fields
// are normalized to no-ops by newSession so call sites stay unconditional.
type sessionEvents struct {
	thinking   func(active bool)
	assistant  func(content string, toolCalls []llm.ToolCall)
	toolStart  func(tc llm.ToolCall, args map[string]any)
	toolResult func(tc llm.ToolCall, args map[string]any, result string, success bool)
	progress   func(text string)
}

func (e *sessionEvents) normalize() {
	if e.thinking == nil {
		e.thinking = func(bool) {}
	}
	if e.assistant == nil {
		e.assistant = func(string, []llm.ToolCall) {}
	}
	if e.toolStart == nil {
		e.toolStart = func(llm.ToolCall, map[string]any) {}
	}
	if e.toolResult == nil {
		e.toolResult = func(llm.ToolCall, map[string]any, string, bool) {}
	}
	if e.progress == nil {
		e.progress = func(string) {}
	}
}

// sessionSeq disambiguates segment ids minted within the same millisecond by
// concurrent workers (of_sessions.id is a primary key).
var sessionSeq atomic.Uint64

// session executes SessionSpecs for one project. It is created per worker (or
// per test) and reused across specs; per-spec state lives in segState.
type session struct {
	deps       *Deps
	pdb        *db.ProjectDB
	projectID  int
	runID      string
	logger     *slog.Logger
	caller     *llmCaller
	resolver   providerSource
	parseCache *snapshotbuild.ModuleParseCache
	events     sessionEvents

	// rolloverCeiling, when set, is a prompt size a provider has rejected as too large
	// (scaled down for headroom): every later segment rolls over at or below it, so a
	// model whose window is smaller than the default trigger costs one rejected call,
	// not one per segment.
	rolloverCeiling int

	// requestHashes are the replay-cache keys of every call this run made, for spec.Accept
	// to evict on. Session-internal: acting on them means owning the rule for when eviction
	// is correct, which is the whole reason Accept exists.
	requestHashes []string

	// stopFn, when set, reports an owner stop request. The loop consults it
	// BETWEEN turns only: cancelling mid-call would abandon a paid response and
	// could land inside a half-run tool; a turn boundary leaves the transcript
	// coherent and the run resumable. nil = no host to ask.
	stopFn func() bool

	// busyFn, when set, brackets the whole run so the host knows a session is in
	// flight. Distinct from stopFn's question: this one is "is anything running",
	// which the worker's State cannot answer on its own. nil = no host to tell.
	busyFn func(bool)
}

// ownerStopRequested reports whether the host has asked this session to wind up.
func (s *session) ownerStopRequested() bool { return s.stopFn != nil && s.stopFn() }

// newSession wires a session for one project worker.
func newSession(deps *Deps, pdb *db.ProjectDB, projectID int, runID string, limiter *llmLimiter, dbLog llm.LLMLogger, logger *slog.Logger, ev sessionEvents) *session {
	ev.normalize()
	if logger == nil {
		logger = slog.Default()
	}
	return &session{
		deps:       deps,
		pdb:        pdb,
		projectID:  projectID,
		runID:      runID,
		logger:     logger,
		caller:     &llmCaller{limiter: limiter, pdb: pdb, dbLog: dbLog, logger: logger, budgetFn: deps.DailyTokenBudgetFn},
		resolver:   newProviderResolver(deps, projectID),
		parseCache: snapshotbuild.NewModuleParseCache(),
		events:     ev,
	}
}

// Run executes one spec to completion across as many segments as needed.
func (s *session) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	if s.busyFn != nil {
		s.busyFn(true)
		defer s.busyFn(false)
	}

	// Accept eviction must only cover this Run's calls: PLAN reuses one session
	// for several Run calls, and a late failure must not evict earlier, usable
	// responses.
	s.requestHashes = s.requestHashes[:0]

	res := SessionResult{}
	seed := spec.Seed
	// find_tool appends its loads to spec.PreloadTools so a rollover keeps them; own the
	// slice first so those appends can never write into the caller's backing array.
	spec.PreloadTools = append([]string(nil), spec.PreloadTools...)
	preloaded := len(spec.PreloadTools) // later entries are find_tool loads

	for segment := 1; segment <= maxSegments; segment++ {
		res.Segments = segment
		segRes, rollover, err := s.runSegment(ctx, &spec, seed, segment, &res)
		if err != nil {
			return res, err
		}
		if !rollover {
			res.Content = segRes.Content
			res.Model = segRes.Model
			res.Messages = segRes.Messages
			res.Paused = segRes.Paused
			res.Ending = segRes.Ending
			res.EndDetail = segRes.EndDetail
			res.LastContextTokens = segRes.LastContextTokens
			res.Reads = segRes.Reads
			res.Tools = spec.PreloadTools
			s.enforceAccept(&spec, res.Content)
			return res, nil
		}
		// Rollover: reseed deterministically from DB state and go again.
		s.logger.Info("session rollover: reseeding fresh segment", "segment", segment+1)
		seed, err = spec.Reseed()
		if err != nil {
			return res, fmt.Errorf("rollover reseed failed: %w", err)
		}
		if note := s.loadedToolNotes(&spec, spec.PreloadTools[preloaded:]); note != "" {
			seed = append(seed, llm.Message{Role: llm.RoleUser, Content: note})
		}
	}
	res.Ending = endSegmentsExhausted.outcome
	res.EndDetail = endSegmentsExhausted.detail
	return res, fmt.Errorf("session exceeded %d segments without completing", maxSegments)
}

// loadedToolNotes restates the routing notes of the tools find_tool loaded before a
// rollover. They arrived only in find_tool's result, which the fresh segment no
// longer holds, while the schemas themselves are carried over.
func (s *session) loadedToolNotes(spec *SessionSpec, names []string) string {
	if len(names) == 0 {
		return ""
	}
	caps, err := llm.LoadCapabilities(s.deps.DB.DB)
	if err != nil {
		return ""
	}
	prose := s.deps.ToolRegistry.RoutingProse(names, caps, strings.ToLower(spec.Stage))
	if prose == "" {
		return ""
	}
	return "Notes for the tools you loaded earlier with find_tool (that result is no longer in context):\n\n" + prose
}

// enforceAccept applies the spec's usability contract to the final content and, on failure,
// evicts the responses behind it from the replay cache: keyed on the request, a
// structurally-wrong response would replay forever (retries included). Here, not at the
// call site, so no caller can forget the discard.
func (s *session) enforceAccept(spec *SessionSpec, content string) {
	if spec.Accept == nil || len(s.requestHashes) == 0 {
		return
	}
	err := spec.Accept(content)
	if err == nil {
		return
	}
	s.caller.cacheDiscard(s.requestHashes)
	s.logger.Info("discarded unusable responses from replay cache",
		"calls", len(s.requestHashes), "reason", err)
}

// runSegment executes one append-only transcript segment. Returns rollover=true when the
// segment ended for context overflow and the caller should reseed. The loop is one turn
// per iteration: gate (stop/rollover), prepare the snapshot, call the model, account for
// the call, then either handle a tool-call-free reply or run the turn's tool calls.
func (s *session) runSegment(ctx context.Context, spec *SessionSpec, seed []llm.Message, segment int, total *SessionResult) (SessionResult, bool, error) {
	sessionID := fmt.Sprintf("sess_%d_%d", time.Now().UnixMilli(), sessionSeq.Add(1))
	s.recordSegmentStart(sessionID, segment)

	rp, err := s.resolver.resolve()
	if err != nil {
		s.recordSegmentEnd(sessionID, endTransportError.detail, 0)
		return SessionResult{}, false, err
	}
	st := s.newSegState(spec, seed, segment, sessionID, rp)

	res := SessionResult{}
	finish := func(e ending) (SessionResult, bool, error) {
		res.Ending = e.outcome
		res.EndDetail = e.detail
		res.Messages = st.tr.Messages()
		res.Paused = e.outcome == EndPaused
		res.LastContextTokens = st.lastContextTokens
		res.Reads = st.reads
		res.Saves, res.Patches, res.AnchorMisses, res.NoOps = st.saves, st.patches, st.anchorMisses, st.noops
		st.addEditCounters(total)
		s.recordSegmentEnd(sessionID, e.detail, st.iteration+1)
		return res, false, nil
	}

	for i := 0; i < spec.MaxIterations+st.patchRetryGrants; i++ {
		st.iteration = i
		if ctx.Err() != nil {
			s.recordSegmentEnd(sessionID, endTransportError.detail, i)
			return res, false, ctx.Err()
		}

		// Owner stop, checked before committing to another call so a stop that
		// arrives between turns costs nothing. The mid-turn case is caught after
		// tool execution (runToolTurn); both end the segment the same way.
		if s.ownerStopRequested() {
			s.logger.Info("owner stop: ending segment", "stage", spec.Stage, "iteration", i)
			return finish(endOwnerStop)
		}

		// Rollover BEFORE the next call so no tool calls are lost: the prior
		// call's total context is what the next one would exceed.
		if spec.Reseed != nil && st.lastContextTokens >= st.rolloverAt {
			s.logger.Info("context rollover", "context_tokens", st.lastContextTokens, "threshold", st.rolloverAt, "iteration", i)
			st.addEditCounters(total)
			s.recordSegmentEnd(sessionID, endRolloverDetail, i)
			return res, true, nil
		}

		s.prepareTurn(ctx, st)
		req := s.buildRequest(spec, st)
		resp, cacheHit, callErr := s.callModel(ctx, spec, st, req)
		if callErr != nil {
			if s.overflowRollsOver(spec, st, callErr) {
				st.addEditCounters(total)
				s.recordSegmentEnd(sessionID, endRolloverDetail, i)
				return res, true, nil
			}
			s.recordSegmentEnd(sessionID, endTransportError.detail, i)
			return res, false, fmt.Errorf("LLM call failed at iteration %d: %w", i, callErr)
		}

		st.tr.Seal() // everything sent is now immutable
		st.accountUsage(&res, total, resp, cacheHit, req.TotalRequestBytes)
		s.logger.Info("llm iteration",
			"stage", spec.Stage, "iter", i, "segment", segment,
			"in", resp.Usage.InputTokens, "out", resp.Usage.OutputTokens,
			"cache_read", resp.Usage.CacheReadInputTokens,
			"replay_hit", cacheHit,
			"stable_prefix_bytes", req.StablePrefixBytes, "msgs", st.tr.Len(), "tools", len(st.toolDefs))

		// A structured (no-tool) response cut at the output cap by a runaway whitespace
		// run keeps only its real prefix, in the result and in the transcript alike.
		st.runawayTrimmed = false
		if len(st.toolDefs) == 0 && len(resp.ToolCalls) == 0 && resp.StopReason == "max_tokens" {
			if trimmed, ok := trimRunawayWhitespace(resp.Content); ok {
				s.logger.Warn("structured response ended in runaway whitespace; trimmed",
					"stage", spec.Stage, "dropped_bytes", len(resp.Content)-len(trimmed))
				resp.Content, st.runawayTrimmed = trimmed, true
			}
		}

		// Multi-continuation content joins (plan JSON across max_tokens cuts).
		if st.continuation > 0 && len(resp.ToolCalls) == 0 {
			res.Content += resp.Content
		} else {
			res.Content = resp.Content
		}
		res.Model = resp.Model

		assistantMsg := llm.Message{Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls}
		if resp.Content != "" || len(resp.ToolCalls) > 0 {
			s.events.assistant(resp.Content, resp.ToolCalls)
		}

		// No tool calls: continuation, re-prompt, or done.
		if len(resp.ToolCalls) == 0 {
			if e := s.handleNoToolCalls(spec, st, resp, assistantMsg); e != nil {
				return finish(*e)
			}
			continue
		}

		// Truncated tool call: arguments that don't parse on a turn that hit
		// the output cap. Drop the cut-off call (only the last can be cut).
		if handled, capped := s.handleTruncatedToolCalls(spec, st, resp, &assistantMsg); capped {
			return finish(endContinuationCap)
		} else if handled {
			continue
		}

		if e := s.runToolTurn(ctx, spec, st, resp, assistantMsg, &res, total); e != nil {
			return finish(*e)
		}
	}
	// The loop ran out of iterations with the model still working: a budget, not a
	// verdict on the work.
	return finish(endIterationCap)
}

// newSegState builds one segment's loop state: a fresh transcript over seed, the
// segment's tool set, and the per-segment call parameters that stay fixed for it.
func (s *session) newSegState(spec *SessionSpec, seed []llm.Message, segment int, sessionID string, rp resolvedProvider) *segState {
	st := &segState{
		tr:     NewTranscript(seed...),
		guards: NewGuardState(),
		// One ReadTracker PER SEGMENT, not per session: a rollover reseed starts a
		// fresh transcript with none of the prior segment's file bodies, so a
		// carried tracker would stub the model's re-orientation reads. Within a
		// segment, repeat reads still dedup.
		reads:     tools.NewReadTracker(),
		snapshot:  sessionSnapshot{mode: spec.Snapshot},
		served:    map[string]bool{},
		sessionID: sessionID,
		segment:   segment,
		rp:        rp,
		// Routing hint for the provider prompt cache, computed once: the system prompt is
		// fixed for the whole segment, so re-digesting it every turn would be waste.
		routeKey:   cacheRouteKey(spec.Stage, spec.System),
		rolloverAt: spec.RolloverInputTokens,
	}
	if st.rolloverAt == 0 {
		st.rolloverAt = defaultRolloverInputTokens
	}
	if cw, ok := rp.Provider.(llm.ContextWindowProvider); ok {
		st.rolloverAt = rolloverForWindow(st.rolloverAt, cw.ContextWindow())
	}
	if s.rolloverCeiling > 0 && s.rolloverCeiling < st.rolloverAt {
		st.rolloverAt = s.rolloverCeiling
	}
	// Warm continuation: the first segment's seed is a prior session's transcript. Adopt its
	// ReadTracker (the bodies it recorded are still in the carried messages) and seal the seed
	// so stable_prefix telemetry reports the carried prefix. Later segments come from Reseed
	// (a cold, pure-DB seed) and take the fresh-tracker/unsealed path.
	if segment == 1 {
		if spec.CarryReads != nil {
			st.reads = spec.CarryReads
		}
		if spec.SealSeed {
			st.tr.Seal()
		}
	}
	st.toolDefs = s.resolveTools(spec)
	st.rebuildAllowed()
	// The segment's opening tool set is what the first call carries, so it is not a
	// change; only a later find_tool append is.
	st.sentToolsGen = st.toolsGen
	return st
}

// prepareTurn brings project_state up to date before a call.
func (s *session) prepareTurn(ctx context.Context, st *segState) {
	// Re-issue a full project_state snapshot once the delta chain outgrows what
	// the model can replay. A snapshot is a few KB and append-only, so it
	// re-buys no cached prefix.
	if st.snapshot.deltas >= rebaselineAfterDeltas {
		st.snapshot.forceBaseline = true
	}

	// Refresh project_state (skipped mid-read-only-streak, when an unchanged snapshot
	// would only bloat the turn). A pending re-baseline overrides that skip: a read-only
	// streak is the model hunting for something it cannot find, exactly when a stale
	// snapshot is the cause.
	if st.iteration == 0 || st.readOnlyRun == 0 || st.snapshot.forceBaseline {
		s.refreshSnapshot(ctx, &st.snapshot, st.tr)
	}
}

// buildRequest assembles this turn's completion request from the segment state.
func (s *session) buildRequest(spec *SessionSpec, st *segState) llm.CompletionRequest {
	// Prefix diagnostics: the sealed prefix is append-only, so the counters
	// come straight from the lengths recorded at Seal time.
	stableBytes, stableMsgs := st.tr.StablePrefix()
	return llm.CompletionRequest{
		Model:             st.rp.Model,
		System:            spec.System,
		Messages:          st.tr.Messages(),
		Tools:             st.toolDefs,
		MaxTokens:         clampMaxTokens(s.modelKey(st), spec.MaxTokens),
		Temperature:       spec.Temperature,
		CacheSystem:       true,
		PromptCacheKey:    st.routeKey,
		ResponseSchema:    spec.ResponseSchema,
		Thinking:          resolveThinking(spec, st.rp),
		Effort:            spec.Effort,
		StablePrefixBytes: stableBytes,
		StablePrefixMsgs:  stableMsgs,
		TotalRequestBytes: len(spec.System) + st.toolBytes + st.tr.TotalBytes(),
	}
}

// modelKey names the provider row and model this segment sends to.
func (s *session) modelKey(st *segState) llm.ModelKey {
	name := ""
	if st.rp.Provider != nil {
		name = st.rp.Provider.Name()
	}
	return llm.ModelKey{Provider: name, Model: st.rp.Model}
}

// callModel issues one completion through the caller (replay cache, budget,
// limiter, retry policy) and records the request for Accept eviction.
func (s *session) callModel(ctx context.Context, spec *SessionSpec, st *segState, req llm.CompletionRequest) (*llm.CompletionResponse, bool, error) {
	toolPrefixChanged := st.toolsGen != st.sentToolsGen
	st.sentToolsGen = st.toolsGen
	if toolPrefixChanged {
		s.logger.Warn("tool block changed mid-segment; the cached prefix re-bills at full price",
			"stage", spec.Stage, "iteration", st.iteration, "tools", len(st.toolDefs), "tool_bytes", st.toolBytes)
	}

	s.events.thinking(true)
	params := callParams{
		rp:                st.rp,
		req:               req,
		policy:            spec.Policy,
		timeout:           spec.Timeout,
		fallback:          s.resolver.fallbackHook(),
		stage:             spec.Stage,
		runID:             s.runID,
		sessionID:         st.sessionID,
		iteration:         st.iteration,
		noReplayCache:     spec.NoReplayCache,
		served:            st.served,
		toolBytes:         st.toolBytes,
		toolPrefixChanged: toolPrefixChanged,
	}
	resp, cacheKey, cacheHit, err := s.caller.call(ctx, params)
	// A model that caps its reply below what was asked rejects the request outright.
	// Learn the real cap from the rejection and retry once at it, so the limit costs
	// one failed call per model instead of pausing the build.
	if limit := learnOutputCap(err, req.MaxTokens); limit > 0 {
		s.logger.Warn("provider rejected max_tokens; retrying at its cap",
			"stage", spec.Stage, "requested", req.MaxTokens, "cap", limit)
		rememberOutputCap(s.modelKey(st), limit)
		params.req.MaxTokens = limit
		resp, cacheKey, cacheHit, err = s.caller.call(ctx, params)
	}
	s.events.thinking(false)
	// Recorded before the error check: a response that arrived and was cached
	// is discardable whatever the iteration goes on to do with it.
	if cacheKey != "" {
		s.requestHashes = append(s.requestHashes, cacheKey)
	}
	return resp, cacheHit, err
}

// accountUsage folds one call's usage into this segment's result and the
// session total, and updates the context size the rollover trigger keys on.
func (st *segState) accountUsage(res, total *SessionResult, resp *llm.CompletionResponse, cacheHit bool, requestBytes int) {
	if !cacheHit {
		for _, r := range []*SessionResult{res, total} {
			r.InputTokens += resp.Usage.InputTokens
			r.OutputTokens += resp.Usage.OutputTokens
			r.CacheReadTokens += resp.Usage.CacheReadInputTokens
			r.CacheWriteTokens += resp.Usage.CacheCreationInputTokens
		}
		// Total prompt = uncached + cache-read + cache-creation (see lastContextTokens):
		// tracks real context growth, invariant to a provider cache eviction.
		st.lastContextTokens = resp.Usage.InputTokens +
			resp.Usage.CacheReadInputTokens +
			resp.Usage.CacheCreationInputTokens
	} else {
		st.lastContextTokens = contextTokensAfterCacheHit(st.lastContextTokens, requestBytes)
	}
	res.Iterations++
	total.Iterations++
}

// addEditCounters folds this segment's edit-shape counters into the session total.
func (st *segState) addEditCounters(total *SessionResult) {
	total.Saves += st.saves
	total.Patches += st.patches
	total.AnchorMisses += st.anchorMisses
	total.NoOps += st.noops
}

// runToolTurn executes a turn's tool calls and its end-of-turn bookkeeping,
// returning the ending the segment should stop on, or nil to keep going.
func (s *session) runToolTurn(ctx context.Context, spec *SessionSpec, st *segState, resp *llm.CompletionResponse, assistantMsg llm.Message, res, total *SessionResult) *ending {
	// Real tool calls landed, so this turn is progress. Both counters below are
	// consecutive-only streaks: a cut or empty turn is evidence of a stuck model ONLY
	// while nothing productive happens between. Clearing them also closes the content
	// join armed by an earlier cut, so the next tool-call-free turn replaces res.Content.
	st.continuation = 0
	st.emptyStreak = 0

	st.tr.Append(assistantMsg)
	patchFailsBefore := st.anchorMisses + st.noops
	s.executeToolCalls(ctx, spec, st, resp.ToolCalls)
	res.ToolCalls += len(resp.ToolCalls)
	total.ToolCalls += len(resp.ToolCalls)
	writeCalls := s.countWriteCalls(resp.ToolCalls)
	if writeCalls > 0 {
		res.Writes += writeCalls
		total.Writes += writeCalls
	}

	// A turn spent only on a drifted or no-op patch (every write rejected, and
	// the failures were anchor misses or byte-identical replaces) buys a bounded
	// extra iteration so a couple of retries do not starve a short LIVE budget.
	// A turn that also landed a successful write is real progress and grants none.
	if st.patchRetryGrants < maxPatchRetryGrants &&
		(st.anchorMisses+st.noops) > patchFailsBefore &&
		st.writeRejects > 0 && writeCalls == st.writeRejects {
		st.patchRetryGrants++
	}

	s.updateReadOnlyStreak(spec, st, resp.ToolCalls)

	// A stop that arrived while this turn ran: the tool calls it had already
	// committed to have finished, so ending now is a clean boundary. Recorded
	// as a stop rather than returned directly so it merges with any ending the
	// turn itself decided on, under the same first-reason-wins rule.
	if s.ownerStopRequested() {
		s.logger.Info("owner stop: ending segment after turn", "stage", spec.Stage, "iteration", st.iteration)
		st.stopWith(endOwnerStop)
	}

	if spec.OnTurnEnd != nil && st.stop == nil {
		// inject and stop are independent signals (see spec.OnTurnEnd): a hook
		// may inject without stopping, stop without injecting (a silent stop),
		// or both. Honor stop even when inject is empty.
		if inject, done := spec.OnTurnEnd(st.iteration, resp.ToolCalls, st.writeRejects); inject != "" || done != nil {
			if inject != "" {
				st.tr.Append(llm.Message{Role: llm.RoleUser, Content: inject})
			}
			if done != nil {
				st.stopWith(*done)
			}
		}
	}

	if st.stop != nil {
		s.logger.Info("segment stopping", "reason", st.stop.detail, "outcome", st.stop.outcome, "iteration", st.iteration)
		return st.stop
	}
	return nil
}

// contextTokensAfterCacheHit keeps the rollover trigger alive across a
// replay-cache hit, which reports no usage while the transcript kept growing.
// The estimate is deliberately crude (bytes/4) and applied as a RATCHET: a hit
// can arm the trigger but never lower a figure a real call already reported, so
// an under-estimate only ever delays a rollover. Pure, so unit-testable.
func contextTokensAfterCacheHit(prev, requestBytes int) int {
	if est := requestBytes / bytesPerTokenEstimate; est > prev {
		return est
	}
	return prev
}

// toolDefsBytes returns the serialized size of the tool schemas for the
// total_request_bytes diagnostic. The tool block rides the cached prefix, so
// its size is part of what every warm call re-reads.
func toolDefsBytes(defs []llm.ToolDef) int {
	if len(defs) == 0 {
		return 0
	}
	b, err := json.Marshal(defs)
	if err != nil {
		return 0
	}
	return len(b)
}

// resolveTools builds the segment's initial tool set: the spec's preloads, sorted and
// deduped so the tool ORDER is deterministic (the prompt cache keys on those bytes), plus
// the find_tool shim. Empty PreloadTools with a ResponseSchema (plan) means no tools.
func (s *session) resolveTools(spec *SessionSpec) []llm.ToolDef {
	if len(spec.PreloadTools) == 0 {
		return nil
	}
	caps, err := llm.LoadCapabilities(s.deps.DB.DB)
	if err != nil {
		s.logger.Warn("load capabilities failed; capability-gated tools hidden", "err", err)
	}
	names := append([]string(nil), spec.PreloadTools...)
	sort.Strings(names)
	dedup := names[:0]
	for i, n := range names {
		if n == "" || (i > 0 && n == names[i-1]) {
			continue
		}
		dedup = append(dedup, n)
	}
	defs := s.deps.ToolRegistry.LookupFull(dedup, caps, spec.Stage)
	defs = append(defs, tools.FindToolDef())
	return defs
}

// modelLacksTools reports whether the resolved model is recorded as unable to call
// tools. Re-prompting such a model cannot help, so the session ends instead.
func (s *session) modelLacksTools(st *segState) bool {
	if st.rp.Provider == nil {
		return false
	}
	k := llm.ModelKey{Provider: st.rp.Provider.Name(), Model: st.rp.Model}
	return llm.Caps.Supports(k, llm.CapTools) == llm.No
}

// handleNoToolCalls decides whether a tool-call-free turn means continuation, re-prompt,
// or an end to the segment. Returns the end reason, or "" to keep going, distinguishing a
// clean finish from the two give-up caps (callers surface those differently to the owner).
func (s *session) handleNoToolCalls(spec *SessionSpec, st *segState, resp *llm.CompletionResponse, assistantMsg llm.Message) *ending {
	// Reset before any branch: the streak counts CONSECUTIVE unusable turns, and a turn
	// that produced text is usable however it stopped.
	if resp.Content != "" {
		st.emptyStreak = 0
	}
	// An empty turn is one with no text and no tool call. Every one counts toward the
	// streak: a deliberate stop emits text, so silence is never evidence the model is
	// finished. The output-token floor only picks the continuation wording below.
	empty := resp.Content == "" && len(resp.ToolCalls) == 0
	if resp.StopReason == "max_tokens" || empty {
		if resp.Content == "" {
			st.emptyStreak++
			s.logger.Warn("llm produced an empty response",
				"stage", spec.Stage, "iteration", st.iteration,
				"stop_reason", resp.StopReason, "output_tokens", resp.Usage.OutputTokens,
				"streak", st.emptyStreak)
			if st.emptyStreak >= maxEmptyResponseStreak {
				s.logger.Warn("llm wedged: empty-response streak cap hit, ending segment")
				if !spec.SuppressProgress {
					s.events.progress("I got a bit stuck on that step and couldn't quite finish it. Tell me here to try again, or tweak what you're after and I'll have another go.")
				}
				return &endWedged
			}
		}
		var contMsg string
		// A toolless stage answers in JSON (PLAN, the audit) except EXPAND, which answers
		// in prose: a JSON demand there would be persisted as the brief.
		jsonStage := spec.Stage != StageExpand
		switch {
		case len(st.toolDefs) == 0 && jsonStage && st.runawayTrimmed && resp.Content == "":
			contMsg = compactJSONRestart
		case len(st.toolDefs) == 0 && jsonStage && resp.StopReason == "max_tokens" && st.runawayTrimmed:
			contMsg = compactJSONContinuation
		case len(st.toolDefs) == 0 && jsonStage && resp.StopReason == "max_tokens":
			contMsg = "Your JSON response was cut off mid-output. Continue EXACTLY where you left off, output ONLY the remaining JSON to complete the object. No preamble, no explanation."
		case len(st.toolDefs) == 0 && jsonStage:
			// Empty turn, not a cut: nothing was emitted, so ask for the whole object.
			contMsg = "Your last turn produced no output. Emit the FULL JSON object now. No preamble, no explanation."
		case len(st.toolDefs) == 0 && resp.StopReason == "max_tokens":
			contMsg = "Your response was cut off mid-output. Continue EXACTLY where you left off and finish it. No preamble, no explanation."
		case len(st.toolDefs) == 0:
			contMsg = "Your last turn produced no output. Write your full answer now."
		case resp.StopReason == "max_tokens":
			contMsg = "Your response was cut off. Continue from where you left off, call your next tool."
		default:
			contMsg = "Your last turn produced no usable output - no text, no tool call - so nothing was received. Call the next tool you need, with valid JSON arguments, not prose."
		}
		st.continuation++
		if st.continuation >= maxContinuations {
			s.logger.Warn("max continuations reached", "continuations", st.continuation)
			return &endContinuationCap
		}
		st.tr.Append(assistantMsg, llm.Message{Role: llm.RoleUser, Content: contMsg})
		return nil
	}

	// Iteration-0 punt with callable tools registered: re-prompt once. Not in LIVE: an
	// owner question ("what do you suggest?") is answered in chat with no tool at all, and
	// the live frame forbids editing until the owner picks, so a text-only first turn there
	// is a finished answer, not a punt.
	if st.iteration == 0 && len(st.toolDefs) > 0 && spec.Stage != StageLive {
		if s.modelLacksTools(st) {
			s.logger.Error("model declares no tool support and made no tool call; stopping", "model", st.rp.Model)
			return &endModelNoTools
		}
		s.logger.Warn("LLM punted on iteration 0, re-prompting", "stop_reason", resp.StopReason)
		st.tr.Append(assistantMsg, llm.Message{
			Role:    llm.RoleUser,
			Content: "You did not call any tool. The instruction REQUIRES a tool call to make progress. Re-read the instruction above and make your first call now.",
		})
		return nil
	}
	// The model stopped calling tools of its own accord. Give the stage one chance to
	// say the work is not actually finished (BUILD checks the checklist here): this is
	// the only turn shape OnTurnEnd cannot see.
	if spec.OnModelDone != nil {
		if inject := spec.OnModelDone(); inject != "" {
			s.logger.Info("model stopped early, stage asked it to continue", "stage", spec.Stage, "iteration", st.iteration)
			st.tr.Append(assistantMsg, llm.Message{Role: llm.RoleUser, Content: inject})
			return nil
		}
	}
	return &endModelDone
}

// handleTruncatedToolCalls drops the trailing tool call when its arguments
// don't parse AND the turn hit the output cap. Returns (handled, capped).
func (s *session) handleTruncatedToolCalls(spec *SessionSpec, st *segState, resp *llm.CompletionResponse, assistantMsg *llm.Message) (bool, bool) {
	if len(resp.ToolCalls) == 0 {
		return false, false
	}
	lastTC := resp.ToolCalls[len(resp.ToolCalls)-1]
	var testArgs map[string]any
	if err := json.Unmarshal([]byte(lastTC.Arguments), &testArgs); err == nil {
		return false, false
	}
	atCap := resp.StopReason == "max_tokens" ||
		(spec.MaxTokens > 0 && resp.Usage.OutputTokens >= spec.MaxTokens)
	if !atCap {
		return false, false
	}
	s.logger.Warn("discarding truncated tool call", "tool", lastTC.Name, "stop_reason", resp.StopReason)
	resp.ToolCalls = resp.ToolCalls[:len(resp.ToolCalls)-1]
	assistantMsg.ToolCalls = resp.ToolCalls
	if len(resp.ToolCalls) == 0 {
		st.continuation++
		if st.continuation >= maxContinuations {
			return true, true
		}
		st.tr.Append(*assistantMsg, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf(
			"Your response was truncated while calling %s - the payload is too large for one call. Continue from where you left off: write a smaller initial body, then extend it with action=patch in follow-up calls instead of one giant save.", lastTC.Name)})
		return true, false
	}
	return false, false
}

// countWriteCalls returns how many of calls are NOT read-only: actual mutations, as opposed
// to reads/probes. Mirrors updateReadOnlyStreak's classification; feeds SessionResult.Writes
// so the scheduler can tell a no-op heartbeat run from one that changed something.
func (s *session) countWriteCalls(calls []llm.ToolCall) int {
	n := 0
	for _, tc := range calls {
		var args map[string]any
		_ = json.Unmarshal([]byte(tc.Arguments), &args)
		if !s.isReadOnlyToolCall(tc, args) {
			n++
		}
	}
	return n
}

// updateReadOnlyStreak counts consecutive all-read-only turns, then nudges and
// force-stops at the spec's thresholds (zero disables).
func (s *session) updateReadOnlyStreak(spec *SessionSpec, st *segState, calls []llm.ToolCall) {
	if len(calls) == 0 || spec.ReadOnlyForceStop == 0 {
		return
	}
	// The streak counts only turns of purely passive source reads. A write OR an
	// active project probe (manage_testing / manage_diagnostics) is forward
	// progress, not a stall - a stuck probe is caught by the identical-result
	// guard instead - so a probe resets the streak.
	passiveRead := true
	for _, tc := range calls {
		var args map[string]any
		_ = json.Unmarshal([]byte(tc.Arguments), &args)
		if !s.isReadOnlyToolCall(tc, args) || isReadOnlyProbe(tc.Name) {
			passiveRead = false
			break
		}
	}
	if !passiveRead {
		st.readOnlyRun = 0
		return
	}
	st.readOnlyRun++
	switch {
	case st.readOnlyRun >= spec.ReadOnlyForceStop:
		// No message is appended: the stop ends the segment before any further
		// request, so appended text would never reach the model.
		s.logger.Warn("read-only streak force-stop", "streak", st.readOnlyRun)
		st.stopWith(endReadOnlyStreak)
	case spec.ReadOnlyNudge > 0 && st.readOnlyRun == spec.ReadOnlyNudge:
		s.logger.Warn("read-only streak nudge", "streak", st.readOnlyRun)
		st.tr.Append(llm.Message{Role: llm.RoleUser, Content: "You've spent several iterations only reading, with no edit. Take ONE concrete action now: make the change with patch (a unique search anchor), or if the task's deliverables already exist and are wired, call complete."})
	}
}

// routeKeyHeadBytes is how much of the system prompt the routing hint keys on. The BUILD
// prompt ends with a Deferred capabilities block listing what the plan did NOT preload, so
// two projects with different plan tool sets differ in their last few KB while sharing
// everything above: projects typically share most of the prompt and diverge exactly at
// that heading. Hashing the WHOLE prompt therefore gives almost every project its own
// shard and shares nothing, which is the same fragmentation a per-project key produced.
// Keying on the head groups them instead. This is only ever a hint about where to look for
// a warm prefix: prefix caching still serves nothing but genuinely matching bytes, so two
// prompts sharing a shard can never be served each other's tails.
const routeKeyHeadBytes = 32768

// cacheRouteKey is the provider cache-NODE routing hint (OpenAI prompt_cache_key). It is
// keyed on the stage plus a digest of the head of the system prompt, so every request whose
// cached head is byte-identical routes to the same warm node and warm-hits there instead of
// cold-starting on a shard of its own. Scoping it per project would make each one pay its
// own cold read of a head that is ~90% shared. Digesting the prompt text means a corpus
// edit or a Wave bump rotates the key on its own, while a differing tail does not.
func cacheRouteKey(stage, system string) string {
	head := system
	if len(head) > routeKeyHeadBytes {
		head = head[:routeKeyHeadBytes]
	}
	sum := sha256.Sum256([]byte(head))
	return fmt.Sprintf("%s:%x", stage, sum[:4])
}

// recordSegmentStart / recordSegmentEnd persist of_sessions metadata.
func (s *session) recordSegmentStart(sessionID string, segment int) {
	if _, err := sqliteretry.Exec(s.pdb.Writer(),
		`INSERT INTO of_sessions (id, run_id, segment) VALUES (?, ?, ?)`,
		sessionID, s.runID, segment); err != nil {
		s.logger.Debug("session start record failed", "error", err)
	}
}

func (s *session) recordSegmentEnd(sessionID, reason string, iterations int) {
	if _, err := sqliteretry.Exec(s.pdb.Writer(),
		`UPDATE of_sessions SET end_reason = ?, iterations = ?, ended_at = CURRENT_TIMESTAMP WHERE id = ?`,
		reason, iterations, sessionID); err != nil {
		s.logger.Debug("session end record failed", "error", err)
	}
}
