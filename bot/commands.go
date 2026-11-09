package bot

import (
	"fmt"

	"openfabrica/db/models"
	"openfabrica/engine"
)

// applyMode changes the engine mode without sending a confirmation message, so
// callers that re-render a screen (the mode picker) don't also post a stray
// bubble. Returns an error if there is no active project, the mode is invalid,
// or the engine rejects the command.
func (s *BotService) applyMode(state *UserState, mode string) error {
	if state.ActiveProjectID == 0 {
		return fmt.Errorf("no active project")
	}
	if !models.ValidMode(mode) {
		return fmt.Errorf("invalid mode %q", mode)
	}
	// The stored mode is the source of truth for the phase and for sticky
	// pauses, so it is written before the worker is told.
	if err := models.UpdateProjectMode(s.deps.DB.DB, state.ActiveProjectID, mode); err != nil {
		return err
	}
	if s.deps.Engine == nil {
		return nil
	}
	cmd := engine.Command{
		Type:    engine.CommandModeChange,
		Payload: map[string]any{"mode": mode},
	}
	if err := s.deps.Engine.Send(state.ActiveProjectID, cmd); err != nil {
		// No worker yet: starting one reads the stored mode.
		return s.deps.Engine.StartProject(state.ActiveProjectID)
	}
	return nil
}

// setMode applies a mode and confirms it with a message. Used by the quick
// pause/resume actions.
func (s *BotService) setMode(state *UserState, mode string) {
	v := s.voice(state)
	if err := s.applyMode(state, mode); err != nil {
		s.send(state, v.trh("Failed: %s", err))
		return
	}
	s.send(state, v.trh("Phase set to: %s", v.modeLabel(mode)))
}

// resumeMode is the mode a paused project returns to, taken from its phase so
// a resume does not fling a mid-build project to live (skipping the rest of the
// build) or send a finished one back into the pipeline.
func (s *BotService) resumeMode(projectID int) string {
	p, err := models.GetProjectByID(s.deps.DB.DB, projectID)
	if err != nil {
		return models.ModeBuilding
	}
	if to := s.projectPhase(p).ResumeTo; to != "" {
		return to
	}
	return models.ModeBuilding
}

// cmdWake nudges the engine to process now.
func (s *BotService) cmdWake(state *UserState) {
	v := s.voice(state)
	if state.ActiveProjectID == 0 {
		s.send(state, v.tr("No active project."))
		return
	}
	cmd := engine.Command{Type: engine.CommandWake}
	if err := s.deps.Engine.Send(state.ActiveProjectID, cmd); err != nil {
		s.send(state, v.trh("Failed: %s", err))
		return
	}
	s.send(state, v.tr("Wake signal sent."))
}

// doStop halts the engine for the active project.
func (s *BotService) doStop(state *UserState) {
	if state.ActiveProjectID == 0 {
		return
	}
	v := s.voice(state)
	if err := s.deps.Engine.StopProject(state.ActiveProjectID); err != nil {
		s.send(state, v.trh("Failed to stop: %s", err))
		return
	}
	s.send(state, v.tr("Open Fabrica stopped."))
}
