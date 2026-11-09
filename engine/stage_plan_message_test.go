package engine

import (
	"strings"
	"testing"
)

// TestUpdatePlanMessageCarriesOwnerAnswers pins that an update re-plan sees the
// owner's answers. The update branch used to return before appending them, so a
// planner that paused on a question during a change request re-ran without the
// answer it asked for.
func TestUpdatePlanMessageCarriesOwnerAnswers(t *testing.T) {
	r := &Run{UpdateDescription: "add a newsletter signup", PlanJSON: `{"goal":"x","tasks":[]}`}
	msg := buildPlanUserMessage("a bakery site", "Use Mailchimp-style double opt-in.", r, nil)
	for _, want := range []string{
		"add a newsletter signup",
		"Owner's answers to your prior questions:",
		"Use Mailchimp-style double opt-in.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("update plan message missing %q:\n%s", want, msg)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(msg), "Produce the updated full plan JSON now.") {
		t.Errorf("update plan message must end on the ask:\n%s", msg)
	}
}

// TestLayoutPathIsNotARoute: the plan frame says `/layout` normalizes to `default`, so
// an app-shaped plan whose shell task owns `/layout` must not be told it has extra
// page routes.
func TestLayoutPathIsNotARoute(t *testing.T) {
	if got := normPlanPath("/Layout"); got != "default" {
		t.Errorf("normPlanPath(/Layout) = %q, want default", got)
	}
	p := &Plan{
		Decisions: []PlanDecision{{Topic: "project shape", Choice: "app"}},
		Tasks: []PlanTaskSpec{
			{Title: "Shell", Paths: []string{"/layout"}},
			{Title: "App", Paths: []string{"/", "/files/app.js"}},
			{Title: "Settings", Paths: []string{"/settings"}},
		},
	}
	if msg := appShapeRouteSprawlProblem(p); msg != "" {
		t.Errorf("one real extra route plus /layout is not sprawl, got: %s", msg)
	}
}

// TestUpdatePlanMessageCarriesSecurityModel pins that an encrypted change request
// gets the same security-model stub a fresh encrypted brief does.
func TestUpdatePlanMessageCarriesSecurityModel(t *testing.T) {
	r := &Run{UpdateDescription: "make the chat end-to-end encrypted", PlanJSON: `{"goal":"x","tasks":[]}`}
	msg := buildPlanUserMessage("a chat app", "", r, nil)
	if !strings.Contains(msg, "Security model for a private / encrypted realtime brief") {
		t.Errorf("encrypted change request lost the security-model stub:\n%s", msg)
	}
	if strings.Contains(msg, "Owner's answers") {
		t.Errorf("no answers were given, so no answers block should render:\n%s", msg)
	}
}
