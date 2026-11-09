package engine

import (
	"strings"
	"testing"
)

// TestUpdateBuildInstructionCarriesChangeRequest: an update run re-seeds the whole
// updated plan as open items, most of which already exist. Without the owner's request
// in the build instruction, an item whose deliverables exist reads as done and is
// completed with the change never applied.
func TestUpdateBuildInstructionCarriesChangeRequest(t *testing.T) {
	_, pdb := scheduledTestWorker(t, nil)

	fresh := freshBuildInstruction(&Plan{}, "", "", pdb.Reader())
	if strings.Contains(fresh, "already live") {
		t.Errorf("a fresh build must not be framed as a change:\n%s", fresh)
	}
	if !strings.Contains(fresh, "Build the whole project") {
		t.Errorf("fresh build lost its directive:\n%s", fresh)
	}

	update := freshBuildInstruction(&Plan{}, "", "make the header navy and add a newsletter signup", pdb.Reader())
	for _, want := range []string{
		"[mode: build]",
		"make the header navy and add a newsletter signup",
		"apply whatever part of this change touches it before you complete it",
	} {
		if !strings.Contains(update, want) {
			t.Errorf("update build instruction missing %q:\n%s", want, update)
		}
	}
}

// TestReopenBlockedTasks: an item blocked on the owner counts as resolved, so after the
// owner answers it must reopen or the build stops without ever building it.
func TestReopenBlockedTasks(t *testing.T) {
	_, pdb := scheduledTestWorker(t, nil)
	if _, err := AddTasks(pdb, "run1", []Task{
		{ID: "stripe", Title: "Stripe checkout", Status: TaskBlocked},
		{ID: "webgl", Title: "Native shader", Status: TaskBlocked},
		{ID: "home", Title: "Home page", Status: TaskDone},
		{ID: "about", Title: "About page", Status: TaskPending},
	}); err != nil {
		t.Fatalf("add tasks: %v", err)
	}
	// stripe waits on the owner's secret; webgl is shipped without for a platform limit
	// that no answer changes.
	if _, err := pdb.ExecWrite(`UPDATE of_tasks SET blocked_on = ? WHERE id = 'stripe'`, BlockedOnOwner); err != nil {
		t.Fatalf("mark owner block: %v", err)
	}
	n, err := ReopenBlockedTasks(pdb)
	if err != nil || n != 1 {
		t.Fatalf("ReopenBlockedTasks = %d, %v; want 1 reopened", n, err)
	}
	all, err := LoadTasks(pdb.Reader())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := map[string]string{"stripe": TaskPending, "webgl": TaskBlocked, "home": TaskDone, "about": TaskPending}
	for _, task := range all {
		if task.Status != want[task.ID] {
			t.Errorf("task %s status = %s, want %s", task.ID, task.Status, want[task.ID])
		}
	}
}

// TestBuildSeedCarriesOwnerPreferences: preferences stored in LIVE ("British spelling")
// must reach the build that writes the copy, not only PLAN and LIVE.
func TestBuildSeedCarriesOwnerPreferences(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_memory (key, value, category) VALUES ('spelling', 'British English', 'preferences')`); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	w.pdb = pdb
	found := false
	for _, m := range w.buildWholeSeed(&Plan{}, "") {
		if strings.Contains(m.Content, "spelling: British English") {
			found = true
		}
	}
	if !found {
		t.Error("build seed does not carry the owner's stored preferences")
	}
}

// TestChangeRequestOnlyOnUpdateRuns: the change request rides only an update run.
func TestChangeRequestOnlyOnUpdateRuns(t *testing.T) {
	w, _ := scheduledTestWorker(t, nil)
	w.run = &Run{Kind: RunKindUpdate, UpdateDescription: "  add a contact form "}
	if got := w.changeRequest(); got != "add a contact form" {
		t.Errorf("changeRequest() = %q, want the trimmed update description", got)
	}
	w.run = &Run{Kind: RunKindBuild, UpdateDescription: "stale"}
	if got := w.changeRequest(); got != "" {
		t.Errorf("changeRequest() on a non-update run = %q, want empty", got)
	}
	w.run = nil
	if got := w.changeRequest(); got != "" {
		t.Errorf("changeRequest() with no run = %q, want empty", got)
	}
}
