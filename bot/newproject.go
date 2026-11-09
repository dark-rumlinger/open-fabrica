package bot

import (
	"fmt"
	"time"

	"openfabrica/db/models"
)

// maxIdeaRunes bounds how much of a first message is kept as a project brief.
const maxIdeaRunes = 4000

// offerProjectForMessage handles a plain message from someone who has no active
// project, which is the natural first thing to type. One project: use it. Otherwise
// offer to start a new project from the message, or to pick an existing one.
func (s *BotService) offerProjectForMessage(state *UserState, text string) {
	projects, _ := models.ListProjects(s.deps.DB.DB)
	if len(projects) == 1 {
		s.switchProject(state, projects[0].ID)
		s.handleChat(state, text)
		return
	}

	idea := truncateRunes(text, maxIdeaRunes)
	state.PendingAction = "confirm_new"
	state.PendingActionAt = time.Now()
	state.PendingData = map[string]string{"idea": idea}
	SaveSession(s.deps.DB, state)

	v := s.voice(state)
	labels := []string{v.tr("🚀 Start a new project")}
	cbs := []string{"new:go"}
	body := v.tr("You haven't opened a project yet. Start a new one from this?")
	if len(projects) > 0 {
		labels = append(labels, v.tr("📋 Pick a project"))
		cbs = append(cbs, "nav:projects")
		body = v.tr("You haven't opened a project yet. Start a new one from this, or pick an existing one?")
	}
	labels = append(labels, v.tr("✖ Not now"))
	cbs = append(cbs, "nav:menu")
	s.showScreen(state, 0, screen{
		text:   fmt.Sprintf("%s\n\n<blockquote>%s</blockquote>", body, escapeHTML(truncate(idea, 400))),
		labels: labels,
		cbs:    cbs,
	})
}
