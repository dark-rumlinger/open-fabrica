package engine

// Command handling: the switch that turns a Command drained by commandLoop
// (worker.go) into a state transition or a wake/resume.

import (
	"context"
	"strings"

	"openfabrica/db/models"
)

func (w *Worker) handleCommand(ctx context.Context, cmd Command) {
	w.logger.Info("handling command", "type", cmd.Type)
	switch cmd.Type {
	case CommandWake:
		w.handleWakeCommand(ctx, cmd)

	case CommandModeChange:
		mode, _ := cmd.Payload["mode"].(string)
		switch mode {
		case models.ModeBuilding:
			w.chatThread = nil
			w.setState(StateBuilding) // commandLoop re-enters drive
		case models.ModeLive:
			// Explicit move to live clears any run pause so wakes flow again.
			if run, _ := activeRun(w.pdb); run != nil && run.Status == RunStatusPaused {
				_ = run.resume(w.pdb)
			}
			w.setState(StateLive)
		case models.ModePaused:
			w.setState(StatePaused)
		}

	case CommandUpdate:
		// Incremental update: a fresh run of kind=update re-plans against the
		// current plan; BUILD then re-presents the full checklist against the
		// current project_state snapshot and the model rebuilds only what changed
		// (model-guided from the snapshot, not a mechanical plan diff).
		desc, _ := cmd.Payload["description"].(string)
		w.chatThread = nil
		run, err := createRun(w.pdb, RunKindUpdate, StagePlan, desc)
		if err != nil {
			w.logger.Error("create update run failed", "error", err)
			return
		}
		// Carry the current plan so PLAN sees the change-request context.
		if planJSON, perr := latestCompletedPlan(w.pdb); perr == nil && planJSON != "" {
			run.PlanJSON = planJSON
			if uerr := run.update(w.pdb, "plan_json = ?", planJSON); uerr != nil {
				w.logger.Warn("carry plan into update run failed", "error", uerr)
			}
		}
		w.setState(StateBuilding) // commandLoop re-enters drive

	case CommandChat:
		msg, _ := cmd.Payload["message"].(string)
		if strings.TrimSpace(msg) == "" {
			return
		}
		// The engine is the one agent the owner talks to. Mid-build messages normally
		// reach the inbox (Send); one that raced the state flip lands here and joins it.
		// Every other state - live, paused, idle, error - answers with a LIVE wake.
		if w.State() == StateBuilding {
			w.pushOwnerInbox(msg)
			return
		}
		w.handleChatWake(ctx, msg)

	case CommandScheduledTask:
		w.handleScheduledTask(ctx, cmd)

	case CommandShutdown:
		w.logger.Info("shutdown command received")
		w.cancel()
	}
}

// handleWakeCommand resumes a paused worker (typically: owner answered) or
// routes an answer that arrived while live.
func (w *Worker) handleWakeCommand(ctx context.Context, cmd Command) {
	payload := map[string]any{}
	for k, v := range cmd.Payload {
		payload[k] = v
	}

	// Manual owner pause is sticky: wakes never auto-resume it.
	if project, err := models.GetProjectByID(w.deps.DB.DB, w.projectID); err == nil && project.Mode == models.ModePaused {
		w.logger.Info("wake suppressed: project manually paused")
		return
	}

	if w.State() == StatePaused {
		run, _ := activeRun(w.pdb)
		if run == nil && w.countPendingQuestions(QuestionOriginLive) > 0 {
			// A LIVE or scheduled batch: wait for the last answer, as a build does.
			w.logger.Info("wake suppressed: live questions still pending")
			return
		}
		if run != nil && run.PauseReason == PauseReasonOwnerAnswers {
			if w.pendingQuestionCount() > 0 {
				w.logger.Info("wake suppressed: questions still pending")
				return
			}
			if answers := w.collectAnswers(); answers != "" {
				payload["answer"] = answers
			}
		}
		if run != nil {
			if err := run.resume(w.pdb); err != nil {
				w.logger.Warn("resume run failed", "error", err)
			}
		}
		w.wakeContext = payload

		// Resume in the project's configured mode.
		project, err := models.GetProjectByID(w.deps.DB.DB, w.projectID)
		if err == nil && project.Mode == models.ModeLive {
			w.setState(StateLive)
			w.handleLiveResume(ctx)
			return
		}
		w.setState(StateBuilding) // commandLoop re-enters drive
		return
	}

	if w.State() == StateLive {
		answer, _ := cmd.Payload["answer"].(string)
		if answer == "" {
			return
		}
		if w.answeredQuestionOrigin(cmd.Payload) == QuestionOriginLive {
			w.wakeContext = payload
			w.handleLiveResume(ctx)
		} else {
			w.publishMessage("Thanks, noted. That was a setup question from the original build, so there's nothing to change now.")
		}
	}
}

// answeredQuestionOrigin reads the answered question's recorded origin so a
// stale build clarification answered after go-live is not mistaken for a live
// owner-reported issue.
func (w *Worker) answeredQuestionOrigin(payload map[string]any) string {
	qid := payloadInt64(payload, "question_id")
	if qid == 0 {
		return QuestionOriginBuild
	}
	var origin string
	if err := w.pdb.Reader().QueryRow(
		"SELECT COALESCE(origin,'build') FROM of_questions WHERE id = ?", qid).Scan(&origin); err != nil {
		return QuestionOriginBuild
	}
	return origin
}

// collectAnswers aggregates recent answered/deferred questions into one block.
// Bounded to the newest 20 (in chronological order) so a long-lived project
// does not grow this block without limit.
func (w *Worker) collectAnswers() string {
	rows, err := w.pdb.Query(
		`SELECT question, answer FROM (
		   SELECT q.id AS id, q.question AS question, a.answer AS answer
		   FROM of_questions q
		   JOIN of_answers a ON a.question_id = q.id
		   WHERE q.status IN ('answered','deferred')
		   ORDER BY q.id DESC LIMIT 20
		 ) ORDER BY id`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var q, a string
		if rows.Scan(&q, &a) == nil {
			parts = append(parts, q+": "+a)
		}
	}
	if rows.Err() != nil {
		return ""
	}
	out := strings.Join(parts, "\n")
	if strings.Contains(out, DeferredAnswerText) {
		out += "\n(" + DeferredAnswerText + " means the owner skipped that question for now: pick a sensible default, say so in the plan, and do not ask it again.)"
	}
	return out
}

// DeferredAnswerText is the answer stored when the owner skips a question. The bot
// and the admin reuse it, and collectAnswers explains it to the model.
const DeferredAnswerText = "(owner will provide later)"
