package engine

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/internal/buildstate"
	"openfabrica/providers/llm"
	"openfabrica/security"
	"openfabrica/tools"
)

// ErrPaused is returned through the session loop when a tool requests a
// pipeline pause (e.g. manage_chat asking the owner for a secret mid-build).
var ErrPaused = errors.New("pipeline paused: awaiting owner input")

// State is the externally visible state of a project worker.
type State string

const (
	StateIdle     State = "idle"
	StateBuilding State = "building"
	StateLive     State = "live"
	StatePaused   State = "paused"
	StateError    State = "error"
)

// Stage names are lowercase registry keys (see stage.go). They are stored in
// of_runs.stage and used as prompt/tuning lookup keys.
const (
	StageExpand = "expand"
	StagePlan   = "plan"
	StageBuild  = "build"
	StageLive   = "live"
)

// Pause reasons stored in of_runs.pause_reason. Manual owner pause is NOT a
// run pause: it lives on projects.mode (global DB) and suppresses wakes until
// the owner flips the mode back.
const (
	PauseReasonOwnerAnswers = buildstate.PauseAwaitingAnswers
	PauseReasonApproval     = buildstate.PauseAwaitingApproval
	// PauseReasonOwnerStop: the owner asked the pipeline to stop. Unlike the
	// other two nothing is being waited on, so it must not read as a pending
	// question: a resume needs no input, just the mode flipped back to building.
	PauseReasonOwnerStop = buildstate.PauseOwnerStop
)

// Question origins stored in of_questions.origin. The origin records which
// agent asked so the answer routes by intent, not by worker state at answer
// time: a plan clarification answered after go-live must not be mistaken for
// a live owner-reported issue.
const (
	QuestionOriginBuild = "build"
	QuestionOriginLive  = "live"
)

// Command is a message sent to a project worker.
type Command struct {
	Type      string         `json:"type"`
	ProjectID int            `json:"project_id"`
	Payload   map[string]any `json:"payload,omitempty"`
}

const (
	CommandWake          = "wake"
	CommandChat          = "chat"
	CommandModeChange    = "mode_change"
	CommandShutdown      = "shutdown"
	CommandScheduledTask = "scheduled_task"
	CommandUpdate        = "update"
)

// Deps holds shared dependencies injected into the engine and its workers.
type Deps struct {
	DB               *db.DB
	ProjectDBManager *db.ProjectDBManager
	Encryptor        *security.Encryptor
	JWTManager       *security.JWTManager
	LLMRegistry      *llm.Set
	ToolRegistry     *tools.Registry
	ToolExecutor     *tools.Executor
	// ToolEnv is the shared per-call tool dependency set (see tools.Env); the
	// composition root builds one and hands it to every tool caller. Nil falls
	// back to an env derived from the fields here (tests), see toolEnv.
	ToolEnv *tools.Env
	Bus     *events.Bus
	// LLMHTTPClient carries the per-call LLM deadline via its Timeout; the
	// engine applies no separate per-request deadline of its own.
	LLMHTTPClient *http.Client

	// MaxConcurrentPerModel caps concurrent LLM conversations per (provider,
	// model) across ALL projects - the guard against provider rate limits,
	// which are metered per model within an account. Zero falls back to
	// defaultPerModelConcurrency.
	MaxConcurrentPerModel int

	// DailyTokenBudgetFn returns the current cap on total engine token spend per
	// UTC day per project, read at call time (not captured once) so admin/setup
	// changes take effect without a restart. A nil func or a returned value <= 0
	// disables the cap.
	DailyTokenBudgetFn func() int

	// StageTimeouts overrides the per-stage wall-clock caps inside the drive
	// loop, keyed by stage name (StageExpand/StagePlan/StageBuild). A missing or
	// zero entry falls back to defaultStageTimeouts.
	StageTimeouts map[string]time.Duration

	PublicPort int

	// PublicCSP is the instance-default Content-Security-Policy for public HTML.
	// project_state reports the resolved policy so the build model knows what it is
	// building under; a per-project override wins over it (config.ResolveCSP).
	PublicCSP string
}

// toolEnv returns the tool env every engine tool call runs under: the injected
// ToolEnv, or one derived from these Deps when none was injected (tests build
// Deps field by field). The derived env carries the same embeddings wiring so a
// test and production shape identical ToolContexts.
func (d *Deps) toolEnv() *tools.Env {
	if d.ToolEnv != nil {
		return d.ToolEnv
	}
	env := &tools.Env{
		Bus:        d.Bus,
		Encryptor:  d.Encryptor,
		JWTManager: d.JWTManager,
		PublicPort: d.PublicPort,
	}
	if d.DB != nil {
		env.GlobalDB = d.DB.DB
		env.EmbedFunc = func(ctx context.Context, texts []string) ([][]float32, error) {
			return llm.EmbedTexts(ctx, d.DB.DB, d.Encryptor, d.LLMRegistry, texts)
		}
	}
	if d.ProjectDBManager != nil {
		env.DataDir = d.ProjectDBManager.DataDir()
	}
	return env
}

// payloadInt64 extracts an int64 from a command payload, tolerating int64,
// float64 (JSON numbers), int, and numeric strings. Returns 0 on miss.
func payloadInt64(payload map[string]any, key string) int64 {
	switch v := payload[key].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// Ambition levels (admin and bot import these). They gate the optional EXPAND
// stage and back the ambition project setting shown in the admin and bot UIs.

const (
	AmbitionOff    = "off"
	AmbitionLow    = "low"
	AmbitionMedium = "medium"
	AmbitionHigh   = "high"
)

// DefaultAmbition is applied when no level is set or an unrecognised value is
// stored or submitted. High: the expansive tier is the product experience the
// platform sells; leaner builds are the opt-in.
const DefaultAmbition = AmbitionHigh

// ValidAmbition reports whether s is one of the four recognised levels.
func ValidAmbition(s string) bool {
	switch s {
	case AmbitionOff, AmbitionLow, AmbitionMedium, AmbitionHigh:
		return true
	}
	return false
}

// NormalizeAmbition lower-cases and trims s, returning the default for any
// unrecognised value so a bad column or payload never breaks a build.
func NormalizeAmbition(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if ValidAmbition(s) {
		return s
	}
	return DefaultAmbition
}

// AmbitionTaskBudget returns the soft target and hard ceiling for the number of
// BUILD tasks a plan should emit at a given ambition level. `target` is advisory;
// `ceiling` is enforced (an over-budget plan is sent back once to trim, then
// ships regardless). AmbitionOff shares the lean low budget, NOT medium: "don't
// inflate my brief" must hold at plan time too, and it keeps the dial monotonic.
func AmbitionTaskBudget(level string) (target, ceiling int) {
	switch NormalizeAmbition(level) {
	case AmbitionOff, AmbitionLow:
		return 5, 8
	case AmbitionMedium:
		return 9, 12
	default: // high, and any value NormalizeAmbition folded to the default
		// The expansive default tier: fuller multi-screen or multi-subsystem builds.
		return 18, 24
	}
}

// AmbitionTaskFloor returns the task count below which an enforced high-ambition
// plan is considered undercovered. It does not trigger a retry (an undershot
// plan seeds as written); it only selects the firmer "target is the size to reach"
// budget directive for the high tier. Only high has one: the expansive tier exists
// to be filled, and planners tend to undershoot its target. Lower levels
// keep zero, where a lean plan is the point.
func AmbitionTaskFloor(level string) int {
	if NormalizeAmbition(level) == AmbitionHigh {
		return 14
	}
	return 0
}

// ownerSpecifiedBriefWords is the word count at/above which an EXPAND-off brief
// counts as owner-specified scope rather than scope the planner must infer. It
// mirrors the floor of EXPAND's "low" band (~150 words): a brief that detailed has
// already done EXPAND's job, so its scope is honoured rather than capped.
const ownerSpecifiedBriefWords = 150

// PlanTaskBudget resolves the task budget from the ambition level and the brief,
// reporting whether the ceiling is enforced (gates a corrective retry) or
// advisory. The cap is enforced when scope is engine-inferred (any EXPAND level,
// or off with a terse brief); off + a detailed brief means the owner specified
// the scope, so the budget turns advisory. brief matters only for the off case.
func PlanTaskBudget(level, brief string) (target, ceiling int, enforced bool) {
	target, ceiling = AmbitionTaskBudget(level)
	if NormalizeAmbition(level) != AmbitionOff {
		return target, ceiling, true
	}
	if briefWordCount(brief) >= ownerSpecifiedBriefWords {
		// Owner-specified scope: surface a generous size sense (high band) but do
		// not gate on it - honour what the brief lays out.
		t, c := AmbitionTaskBudget(AmbitionHigh)
		return t, c, false
	}
	// Terse off brief: the planner is inferring scope from a handful of words;
	// keep the lean cap enforced so it cannot balloon into a grinding build.
	return target, ceiling, true
}

// briefWordCount counts whitespace-separated tokens in a brief - a rough proxy for
// how much scope the owner spelled out (used only to classify an off brief as
// terse vs owner-specified).
func briefWordCount(brief string) int {
	return len(strings.Fields(brief))
}
