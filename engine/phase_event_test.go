package engine

import (
	"testing"

	"openfabrica/events"
	"openfabrica/internal/projectphase"
)

// phaseEvents returns the phases announced so far, oldest first.
func phaseEvents(evs []events.Event) []projectphase.Phase {
	var out []projectphase.Phase
	for _, e := range evs {
		if e.Type != events.EventProjectPhaseChanged {
			continue
		}
		if p, ok := e.Payload["phase"].(projectphase.Phase); ok {
			out = append(out, p)
		}
	}
	return out
}

// Parking a run on the owner leaves the stored mode at building, so the mode
// event alone cannot tell the admin anything happened. The phase event must.
func TestParkOnAnswersAnnouncesNeedsYouThenBuildingOnResume(t *testing.T) {
	worker, _ := newStopTestWorker(t)
	snapshot := captureEvents(t, worker, nil)

	run, err := createRun(worker.pdb, RunKindBuild, StagePlan, "")
	if err != nil {
		t.Fatal(err)
	}
	worker.run = run
	if _, err := worker.pdb.ExecWrite(
		`INSERT INTO of_questions (question, status, type) VALUES ('Brand colour?', 'pending', 'text')`); err != nil {
		t.Fatal(err)
	}

	worker.setState(StateBuilding)
	worker.pauseAwaitingAnswers()

	got := phaseEvents(snapshot())
	if len(got) != 2 {
		t.Fatalf("phase events = %+v, want building then needs_you", got)
	}
	if got[0].Key != projectphase.KeyBuilding || got[1].Key != projectphase.KeyNeedsYou {
		t.Fatalf("phase keys = %q, %q, want building, needs_you", got[0].Key, got[1].Key)
	}
	if got[1].Pending != 1 || got[1].Reason != "1 question needs your answer" {
		t.Errorf("needs_you = %+v, want the pending question counted", got[1])
	}

	if err := run.resume(worker.pdb); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.pdb.ExecWrite(`UPDATE of_questions SET status = 'answered'`); err != nil {
		t.Fatal(err)
	}
	worker.setState(StateBuilding)

	got = phaseEvents(snapshot())
	if last := got[len(got)-1]; last.Key != projectphase.KeyBuilding {
		t.Errorf("phase after resume = %q, want building", last.Key)
	}
}

func TestPhaseIsAnnouncedOnlyWhenItChanges(t *testing.T) {
	worker, _ := newStopTestWorker(t)
	snapshot := captureEvents(t, worker, nil)

	worker.setState(StateBuilding)
	worker.setState(StateBuilding)
	worker.publishPhase()

	if n := len(phaseEvents(snapshot())); n != 1 {
		t.Errorf("phase events = %d, want 1 for an unchanged phase", n)
	}
}

// An owner stop mirrors into the stored mode, so the announced phase is paused
// with the reason attached rather than a building project that went quiet.
func TestOwnerStopAnnouncesPausedWithReason(t *testing.T) {
	worker, _ := newStopTestWorker(t)
	snapshot := captureEvents(t, worker, nil)

	run, err := createRun(worker.pdb, RunKindBuild, StageBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	worker.run = run
	worker.setState(StateBuilding)
	worker.pauseOwnerStop()

	got := phaseEvents(snapshot())
	last := got[len(got)-1]
	if last.Key != projectphase.KeyPaused || last.Reason != "Stopped by you" {
		t.Errorf("phase after an owner stop = %+v, want paused, stopped by you", last)
	}
	if last.ResumeTo != "building" {
		t.Errorf("resume target = %q, want building for an unfinished run", last.ResumeTo)
	}
}
