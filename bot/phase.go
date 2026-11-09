package bot

import (
	"openfabrica/db/models"
	"openfabrica/internal/projectphase"
)

// projectPhase derives the owner-facing phase shown on every screen, the same
// value the admin API reports for the project.
func (s *BotService) projectPhase(p *models.Project) projectphase.Phase {
	return projectphase.Load(s.deps.ProjectDBManager, s.engineRunning(p.ID), p)
}

// modeLabel is the phase name a stored mode leads to, for confirming a manual
// change before the worker has announced it.
func (v voice) modeLabel(mode string) string {
	return v.phaseLabel(projectphase.Derive(projectphase.Inputs{Mode: mode, EngineRunning: true}))
}

// phaseLine is a phase as one line of plain text: the label, plus the reason
// when it says something the label does not. A phase that needs the owner
// leaves its reason to the pending-questions line the screens already show.
func (v voice) phaseLine(p projectphase.Phase) string {
	label := v.phaseLabel(p)
	reason := v.reasonLine(p)
	if reason == "" || p.Key == projectphase.KeyNeedsYou {
		return label
	}
	return label + " · " + reason
}
