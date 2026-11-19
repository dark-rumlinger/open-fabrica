package engine

// Regression test for the wake-answer lifetime: an owner answer folded into
// the wake context must survive a transient stage failure, so the drive
// loop's in-place retry re-plans WITH the answer instead of answerless.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

func TestWakeAnswerSurvivesTransientStageRetry(t *testing.T) {
	dir := t.TempDir()
	gdb, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	defer gdb.Close()
	mgr := db.NewProjectDBManager(dir)
	defer mgr.CloseAll()

	if _, err := gdb.ExecWrite(
		`INSERT INTO projects (id, name, description, published, mode, llm_model_id, ambition_level)
		 VALUES (1, 'Tip Calc', 'a one-page tip calculator', 1, 'building', 1, 'off')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	// Scripted run: the first PLAN main call dies with an untyped (transient at
	// the stage layer, non-retryable at the call layer) error; the drive loop
	// retries the stage, whose second attempt must still carry the answer.
	const answerMarker = "Use euros for all prices."
	provider := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass, attempt 1
		enginetest.Fail(errors.New("scripted transient hiccup")), // PLAN main call, attempt 1
		// Attempt 2's pre-pass repeats attempt 1's request byte-for-byte, so
		// the replay cache may serve it without consuming a step. Either
		// alignment completes: on a cache hit this selectNoGuides lands on the
		// main call (whose parse failure makes planJSON the corrective retry);
		// on a miss it feeds the pre-pass and planJSON the main call.
		selectNoGuides, noFeatures,
		enginetest.Text(planJSON),
		enginetest.Calls(llm.ToolCall{ID: "c1", Name: "manage_files",
			Arguments: `{"action":"save","kind":"page","file_path":"/","body":"<h1>Tip calculator</h1>","title":"Tip calculator"}`}),
		enginetest.Calls(llm.ToolCall{ID: "c2", Name: "manage_tasks",
			Arguments: `{"action":"complete","task_id":"home-page","result":"built the calculator page"}`}),
	}}

	deps := &Deps{
		DB:               gdb,
		ProjectDBManager: mgr,
		ToolRegistry:     tools.DefaultRegistry(),
		ToolExecutor:     tools.NewExecutor(tools.DefaultRegistry()),
	}
	worker, err := newWorker(deps, 1, newLLMLimiter(2), context.Background())
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	defer worker.dbLog.Close()
	worker.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: provider, Model: "fake-model", ProviderID: 1}}

	// The answer a wake/resume would have folded in (handleWakeCommand /
	// autoRecover both set this exact shape).
	worker.wakeContext = map[string]any{"reason": "question_answered", "answer": answerMarker}

	// No validators (the runtime probe needs a server) and no real backoff.
	validatorMu.Lock()
	savedValidators := validatorSet
	validatorSet = nil
	validatorMu.Unlock()
	savedBase := stageRetryBase
	stageRetryBase = time.Millisecond
	defer func() {
		validatorMu.Lock()
		validatorSet = savedValidators
		validatorMu.Unlock()
		stageRetryBase = savedBase
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	worker.setState(StateBuilding)
	worker.drive(ctx)

	if got := worker.State(); got != StateLive {
		t.Fatalf("worker state = %v, want live", got)
	}

	// Both PLAN main calls - before AND after the transient failure - must
	// carry the owner's answer in their user messages.
	planCallsWithAnswer := 0
	for _, req := range provider.Requests {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, answerMarker) {
				planCallsWithAnswer++
				break
			}
		}
	}
	if planCallsWithAnswer < 2 {
		t.Fatalf("answer reached %d PLAN call(s), want 2 (initial + stage retry): the wake answer was lost on retry", planCallsWithAnswer)
	}

	// Settled run: the payload must not leak into later stages or runs.
	if worker.wakeContext != nil {
		t.Fatal("wakeContext not cleared after the run settled")
	}
}

// collectAnswers is bounded to the newest 20 answers, kept in chronological
// order, so the block cannot grow without limit over a project's life.
func TestCollectAnswersBounded(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	w := &Worker{pdb: pdb}

	for i := 1; i <= 25; i++ {
		res, err := pdb.ExecWrite(`INSERT INTO of_questions (question, status) VALUES (?, 'answered')`, fmt.Sprintf("q%02d", i))
		if err != nil {
			t.Fatalf("insert question: %v", err)
		}
		qid, _ := res.LastInsertId()
		if _, err := pdb.ExecWrite(`INSERT INTO of_answers (question_id, answer) VALUES (?, ?)`, qid, fmt.Sprintf("a%02d", i)); err != nil {
			t.Fatalf("insert answer: %v", err)
		}
	}

	lines := strings.Split(w.collectAnswers(), "\n")
	if len(lines) != 20 {
		t.Fatalf("got %d lines, want the newest 20", len(lines))
	}
	if !strings.HasPrefix(lines[0], "q06:") {
		t.Errorf("oldest kept line should be q06, got %q", lines[0])
	}
	if !strings.HasPrefix(lines[19], "q25:") {
		t.Errorf("newest line should be q25, got %q", lines[19])
	}
}
