// Package buildstate names the persisted values of a project's build state: the
// status columns of of_runs and of_tasks, and the kinds and stage statuses written
// to of_run_log. The engine writes them; the admin triage/cost views, live chat,
// and the task tool read or write them too. Defining them once in a leaf package
// (it imports nothing) lets every one of those packages share the exact strings
// instead of mirroring literals that drift apart silently.
//
// The values are stored in the database, so they are part of the on-disk format:
// never rename one without a migration.
package buildstate

// of_runs.status.
const (
	RunRunning   = "running"
	RunPaused    = "paused"
	RunCompleted = "completed"
	RunFailed    = "failed"
)

// of_tasks.status.
const (
	TaskPending = "pending"
	TaskActive  = "active"
	TaskDone    = "done"
	TaskBlocked = "blocked"
)

// of_runs.pause_reason for a run parked on something other than a failure. Any
// other non-empty reason is failure text.
const (
	PauseAwaitingAnswers  = "awaiting_owner_answers"
	PauseAwaitingApproval = "awaiting_approval"
	PauseOwnerStop        = "stopped_by_owner"
)

// UserFlowsTaskID is the id of the checklist item the engine appends to every plan with
// pages (write and replay of_test.js). The task tool refuses to complete it until
// of_test.js exists, since the item declares no owned paths to prove it.
const UserFlowsTaskID = "user-flows"

// of_run_log.kind.
const (
	LogStage      = "stage"   // ref = stage name; payload {status, error?}
	LogTool       = "tool"    // ref = tool name;  payload {action, ok, summary}
	LogMessage    = "message" // ref = "";         payload {text}
	LogIssue      = "issue"   // ref = validator;  payload {issues: [...]}
	LogSession    = "session" // ref = session id; payload {segment, end_reason, iterations}
	LogSummary    = "summary" // ref = "";         payload {calls, tokens, cache stats}
	LogTask       = "task"    // ref = task id;    payload {iteration, title} - one row per task completion
	LogEditStats  = "edit_stats"
	LogValidation = "validation"
)

// The status a LogStage entry records for one pass through a stage.
const (
	StageStarted   = "started"
	StageCompleted = "completed"
	StageStopped   = "stopped" // the owner stopped the run mid-stage
	StagePaused    = "paused"  // parked awaiting owner input
	StageFailed    = "failed"
	StageBlocked   = "blocked" // finished, but blocking issues kept it from going live
)
