package engine

import (
	"context"
	"testing"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

func setProjectMode(t *testing.T, w *Worker, mode string) {
	t.Helper()
	if _, err := w.deps.DB.ExecWrite("UPDATE projects SET mode = ? WHERE id = 1", mode); err != nil {
		t.Fatalf("set mode: %v", err)
	}
}

func projectMode(t *testing.T, w *Worker) string {
	t.Helper()
	var mode string
	if err := w.deps.DB.QueryRow("SELECT mode FROM projects WHERE id = 1").Scan(&mode); err != nil {
		t.Fatalf("read mode: %v", err)
	}
	return mode
}

func insertQuestion(t *testing.T, pdb *db.ProjectDB, q, status string) int64 {
	t.Helper()
	res, err := pdb.ExecWrite("INSERT INTO of_questions (question, status) VALUES (?, ?)", q, status)
	if err != nil {
		t.Fatalf("insert question: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertAnswer(t *testing.T, pdb *db.ProjectDB, qid int64, answer string) {
	t.Helper()
	if _, err := pdb.ExecWrite("INSERT INTO of_answers (question_id, answer, answered_by) VALUES (?, ?, 'owner')", qid, answer); err != nil {
		t.Fatalf("insert answer: %v", err)
	}
}

// A restart while the owner has not answered yet must park the worker without
// flipping projects.mode to paused: a sticky manual pause would swallow the
// answer's wake and the build would never resume on its own.
func TestDriveWaitingOnAnswersKeepsProjectMode(t *testing.T) {
	for _, reason := range []string{PauseReasonOwnerAnswers, PauseReasonApproval} {
		t.Run(reason, func(t *testing.T) {
			w, pdb := scheduledTestWorker(t, nil)
			setProjectMode(t, w, "building")
			run, err := createRun(pdb, RunKindBuild, StageBuild, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := run.pause(pdb, reason); err != nil {
				t.Fatal(err)
			}
			insertQuestion(t, pdb, "Which palette?", "pending")

			w.drive(context.Background())

			if got := projectMode(t, w); got != "building" {
				t.Fatalf("projects.mode = %q, want building (waiting is not a manual pause)", got)
			}
			if w.State() != StatePaused {
				t.Fatalf("worker state = %v, want paused", w.State())
			}
		})
	}
}

// Once every question is answered, a restart folds the answers in and resumes.
func TestAutoRecoverResumesWhenAnswered(t *testing.T) {
	for _, reason := range []string{PauseReasonOwnerAnswers, PauseReasonApproval} {
		t.Run(reason, func(t *testing.T) {
			w, pdb := scheduledTestWorker(t, nil)
			run, _ := createRun(pdb, RunKindBuild, StageBuild, "")
			if err := run.pause(pdb, reason); err != nil {
				t.Fatal(err)
			}
			qid := insertQuestion(t, pdb, "Which palette?", "answered")
			insertAnswer(t, pdb, qid, "Ocean")

			proceed, waiting := w.autoRecover(run)
			if !proceed || waiting {
				t.Fatalf("proceed=%v waiting=%v, want to proceed", proceed, waiting)
			}
			if run.Status != RunStatusRunning {
				t.Fatalf("run status = %q, want running", run.Status)
			}
			if a, _ := w.wakeContext["answer"].(string); a != "Which palette?: Ocean" {
				t.Fatalf("answers not folded into the wake context: %v", w.wakeContext)
			}
		})
	}
}

// A failure pause still flips the mode: it needs an explicit owner action.
func TestAutoRecoverErrorCountStillPausesMode(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	run, _ := createRun(pdb, RunKindBuild, StageBuild, "")
	run.ErrorCount = maxStageErrors
	proceed, waiting := w.autoRecover(run)
	if proceed || waiting {
		t.Fatalf("proceed=%v waiting=%v, want a hard pause", proceed, waiting)
	}
}

// A LIVE question must not reopen the finished build run.
func TestParkRunLeavesCompletedRunAlone(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	run, _ := createRun(pdb, RunKindBuild, StageLive, "")
	if err := run.complete(pdb); err != nil {
		t.Fatal(err)
	}
	w.run = run

	w.pauseAwaitingAnswers()

	last, err := latestRun(pdb)
	if err != nil || last == nil {
		t.Fatalf("latest run: %v %v", last, err)
	}
	if last.Status != RunStatusCompleted {
		t.Fatalf("finished run status = %q, want completed", last.Status)
	}
	if w.State() != StatePaused {
		t.Fatalf("worker state = %v, want paused", w.State())
	}
}

// A scheduled task that asks the owner parks the worker, tags the question as
// live-origin, and the wake waits for the whole batch.
func TestScheduledTaskAskParksWorker(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(llm.ToolCall{ID: "f1", Name: tools.FindToolName, Arguments: `{"names":["manage_chat"]}`}),
		enginetest.Calls(llm.ToolCall{ID: "q1", Name: "manage_chat",
			Arguments: `{"action":"ask","question":"Archive last year's orders?","options":["Yes","No"]}`}),
		enginetest.Text("never reached"),
	}}
	w, pdb := scheduledTestWorker(t, p)
	taskID, runID := seedScheduledTask(t, pdb, "tidy old orders", "")

	w.handleScheduledTask(context.Background(), Command{
		Type:    CommandScheduledTask,
		Payload: map[string]any{"task_id": taskID, "run_id": runID, "prompt": "tidy old orders"},
	})

	if w.State() != StatePaused {
		t.Fatalf("worker state = %v, want paused while the question is open", w.State())
	}
	var origin string
	if err := pdb.QueryRow("SELECT origin FROM of_questions WHERE status = 'pending'").Scan(&origin); err != nil {
		t.Fatalf("pending question: %v", err)
	}
	if origin != QuestionOriginLive {
		t.Fatalf("origin = %q, want live", origin)
	}

	// A second live question keeps the wake suppressed after the first answer.
	second := insertQuestion(t, pdb, "And the year before?", "pending")
	if _, err := pdb.ExecWrite("UPDATE of_questions SET origin = 'live' WHERE id = ?", second); err != nil {
		t.Fatal(err)
	}
	if _, err := pdb.ExecWrite("UPDATE of_questions SET status = 'answered' WHERE id != ?", second); err != nil {
		t.Fatal(err)
	}
	w.handleWakeCommand(context.Background(), Command{Type: CommandWake, Payload: map[string]any{"answer": "Yes"}})
	if w.State() != StatePaused {
		t.Fatalf("wake must wait for the last live answer, state = %v", w.State())
	}
}
