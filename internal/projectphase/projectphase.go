// Package projectphase derives the one owner-facing phase of a project from the
// stored mode, the latest run, and the open questions. The admin API, the admin
// UI, and the Telegram bot all show this value, so one project cannot read as
// "Building" in one place and "Waiting" in another.
//
// The phase is computed on read and never stored. Only projects.mode is stored;
// waiting on the owner is a run state, and mirroring it into a column would
// need every pause and resume path to keep the copy in step.
package projectphase

import (
	"database/sql"
	"fmt"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/buildstate"
)

// Phase keys are stable strings the UI branches on.
const (
	KeyBuilding = "building"
	KeyNeedsYou = "needs_you"
	KeyLive     = "live"
	KeyPaused   = "paused"
)

// Reason keys are stable strings the UI translates.
const (
	ReasonAnswers      = "answers"       // N questions need your answer
	ReasonAnswersNone  = "answers_none"  // waiting for an answer, with no count to show
	ReasonApproval     = "approval"      // waiting for the owner's approval
	ReasonEngineDown   = "engine_down"   // the engine is shut down
	ReasonStoppedOwner = "stopped_owner" // the owner stopped the run
	ReasonStoppedError = "stopped_error" // the run stopped after an error
	ReasonPausedOwner  = "paused_owner"  // the owner paused the project
)

// Phase is what an owner sees for a project.
type Phase struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Reason is one short line explaining the phase, empty when the label says it all.
	// It is English: the Telegram bot shows it as written.
	Reason string `json:"reason"`
	// ReasonKey names the reason so the admin UI can say it in its own language
	// (one of the Reason* constants), empty when Reason is. Pending supplies the
	// count for ReasonAnswers.
	ReasonKey string `json:"reason_key"`
	// Pending is the number of unanswered questions.
	Pending int `json:"pending"`
	// Stalled marks a building project whose engine is shut down, so nothing
	// is progressing until the engine is started again.
	Stalled bool `json:"stalled"`
	// ResumeTo is the mode a paused project returns to: building while a run
	// is unfinished, live once the last run completed. Empty unless paused.
	ResumeTo string `json:"resume_to,omitempty"`
}

// Inputs are the facts a phase is derived from.
type Inputs struct {
	Mode          string
	RunStatus     string // status of the latest run, empty when there is none
	PauseReason   string // pause_reason of the latest run
	Pending       int
	EngineRunning bool
}

// Derive maps the inputs to a phase.
func Derive(in Inputs) Phase {
	p := Phase{Pending: in.Pending}
	switch in.Mode {
	case models.ModeLive:
		p.Key, p.Label = KeyLive, "Live"
	case models.ModePaused:
		p.Key, p.Label = KeyPaused, "Paused"
		p.Reason, p.ReasonKey = pausedReason(in)
		p.ResumeTo = models.ModeBuilding
		if in.RunStatus == buildstate.RunCompleted {
			p.ResumeTo = models.ModeLive
		}
	default:
		if in.RunStatus == buildstate.RunPaused {
			switch in.PauseReason {
			case buildstate.PauseAwaitingAnswers:
				p.Key, p.Label = KeyNeedsYou, "Needs you"
				p.Reason, p.ReasonKey = answersReason(in.Pending)
				return p
			case buildstate.PauseAwaitingApproval:
				p.Key, p.Label = KeyNeedsYou, "Needs you"
				p.Reason, p.ReasonKey = "Waiting for your approval", ReasonApproval
				return p
			}
		}
		p.Key, p.Label = KeyBuilding, "Building"
		if !in.EngineRunning {
			p.Stalled = true
			p.Reason, p.ReasonKey = "The engine is shut down", ReasonEngineDown
		}
	}
	return p
}

func answersReason(pending int) (reason, key string) {
	switch {
	case pending == 1:
		return "1 question needs your answer", ReasonAnswers
	case pending > 1:
		return fmt.Sprintf("%d questions need your answer", pending), ReasonAnswers
	}
	return "Waiting for your answer", ReasonAnswersNone
}

func pausedReason(in Inputs) (reason, key string) {
	switch {
	case in.PauseReason == buildstate.PauseOwnerStop:
		return "Stopped by you", ReasonStoppedOwner
	case in.RunStatus == buildstate.RunFailed:
		return "Stopped after an error", ReasonStoppedError
	case in.RunStatus == buildstate.RunPaused && in.PauseReason != "" &&
		in.PauseReason != buildstate.PauseAwaitingAnswers &&
		in.PauseReason != buildstate.PauseAwaitingApproval:
		return "Stopped after an error", ReasonStoppedError
	}
	return "Paused by you", ReasonPausedOwner
}

// RowQuerier is the read access LoadFor needs: a project database handle.
type RowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// Load derives the phase of a project. The project database is read only when
// it is already open and the mode needs run detail; a closed database yields a
// phase from the mode alone. Callers that want run detail for one project open
// its database first.
func Load(mgr *db.ProjectDBManager, engineRunning bool, p *models.Project) Phase {
	var q RowQuerier
	if mgr != nil {
		if pdb := mgr.Get(p.ID); pdb != nil {
			q = pdb
		}
	}
	return LoadFor(q, p.Mode, engineRunning)
}

// LoadFor derives a phase from a stored mode and, when q is not nil, the run and
// question rows of the project database behind it. A live project needs no run
// detail, so it is never queried.
func LoadFor(q RowQuerier, mode string, engineRunning bool) Phase {
	in := Inputs{Mode: mode, EngineRunning: engineRunning}
	if mode != models.ModeLive && q != nil {
		_ = q.QueryRow(
			`SELECT status, COALESCE(pause_reason, '') FROM of_runs
			 ORDER BY started_at DESC, id DESC LIMIT 1`,
		).Scan(&in.RunStatus, &in.PauseReason)
		_ = q.QueryRow(
			`SELECT COUNT(*) FROM of_questions WHERE status = 'pending'`,
		).Scan(&in.Pending)
	}
	return Derive(in)
}
