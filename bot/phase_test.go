package bot

import (
	"strings"
	"testing"

	"openfabrica/db/models"
	"openfabrica/internal/projectphase"
)

func TestPhaseLine(t *testing.T) {
	cases := []struct {
		name  string
		phase projectphase.Phase
		want  string
	}{
		{"label alone", projectphase.Phase{Key: projectphase.KeyLive, Label: "Live"}, "Live"},
		{"reason appended", projectphase.Phase{Key: projectphase.KeyPaused, Label: "Paused", Reason: "Stopped by you"}, "Paused · Stopped by you"},
		{"needs you leaves the reason to the question count", projectphase.Phase{Key: projectphase.KeyNeedsYou, Label: "Needs you", Reason: "2 questions need your answer"}, "Needs you"},
	}
	for _, c := range cases {
		if got := (voice{}).phaseLine(c.phase); got != c.want {
			t.Errorf("%s: phaseLine = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDashboardShowsPhaseNotEngineAndMode(t *testing.T) {
	svc, fp, pdb := newTestService(t)
	screenUser(t, svc, "Acme")
	if _, err := pdb.ExecWrite(`INSERT INTO of_runs (id, status, stage, pause_reason)
		VALUES ('r1', 'paused', 'build', 'awaiting_owner_answers')`); err != nil {
		t.Fatal(err)
	}

	svc.HandleCallback("telegram", "555", "999", 100, "nav:menu")

	text := lastEdit(fp)
	if !strings.Contains(text, "Phase: <b>Needs you</b>") {
		t.Errorf("dashboard should lead with the phase, got:\n%s", text)
	}
	for _, gone := range []string{"Builder:", "Mode:"} {
		if strings.Contains(text, gone) {
			t.Errorf("dashboard still shows %q:\n%s", gone, text)
		}
	}
}

func TestSettingsScreenSeparatesPhaseFromPublished(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := screenUser(t, svc, "Acme")
	if err := models.UpdateProjectMode(svc.deps.DB.DB, st.ActiveProjectID, models.ModeLive); err != nil {
		t.Fatal(err)
	}
	if err := models.SetProjectPublished(svc.deps.DB.DB, st.ActiveProjectID, false); err != nil {
		t.Fatal(err)
	}

	sc := svc.settingsScreen(st)
	for _, want := range []string{"Phase: Live", "Published: no"} {
		if !strings.Contains(sc.text, want) {
			t.Errorf("settings missing %q in:\n%s", want, sc.text)
		}
	}
	joined := strings.Join(sc.labels, "|")
	if !strings.Contains(joined, "Publish") || strings.Contains(joined, "live") {
		t.Errorf("publish button should not say live, got %q", joined)
	}

	if err := models.SetProjectPublished(svc.deps.DB.DB, st.ActiveProjectID, true); err != nil {
		t.Fatal(err)
	}
	sc = svc.settingsScreen(st)
	if !strings.Contains(sc.text, "Published: yes") || !strings.Contains(strings.Join(sc.labels, "|"), "Unpublish") {
		t.Errorf("published project should offer Unpublish:\n%s\n%v", sc.text, sc.labels)
	}
}

func TestApplyModePersistsSoAPauseSticks(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := screenUser(t, svc, "Acme")

	if err := svc.applyMode(st, models.ModePaused); err != nil {
		t.Fatal(err)
	}
	p, err := models.GetProjectByID(svc.deps.DB.DB, st.ActiveProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != models.ModePaused {
		t.Errorf("mode after a bot pause = %q, want %q", p.Mode, models.ModePaused)
	}
	if got := svc.projectPhase(p); got.Key != projectphase.KeyPaused {
		t.Errorf("phase after a bot pause = %q, want %q", got.Key, projectphase.KeyPaused)
	}
}

func TestPhasePickerSetsTheStoredModeAndNamesThePhase(t *testing.T) {
	svc, _, _ := newTestService(t)
	st := screenUser(t, svc, "Acme")

	svc.HandleCallback("telegram", "555", "999", 100, "phase:paused")
	p, err := models.GetProjectByID(svc.deps.DB.DB, st.ActiveProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != models.ModePaused {
		t.Errorf("mode after picking Paused = %q, want %q", p.Mode, models.ModePaused)
	}
	if got := (voice{}).modeLabel(models.ModePaused); got != "Paused" {
		t.Errorf("modeLabel(paused) = %q, want Paused", got)
	}
	sc := svc.phaseScreen(voice{})
	if strings.Join(sc.cbs, "|") != "phase:building|phase:live|phase:paused|nav:settings" {
		t.Errorf("phase picker callbacks = %v", sc.cbs)
	}
}
