package engine

// SessionSpec is the single parameterization of the unified session loop:
// every stage (plan, build, live) and every future stage runs through
// session.Run with one of these. Stage tuning defaults live in specDefaults.

import (
	"time"

	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// SnapshotMode selects how the session keeps the model's project_state view
// current. Every mode is append-only - history is never rewritten.
type SnapshotMode int

const (
	// SnapshotNone: no project_state injection (plan - state is in the seed).
	SnapshotNone SnapshotMode = iota
	// SnapshotBaselineDelta: full baseline once at session start, then small
	// delta messages appended as the project changes (build tasks).
	SnapshotBaselineDelta
	// SnapshotFull: a full snapshot appended whenever the project changed
	// since the last one, superseding earlier snapshots in place (live chat).
	SnapshotFull
)

// SessionSpec parameterizes one session.Run call.
type SessionSpec struct {
	// Stage keys prompt/tool gating and log labels: plan | build | live.
	Stage string

	// System is the fully assembled system prompt. It MUST be byte-stable for
	// the whole session: no dates, names, or randomness (owner context goes in
	// a seed user message instead).
	System string

	// Seed is the first segment's message seed, built deterministically from
	// DB state.
	Seed []llm.Message

	// Reseed rebuilds the seed for a fresh segment after a context rollover. Must be a pure
	// function of DB state so a rollover (or crash resume) reproduces the same bytes. Nil
	// disables rollover: the session force-stops on overflow instead.
	Reseed func() ([]llm.Message, error)

	// Accept, when set, is this session's usability contract on the final content: return an
	// error if the response is structurally unusable (does not parse). Run applies it once and
	// evicts those responses from the replay cache. Structurally unusable only: a response
	// that parses but is wrong is a valid answer and stays cached.
	Accept func(content string) error

	// PreloadTools are tool names whose schemas load up front. find_tool is
	// always available when this is non-empty, so the model can discover and
	// load anything else.
	PreloadTools []string

	MaxIterations int
	MaxTokens     int
	Temperature   *float64
	// Thinking is the reasoning-mode toggle sent with every call (nil sends none).
	// When ThinkingStage is set, it is that stage's default and the provider row may
	// override it (providercfg Config.Thinking, see resolveThinking); without a
	// ThinkingStage it is fixed, as for the small utility calls.
	Thinking      *bool
	ThinkingStage string
	// Effort tunes how much the model deliberates per turn ("low".."max"). Empty =
	// use the provider default. Set per stage in DefaultSpec.
	//
	// Only the anthropic transport reads it today (output_config.effort); the
	// openai-chat client ignores it and takes its reasoning cue from Thinking
	// instead. So on an openai-chat model these values are inert, and tuning
	// per-turn latency there means Thinking, not this.
	Effort string
	// Timeout is the per-LLM-call deadline.
	Timeout time.Duration
	// ResponseSchema constrains every completion to a JSON schema (plan).
	ResponseSchema *llm.ResponseSchema

	Snapshot SnapshotMode

	// Guard, when non-nil, runs before (screen) and after (observe) every
	// tool call. nil = no screening.
	Guard *RepeatGuard

	// OnTurnEnd, when non-nil, runs after each iteration's tool execution.
	// A non-empty inject is appended as a user message; a non-nil stop ends
	// the session after this iteration with that ending. The two signals are
	// independent: inject without stopping, stop silently, or both.
	// writeRejects is how many of this turn's mutations were REJECTED, so a hook can
	// refuse to stop on a half-applied turn.
	OnTurnEnd func(iteration int, calls []llm.ToolCall, writeRejects int) (inject string, stop *ending)

	// OnModelDone, when non-nil, runs when the model stops calling tools of its own
	// accord. A non-empty return is appended as a user message and the segment keeps
	// going; "" lets it end. OnTurnEnd never sees this turn (it has no tool calls), so
	// without this a model that narrates "all done" ends the session whatever the
	// checklist says. A hook that can fire more than once must latch itself.
	OnModelDone func() string

	// SeedGuides names the guide slugs whose cores the seed inlines, threaded
	// into every ToolContext so manage_guides recall can skip re-serving them.
	// Nil outside BUILD.
	SeedGuides map[string]bool

	// NoReplayCache disables of_llm_cache reads (live chat - owners expect a
	// fresh response, not a replay).
	NoReplayCache bool

	// SuppressProgress silences templated per-iteration progress messages.
	SuppressProgress bool

	// ReadOnlyNudge / ReadOnlyForceStop tune the read-only-streak guard;
	// zero disables.
	ReadOnlyNudge     int
	ReadOnlyForceStop int

	// RolloverInputTokens ends the segment and reseeds once a call's input
	// grows past this. Zero applies defaultRolloverInputTokens (only when
	// Reseed is set).
	RolloverInputTokens int

	// SealSeed marks the first segment's seed as already provider-seen: a WARM continuation
	// carries a prior session's transcript as its seed, and sealing it up front makes
	// stable_prefix telemetry report the carried prefix instead of 0. (Provider caching is
	// content-addressed and needs no seal; this is measurement, not behavior.) Reseed
	// segments are always cold and never sealed early.
	SealSeed bool

	// CarryReads, when set, is the prior session's ReadTracker adopted for the FIRST segment
	// (the warm-continuation pair of a carried transcript): the carried transcript still holds
	// the bodies it recorded, so re-reads keep stubbing. A rollover reseeds cold and gets a
	// fresh tracker.
	CarryReads *tools.ReadTracker

	// Policy is the retry budget; zero value falls back to the stage default.
	Policy StagePolicy
}

// SessionResult is what one session.Run returns.
type SessionResult struct {
	Content          string
	Model            string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ToolCalls        int
	// Writes counts non-read-only tool calls (an actual mutation, not a
	// read/probe). A scheduled run with zero Writes changed nothing observable,
	// the signal the no-op backoff uses to stop an idle heartbeat.
	Writes     int
	Iterations int
	Segments   int
	// Edit-shape counters (manage_files) summed across segments: full-body saves
	// vs patches, patch-anchor misses, and no-op patches. NoOps is tracked apart
	// from AnchorMisses so an authoring slip is not read as anchor drift.
	Saves, Patches, AnchorMisses, NoOps int
	// Paused is true when a tool requested owner input (the run should pause).
	Paused bool
	// Messages is the final segment's transcript (live chat carries it over).
	Messages []llm.Message
	// Ending is how the final segment stopped, in terms a caller can branch on.
	// It does NOT say whether the work got done; the checklist says that.
	Ending Ending
	// EndDetail names the mechanism behind Ending, for of_sessions and a
	// post-mortem. Two mechanisms can share an outcome, and only this tells them
	// apart, so it is recorded rather than reconstructed.
	EndDetail string
	// LastContextTokens is the final call's TOTAL prompt size (uncached + cache-read +
	// cache-creation), the same signal the rollover trigger keys on. A caller weighing a
	// warm continuation reads it to know how much headroom the carried transcript leaves.
	LastContextTokens int
	// Reads is the final segment's ReadTracker, paired with Messages: a warm
	// continuation passes it back as CarryReads so bodies resident in the
	// carried transcript keep dedup-stubbing instead of re-serving.
	Reads *tools.ReadTracker
	// Tools is the session's final tool set: the spec's preloads plus every find_tool
	// load. A warm continuation preloads it, so the tools its carried transcript
	// called are still callable.
	Tools []string
}

// Ending is HOW the loop stopped, in the four terms a caller can act on. Every terminator
// maps onto exactly one, so a caller branches on this rather than the mechanism that fired.
// Whether the WORK is done is a SEPARATE question this cannot answer (a model can stop with
// the job half finished; the task checklist answers that).
type Ending string

const (
	// EndDone: the model stopped on its own terms. It stopped calling tools, or a
	// turn-end hook said the work was complete.
	EndDone Ending = "done"
	// EndExhausted: a budget ran out (iterations, segments, a read-only streak,
	// a deadline). There was more to do and permission to do it ran out.
	EndExhausted Ending = "exhausted"
	// EndPaused: something asked the loop to stop and wait (a tool, an approval).
	EndPaused Ending = "paused"
	// EndFailed: the loop could not make progress. Distinct from EndExhausted,
	// and the distinction is the useful one: a budget is a permission to spend,
	// while these are symptoms of a loop that spending more would not help.
	EndFailed Ending = "failed"
)

// ending pairs the branchable outcome with the mechanism that produced it. Two fields for
// two audiences: outcome is for callers deciding what to do next; detail is for of_sessions
// and a post-mortem, the only thing that tells two mechanisms with the same outcome apart.
type ending struct {
	outcome Ending
	detail  string
}

// The terminators, each naming its own outcome at the point of decision rather
// than leaving a caller to infer one from a shared bool.
var (
	endModelDone         = ending{EndDone, "done"}
	endChecklistComplete = ending{EndDone, "checklist_complete"}

	endIterationCap      = ending{EndExhausted, "iteration_cap"}
	endReadOnlyStreak    = ending{EndExhausted, "read_only_streak"}
	endSegmentsExhausted = ending{EndExhausted, "segments_exhausted"}

	endToolPause = ending{EndPaused, "tool_pause"}
	endApproval  = ending{EndPaused, "approval"}
	endOwnerStop = ending{EndPaused, "owner_stop"}

	endWedged          = ending{EndFailed, "wedged"}
	endContinuationCap = ending{EndFailed, "continuation_cap"}
	endRepeatStall     = ending{EndFailed, "repeat_stall"}
	endTransportError  = ending{EndFailed, "error"}
	endModelNoTools    = ending{EndFailed, "model_lacks_tools"}
)

// endRolloverDetail is written to of_sessions when a segment rolls over. It is
// not an ending: the session continues in a fresh segment, so it never reaches a
// caller and has no outcome to report.
const endRolloverDetail = "rollover"

// Loop tuning shared by every stage.
const (
	// maxContinuations caps CONSECUTIVE "continue" re-prompts after max_tokens cuts. A turn
	// that lands real tool calls clears the count: with BUILD's output cap sized so one turn
	// can carry a whole file body, cuts are routine across a long build, not a stuck model.
	maxContinuations = 5
	// maxEmptyResponseStreak caps consecutive turns that produced neither
	// content nor a tool call while spending output tokens (wedged provider).
	maxEmptyResponseStreak = 2
	// defaultRolloverInputTokens triggers a segment rollover when a call's TOTAL context
	// (uncached + cache-read + cache-creation, see session.go's lastContextTokens) exceeds it
	// and the spec allows reseeding. Against total, not the uncached portion, so a cache
	// eviction cannot spuriously trip it.
	defaultRolloverInputTokens = 200_000
	// minUsableWindow is the smallest context window the rollover will size itself
	// from. Below it the build prompt alone does not fit, so a lower trigger would only
	// roll over on every call; the default stays and the model's own limit applies.
	minUsableWindow = 16_000
	// maxSegments bounds rollovers per session so a pathological task cannot
	// reseed forever.
	maxSegments = 8
	// rebaselineAfterDeltas is how many project_state change updates may pile up behind a
	// baseline before a full snapshot is re-issued instead of another delta. The
	// delta chain is only replayable while short, so this stays small enough for the model to
	// replay in its head.
	rebaselineAfterDeltas = 6
	// bytesPerTokenEstimate converts request bytes to tokens for the ONE place no provider
	// usage figure exists: a replay-cache hit, which reports no usage. Deliberately crude and
	// applied as a ratchet (see session.go), so under-counting only delays a trigger.
	bytesPerTokenEstimate = 4
	// maxPatchRetryGrants bounds the extra iterations a segment may earn from turns
	// spent entirely on a drifted or no-op patch (see session.go), so a couple of
	// retries do not starve a short budget while a stuck model still cannot loop forever.
	maxPatchRetryGrants = 6
)

// thinkingOff is every stage's reasoning default. Reasoning is opt-in per model
// (llm_models.config, see resolveThinking): on GLM, which takes no thinking budget,
// it turned a 23 s EXPAND into 134 s and a 1-minute PLAN into 7+ minutes, so a model
// thinks only where its admin turned that stage on, and the eval harness says whether
// it paid off. Providers without a toggle ignore the value.
var thinkingOff = false

// Per-stage effort (how hard the model deliberates per turn). EXPAND runs "high": one
// call whose latency is irrelevant and whose output shapes the whole build. The rest
// run "medium", which often matches high in quality at far lower latency. Named
// constants so a stage can be swept and measured.
//
// Live on the anthropic transport only. Every other provider rides openai-chat, which
// does not send an effort field, so these are inert there and a sweep would measure
// nothing. Wire it through that client before treating these as a tuning knob.
const (
	effortExpand = "high"
	effortPlan   = "medium"
	effortBuild  = "medium"
	effortLive   = "medium"
)

// DefaultSpec returns the tuning skeleton for a stage. Callers fill in
// System/Seed/tools/guards.
func DefaultSpec(stage string) SessionSpec {
	switch stage {
	case StageExpand:
		return SessionSpec{
			Stage:         StageExpand,
			MaxIterations: 1,
			// 16384 leaves ample room for the up-to-1000-word brief (~1500
			// tokens); EXPAND runs once per project and bills only emitted tokens.
			MaxTokens: 16384,
			// Widest sampling in the pipeline: the visual direction is invented
			// here, and one converged sample locks the whole build, so distinct
			// briefs must diverge instead of collapsing to one default look.
			Temperature:   f64(0.9),
			Timeout:       5 * time.Minute,
			Thinking:      &thinkingOff,
			ThinkingStage: StageExpand,
			Effort:        effortExpand,
			Policy:        policyPlan,
		}
	case StagePlan:
		return SessionSpec{
			Stage:         StagePlan,
			MaxIterations: 8,
			MaxTokens:     32768,
			// Mildly wide so the plan's design and structure choices vary
			// across projects rather than converging on one shape.
			Temperature:   f64(0.5),
			Timeout:       30 * time.Minute,
			Thinking:      &thinkingOff,
			ThinkingStage: StagePlan,
			Effort:        effortPlan,
			Policy:        policyPlan,
		}
	case StageBuild:
		return SessionSpec{
			Stage:         StageBuild,
			MaxIterations: 200,
			// 32768: lets one turn carry a complete file body (or several small ones) plus
			// its checklist-complete call; half this cap forced multi-turn write chains. With
			// one continuous build session, 200 iterations per segment plus rollover
			// cover even large projects.
			MaxTokens: 32768,
			// Above deterministic so the realized UI is expressive rather than
			// templated; the save gates and fix-loop catch the added variance.
			Temperature:       f64(0.4),
			Timeout:           90 * time.Minute,
			Snapshot:          SnapshotBaselineDelta,
			Thinking:          &thinkingOff,
			ThinkingStage:     StageBuild,
			Effort:            effortBuild,
			ReadOnlyNudge:     6,
			ReadOnlyForceStop: 16,
			Policy:            policyBuild,
		}
	case StageLive:
		return SessionSpec{
			Stage: StageLive,
			// Headroom for locate-before-read: a live fix front-loads discovery
			// (manage_codebase resolve/where_used/grep) before the read+patch+test, which at
			// 10 could exhaust the budget mid-fix and exit before the closing summary.
			// Kept generous so a retried patch (anchor miss / no-op) does not starve
			// the pass before it finishes.
			// Snapshot is baseline then deltas, as in BUILD: a live fix edits several
			// times, and a full re-send per edit repeats the whole project state for a
			// few changed rows.
			MaxIterations:    28,
			MaxTokens:        16384,
			Temperature:      f64(0.2),
			Timeout:          15 * time.Minute,
			Snapshot:         SnapshotBaselineDelta,
			Thinking:         &thinkingOff,
			ThinkingStage:    StageLive,
			Effort:           effortLive,
			NoReplayCache:    true,
			SuppressProgress: true,
			Policy:           policyLive,
		}
	default:
		return SessionSpec{Stage: stage, MaxIterations: 5, MaxTokens: 8192, Thinking: &thinkingOff, Policy: policyBuild}
	}
}

func f64(v float64) *float64 { return &v }

// rolloverForWindow lowers the rollover trigger to fit a context window the
// provider is known to have, keeping a share of it free for the model's reply and
// the next tool results. The default applies when the window is unknown or too
// small to size from.
func rolloverForWindow(trigger, window int) int {
	if window < minUsableWindow {
		return trigger
	}
	if limit := window * 7 / 10; limit < trigger {
		return limit
	}
	return trigger
}

// resolveThinking is the toggle a call sends: the resolved model's setting for the
// spec's stage when it names one, else the stage default. A spec without a
// ThinkingStage (a utility call) is fixed.
func resolveThinking(spec *SessionSpec, rp resolvedProvider) *bool {
	if spec.ThinkingStage == "" {
		return spec.Thinking
	}
	if on, set := rp.Thinking[spec.ThinkingStage]; set {
		return &on
	}
	return spec.Thinking
}

// StageThinkingDefaults reports each stage's reasoning default, keyed by the stage
// names a model's thinking map uses, so the admin UI shows what an unset
// stage does without keeping its own copy of these defaults.
func StageThinkingDefaults() map[string]bool {
	out := map[string]bool{StageReview: reviewThinkingDefault}
	for _, st := range []string{StageExpand, StagePlan, StageBuild, StageLive} {
		if t := DefaultSpec(st).Thinking; t != nil {
			out[st] = *t
		}
	}
	return out
}
