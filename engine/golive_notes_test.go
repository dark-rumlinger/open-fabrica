package engine

import (
	"log/slog"
	"strings"
	"testing"

	"openfabrica/db"
)

// A secret the build stored itself (a staff passcode) is named at go-live, so the
// owner can set a value they know. One saved before the run is not this build's.
func TestSecretNotesNameThisRunsSecrets(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	for _, q := range []string{
		`INSERT INTO of_runs (id, kind, status, stage, started_at) VALUES ('run_1', 'build', 'running', 'build', '2026-10-16 10:24:08')`,
		`INSERT INTO of_secrets (name, value_encrypted, updated_at) VALUES ('OLD_KEY', 'x', '2026-10-15 09:00:00')`,
		`INSERT INTO of_secrets (name, value_encrypted, updated_at) VALUES ('staff_passcode', 'x', '2026-10-16 10:26:12')`,
	} {
		if _, err := pdb.ExecWrite(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	w := &Worker{projectID: 7, pdb: pdb, logger: slog.Default(), run: &Run{ID: "run_1"}}
	notes := w.secretNotes()
	if len(notes) != 1 {
		t.Fatalf("notes = %q, want one note", notes)
	}
	if !strings.Contains(notes[0], "`staff_passcode`") || strings.Contains(notes[0], "OLD_KEY") {
		t.Errorf("note should name only this run's secret: %q", notes[0])
	}
	if !strings.Contains(notes[0], "/projects/7/secrets") {
		t.Errorf("note should link the API keys page: %q", notes[0])
	}

	if (&Worker{projectID: 7, pdb: pdb, logger: slog.Default()}).secretNotes() != nil {
		t.Error("no run means no notes")
	}
}
