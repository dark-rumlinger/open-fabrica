package projectphase

import (
	"testing"

	"openfabrica/db/models"
	"openfabrica/internal/buildstate"
)

func TestDerive(t *testing.T) {
	cases := []struct {
		name    string
		in      Inputs
		key     string
		label   string
		reason  string
		stalled bool
	}{
		{"building running", Inputs{Mode: models.ModeBuilding, RunStatus: buildstate.RunRunning, EngineRunning: true},
			KeyBuilding, "Building", "", false},
		{"building without a run yet", Inputs{Mode: models.ModeBuilding, EngineRunning: true},
			KeyBuilding, "Building", "", false},
		{"building with engine shut down", Inputs{Mode: models.ModeBuilding, RunStatus: buildstate.RunRunning},
			KeyBuilding, "Building", "The engine is shut down", true},
		{"empty mode reads as building", Inputs{EngineRunning: true},
			KeyBuilding, "Building", "", false},
		{"parked on one question", Inputs{Mode: models.ModeBuilding, RunStatus: buildstate.RunPaused,
			PauseReason: buildstate.PauseAwaitingAnswers, Pending: 1, EngineRunning: true},
			KeyNeedsYou, "Needs you", "1 question needs your answer", false},
		{"parked on several questions", Inputs{Mode: models.ModeBuilding, RunStatus: buildstate.RunPaused,
			PauseReason: buildstate.PauseAwaitingAnswers, Pending: 3, EngineRunning: true},
			KeyNeedsYou, "Needs you", "3 questions need your answer", false},
		{"parked with no count", Inputs{Mode: models.ModeBuilding, RunStatus: buildstate.RunPaused,
			PauseReason: buildstate.PauseAwaitingAnswers, EngineRunning: true},
			KeyNeedsYou, "Needs you", "Waiting for your answer", false},
		{"parked on approval", Inputs{Mode: models.ModeBuilding, RunStatus: buildstate.RunPaused,
			PauseReason: buildstate.PauseAwaitingApproval, EngineRunning: true},
			KeyNeedsYou, "Needs you", "Waiting for your approval", false},
		{"waiting outranks a shut down engine", Inputs{Mode: models.ModeBuilding, RunStatus: buildstate.RunPaused,
			PauseReason: buildstate.PauseAwaitingAnswers},
			KeyNeedsYou, "Needs you", "Waiting for your answer", false},
		{"paused run that is not waiting stays building", Inputs{Mode: models.ModeBuilding, RunStatus: buildstate.RunPaused,
			PauseReason: buildstate.PauseOwnerStop, EngineRunning: true},
			KeyBuilding, "Building", "", false},
		{"live", Inputs{Mode: models.ModeLive, RunStatus: buildstate.RunCompleted, EngineRunning: true},
			KeyLive, "Live", "", false},
		{"live ignores a stopped engine", Inputs{Mode: models.ModeLive},
			KeyLive, "Live", "", false},
		{"live ignores run pauses", Inputs{Mode: models.ModeLive, RunStatus: buildstate.RunPaused,
			PauseReason: buildstate.PauseAwaitingAnswers},
			KeyLive, "Live", "", false},
		{"paused by the owner stop", Inputs{Mode: models.ModePaused, RunStatus: buildstate.RunPaused,
			PauseReason: buildstate.PauseOwnerStop},
			KeyPaused, "Paused", "Stopped by you", false},
		{"paused after a failed run", Inputs{Mode: models.ModePaused, RunStatus: buildstate.RunFailed},
			KeyPaused, "Paused", "Stopped after an error", false},
		{"paused with failure text", Inputs{Mode: models.ModePaused, RunStatus: buildstate.RunPaused,
			PauseReason: "budget exceeded"},
			KeyPaused, "Paused", "Stopped after an error", false},
		{"paused by hand mid-run", Inputs{Mode: models.ModePaused, RunStatus: buildstate.RunRunning},
			KeyPaused, "Paused", "Paused by you", false},
		{"paused while waiting on answers", Inputs{Mode: models.ModePaused, RunStatus: buildstate.RunPaused,
			PauseReason: buildstate.PauseAwaitingAnswers},
			KeyPaused, "Paused", "Paused by you", false},
		{"paused with no runs", Inputs{Mode: models.ModePaused},
			KeyPaused, "Paused", "Paused by you", false},
		{"paused ignores engine state", Inputs{Mode: models.ModePaused, EngineRunning: true},
			KeyPaused, "Paused", "Paused by you", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Derive(c.in)
			if got.Key != c.key || got.Label != c.label || got.Reason != c.reason || got.Stalled != c.stalled {
				t.Errorf("Derive(%+v) = %+v, want key=%q label=%q reason=%q stalled=%v",
					c.in, got, c.key, c.label, c.reason, c.stalled)
			}
		})
	}
}

func TestDeriveResumeTarget(t *testing.T) {
	cases := []struct {
		name string
		in   Inputs
		want string
	}{
		{"finished build resumes to live", Inputs{Mode: models.ModePaused, RunStatus: buildstate.RunCompleted}, models.ModeLive},
		{"unfinished run resumes to building", Inputs{Mode: models.ModePaused, RunStatus: buildstate.RunPaused, PauseReason: buildstate.PauseOwnerStop}, models.ModeBuilding},
		{"failed run resumes to building", Inputs{Mode: models.ModePaused, RunStatus: buildstate.RunFailed}, models.ModeBuilding},
		{"no runs resumes to building", Inputs{Mode: models.ModePaused}, models.ModeBuilding},
		{"live has no target", Inputs{Mode: models.ModeLive}, ""},
		{"building has no target", Inputs{Mode: models.ModeBuilding, EngineRunning: true}, ""},
	}
	for _, c := range cases {
		if got := Derive(c.in).ResumeTo; got != c.want {
			t.Errorf("%s: ResumeTo = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDeriveKeepsPendingCount(t *testing.T) {
	got := Derive(Inputs{Mode: models.ModeLive, Pending: 2})
	if got.Pending != 2 {
		t.Errorf("pending = %d, want 2", got.Pending)
	}
}

func TestLoadWithoutDatabases(t *testing.T) {
	p := &models.Project{ID: 1, Mode: models.ModeBuilding}
	got := Load(nil, true, p)
	if got.Key != KeyBuilding || got.Stalled {
		t.Errorf("Load with no manager = %+v, want a plain building phase", got)
	}
	got = Load(nil, false, p)
	if !got.Stalled {
		t.Errorf("Load with no manager and no engine = %+v, want stalled", got)
	}
}

// Every reason the English line can give has a key the UI translates, and no key
// exists without a line, across every input the phase can be derived from.
func TestReasonKeysCoverEveryReason(t *testing.T) {
	known := map[string]bool{
		ReasonAnswers: true, ReasonAnswersNone: true, ReasonApproval: true, ReasonEngineDown: true,
		ReasonStoppedOwner: true, ReasonStoppedError: true, ReasonPausedOwner: true,
	}
	used := map[string]bool{}
	for _, mode := range []string{models.ModeBuilding, models.ModeLive, models.ModePaused} {
		for _, run := range []string{"", buildstate.RunRunning, buildstate.RunPaused, buildstate.RunCompleted, buildstate.RunFailed} {
			for _, pause := range []string{"", buildstate.PauseAwaitingAnswers, buildstate.PauseAwaitingApproval, buildstate.PauseOwnerStop, "budget exceeded"} {
				for _, pending := range []int{0, 1, 4} {
					for _, running := range []bool{false, true} {
						p := Derive(Inputs{Mode: mode, RunStatus: run, PauseReason: pause, Pending: pending, EngineRunning: running})
						if (p.Reason == "") != (p.ReasonKey == "") {
							t.Errorf("%+v: reason %q and key %q disagree", p, p.Reason, p.ReasonKey)
						}
						if p.ReasonKey != "" && !known[p.ReasonKey] {
							t.Errorf("unknown reason key %q", p.ReasonKey)
						}
						used[p.ReasonKey] = true
					}
				}
			}
		}
	}
	for key := range known {
		if !used[key] {
			t.Errorf("reason key %q is never produced", key)
		}
	}
}
