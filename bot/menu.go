/**
 * Button-driven menu for the bot: a small set of screens (main menu, project
 * list, per-project chat header, project settings) that navigate in place by
 * editing the same message, mirroring how the admin web UI moves between pages.
 * All screens are rendered as inline keyboards; free-text in the DM stays the
 * live chat with the active project.
 */

package bot

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"openfabrica/db/models"
	"openfabrica/engine"
	"openfabrica/events"
)

// screen is a renderable menu view: body text plus parallel button label/callback
// slices (two buttons per row, see buildInlineKeyboard).
type screen struct {
	text   string
	labels []string
	cbs    []string
}

// showScreen renders a screen. When messageID is non-zero it edits that message
// in place (the SPA-in-a-message navigation); a "not modified" reply is a benign
// no-op. Any other edit failure (message too old, deleted) falls back to posting
// a fresh message, and a keyboard failure falls back to plain text.
func (s *BotService) showScreen(state *UserState, messageID int, sc screen) {
	p, ok := s.provider(state.Provider)
	if !ok {
		return
	}
	if messageID != 0 {
		if kb, err := buildInlineKeyboard(sc.labels, sc.cbs); err == nil {
			editErr := p.EditMessageKeyboard(state.ChatID, messageID, sc.text, kb)
			if editErr == nil || isNotModifiedErr(editErr) {
				return
			}
			// Edit failed (stale/deleted message): fall through to a fresh send.
		}
	}
	if err := p.SendQuestion(state.ChatID, sc.text, sc.labels, sc.cbs); err != nil {
		s.send(state, sc.text)
	}
}

// --- Screen builders ---

func (s *BotService) mainMenuScreen(v voice) screen {
	return screen{
		text:   v.tr("<b>Open Fabrica</b>\nBuild and manage your projects from here.\n\nPick a project to continue, or start a new one."),
		labels: []string{v.tr("📋 My Projects"), v.tr("➕ New Project"), v.tr("🔔 Alerts"), v.tr("❔ Help")},
		cbs:    []string{"nav:projects", "nav:new", "nav:alerts", "nav:help"},
	}
}

// projectsScreen lists projects as buttons, paginated. Tapping one opens its
// chat (project:<id>). Falls back to a friendly empty state.
func (s *BotService) projectsScreen(state *UserState, page int) screen {
	v := s.voice(state)
	projects, err := models.ListProjects(s.deps.DB.DB)
	if err != nil || len(projects) == 0 {
		return screen{
			text:   v.tr("You have no projects yet."),
			labels: []string{v.tr("➕ New Project"), v.tr("☰ Menu")},
			cbs:    []string{"nav:new", "nav:menu"},
		}
	}

	total := len(projects)
	start := page * projectsPerPage
	if start >= total {
		page, start = 0, 0
	}
	end := start + projectsPerPage
	if end > total {
		end = total
	}

	var labels, cbs []string
	for _, project := range projects[start:end] {
		// Button text isn't HTML-parsed, so the raw name is safe here.
		label := project.Name
		if project.ID == state.ActiveProjectID {
			label = "✓ " + label
		}
		labels = append(labels, label)
		cbs = append(cbs, fmt.Sprintf("project:%d", project.ID))
	}
	if start > 0 {
		labels = append(labels, v.tr("◀ Prev"))
		cbs = append(cbs, fmt.Sprintf("projpage:%d", page-1))
	}
	if end < total {
		labels = append(labels, v.tr("Next ▶"))
		cbs = append(cbs, fmt.Sprintf("projpage:%d", page+1))
	}
	labels = append(labels, v.tr("➕ New"), v.tr("☰ Menu"))
	cbs = append(cbs, "nav:new", "nav:menu")

	// Numeric-only interpolation: the arguments are numbers, which trh leaves alone.
	text := v.trh("<b>Your projects</b> (%d-%d of %d)\nTap one to open its chat.", start+1, end, total)
	return screen{text: text, labels: labels, cbs: cbs}
}

// projectChatScreen is the per-project landing view: a compact header telling the
// user they're now chatting with the project, plus navigation. Free text they
// send from here goes straight to the engine.
func (s *BotService) projectChatScreen(state *UserState) screen {
	return s.dashboardScreen(state)
}

// settingsScreen mirrors the useful subset of the admin project settings page:
// read-only info plus one-tap actions.
func (s *BotService) settingsScreen(state *UserState) screen {
	v := s.voice(state)
	project, err := models.GetProjectByID(s.deps.DB.DB, state.ActiveProjectID)
	if err != nil {
		return s.mainMenuScreen(v)
	}
	running := s.engineRunning(state.ActiveProjectID)
	published := project.Published
	publishedWord := v.tr("no")
	if published {
		publishedWord = v.tr("yes")
	}
	domain := "-"
	if project.Domain != nil && *project.Domain != "" {
		domain = *project.Domain
	}

	text := v.trh(
		"<b>%s - Settings</b>\n\nPhase: %s\nPublished: %s\nModel: %s\nDomain: %s",
		project.Name, v.phaseLine(s.projectPhase(project)), publishedWord, s.modelDisplayName(project.LLMModelID), domain,
	)

	startStopLabel, startStopCB := v.tr("▶ Start engine"), "nav:start"
	if running {
		startStopLabel, startStopCB = v.tr("⏹ Shut down engine"), "nav:stop"
	}
	publishLabel, publishCB := v.tr("🌐 Publish"), "nav:publish"
	if published {
		publishLabel, publishCB = v.tr("🌐 Unpublish"), "nav:unpublish"
	}

	return screen{
		text:   text,
		labels: []string{startStopLabel, v.tr("🔀 Change phase"), publishLabel, v.tr("✏️ Rename"), v.tr("🗑 Delete"), v.tr("◀ Back")},
		cbs:    []string{startStopCB, "nav:phase", publishCB, "nav:rename", "nav:delete", "nav:chat"},
	}
}

// phaseScreen is the phase picker: a manual override that sets the stored mode a
// phase is derived from, with a Back button to the settings screen.
func (s *BotService) phaseScreen(v voice) screen {
	return screen{
		text:   v.tr("Choose a phase:"),
		labels: []string{v.tr("Building"), v.tr("Live"), v.tr("Paused"), v.tr("◀ Back")},
		cbs:    []string{"phase:building", "phase:live", "phase:paused", "nav:settings"},
	}
}

// --- Navigation dispatch ---

// handleNav routes a nav:<screen> callback. messageID is the tapped message, so
// each screen can edit in place. Returns optional toast text.
func (s *BotService) handleNav(state *UserState, messageID int, screenName string) string {
	v := s.voice(state)
	switch screenName {
	case "menu":
		s.showScreen(state, messageID, s.dashboardScreen(state))

	case "status", "questions", "site":
		if state.ActiveProjectID == 0 {
			s.showScreen(state, messageID, s.mainMenuScreen(v))
			return v.tr("Pick a project first")
		}
		switch screenName {
		case "status":
			s.showScreen(state, messageID, s.statusScreen(state))
		case "questions":
			s.showScreen(state, messageID, s.questionsScreen(state))
		default:
			s.showScreen(state, messageID, s.siteScreen(state))
		}

	case "help":
		s.showScreen(state, messageID, s.helpScreen(v))

	case "selfdestruct":
		s.showScreen(state, messageID, s.selfDestructWarnScreen(v))

	case "selfdestruct2":
		s.showScreen(state, messageID, s.selfDestructFinalScreen(state))

	case "projects":
		s.showScreen(state, messageID, s.projectsScreen(state, 0))

	case "new":
		state.PendingAction = "awaiting_description"
		state.PendingActionAt = time.Now()
		SaveSession(s.deps.DB, state)
		s.showScreen(state, messageID, screen{
			text:   v.tr("➕ <b>New project</b>\n\nDescribe what you want to build:"),
			labels: []string{v.tr("◀ Cancel")},
			cbs:    []string{"nav:menu"},
		})

	case "log":
		if state.ActiveProjectID == 0 {
			s.showScreen(state, messageID, s.mainMenuScreen(v))
			return v.tr("Pick a project first")
		}
		s.showScreen(state, messageID, s.logListScreen(state, 0))

	case "alerts":
		s.showScreen(state, messageID, s.alertsScreen(state))

	case "chat":
		if state.ActiveProjectID == 0 {
			s.showScreen(state, messageID, s.mainMenuScreen(v))
			return v.tr("Pick a project first")
		}
		s.showScreen(state, messageID, s.projectChatScreen(state))

	case "settings":
		if state.ActiveProjectID == 0 {
			s.showScreen(state, messageID, s.mainMenuScreen(v))
			return v.tr("Pick a project first")
		}
		s.showScreen(state, messageID, s.settingsScreen(state))

	case "phase":
		if state.ActiveProjectID == 0 {
			return v.tr("Pick a project first")
		}
		s.showScreen(state, messageID, s.phaseScreen(v))

	case "start":
		if state.ActiveProjectID == 0 {
			return v.tr("Pick a project first")
		}
		if s.deps.Engine == nil {
			return v.tr("Open Fabrica unavailable")
		}
		if err := s.deps.Engine.StartProject(state.ActiveProjectID); err != nil {
			return v.tr("Couldn't start")
		}
		s.showScreen(state, messageID, s.settingsScreen(state))
		return v.tr("Starting…")

	case "stop":
		if state.ActiveProjectID == 0 {
			return v.tr("Pick a project first")
		}
		if s.deps.Engine == nil {
			return v.tr("Open Fabrica unavailable")
		}
		if err := s.deps.Engine.StopProject(state.ActiveProjectID); err != nil {
			return v.tr("Couldn't stop")
		}
		s.showScreen(state, messageID, s.settingsScreen(state))
		return v.tr("Stopping…")

	case "publish", "unpublish":
		if state.ActiveProjectID == 0 {
			return v.tr("Pick a project first")
		}
		publish := screenName == "publish"
		if err := models.SetProjectPublished(s.deps.DB.DB, state.ActiveProjectID, publish); err != nil {
			return v.tr("Update failed")
		}
		if project, err := models.GetProjectByID(s.deps.DB.DB, state.ActiveProjectID); err == nil {
			// Announce so the TLS proxy reloads (unpublished projects stop serving).
			s.deps.Bus.Publish(events.NewEvent(events.EventProjectUpdated, state.ActiveProjectID, map[string]any{
				"name":      project.Name,
				"published": project.Published,
			}))
		}
		s.showScreen(state, messageID, s.settingsScreen(state))
		return v.tr("Updated")

	case "rename":
		if state.ActiveProjectID == 0 {
			return v.tr("Pick a project first")
		}
		state.PendingAction = "awaiting_rename"
		state.PendingActionAt = time.Now()
		SaveSession(s.deps.DB, state)
		s.showScreen(state, messageID, screen{
			text:   v.tr("✏️ Send the new project name:"),
			labels: []string{v.tr("◀ Cancel")},
			cbs:    []string{"nav:settings"},
		})

	case "delete":
		if state.ActiveProjectID == 0 {
			return v.tr("Pick a project first")
		}
		name := ""
		if project, err := models.GetProjectByID(s.deps.DB.DB, state.ActiveProjectID); err == nil {
			name = project.Name
		}
		s.showScreen(state, messageID, screen{
			text:   v.trh("🗑 Delete <b>%s</b>?\nThis permanently removes its pages, data, and files. This cannot be undone.", name),
			labels: []string{v.tr("Yes, delete"), v.tr("◀ Cancel")},
			// Bound to this project: a tap after switching projects must not delete
			// whichever one is active by then.
			cbs: []string{"cfm:delete:" + strconv.Itoa(state.ActiveProjectID), "nav:settings"},
		})
	}
	return ""
}

// --- Mutations backing the settings actions ---

// renameProject applies a new name (from the awaiting_rename flow) and re-renders
// settings.
func (s *BotService) renameProject(state *UserState, name string) {
	v := s.voice(state)
	name = strings.TrimSpace(name)
	if name == "" {
		s.send(state, v.tr("Name can't be empty. Tap Rename again to retry."))
		return
	}
	name = truncateRunes(name, 60)
	if err := models.UpdateProjectName(s.deps.DB.DB, state.ActiveProjectID, name); err != nil {
		s.send(state, v.trh("Rename failed: %s", err))
		return
	}
	s.deps.Bus.Publish(events.NewEvent(events.EventProjectUpdated, state.ActiveProjectID, map[string]any{
		"name": name,
	}))
	// No messageID in a text flow, so post the refreshed settings as a fresh view.
	s.showScreen(state, 0, s.settingsScreen(state))
}

// deleteProject stops the engine and tears down the active project, mirroring the
// admin delete path (engine stop → serialized row delete → per-project DB delete
// → event). Clears the user's active project on success.
func (s *BotService) deleteProject(state *UserState) error {
	pid := state.ActiveProjectID
	if pid == 0 {
		return fmt.Errorf("no active project")
	}
	if s.deps.Engine != nil {
		// Same split as the admin delete path: an idle project has no worker to
		// stop, but a worker that misses the deadline outlives the project row
		// and database this is about to remove.
		switch err := s.deps.Engine.StopProject(pid); {
		case err == nil:
		case errors.Is(err, engine.ErrNoWorker):
			s.logger.Debug("no engine worker to stop before delete", "project", pid)
		default:
			s.logger.Error("engine worker did not stop before delete; it will keep running against a deleted project until it returns",
				"project", pid, "error", err)
		}
	}
	if _, err := s.deps.DB.ExecWrite("DELETE FROM projects WHERE id = ?", pid); err != nil {
		return err
	}
	if err := s.deps.ProjectDBManager.Delete(pid); err != nil {
		// Row already gone; log but don't fail the delete.
		s.logger.Error("failed to delete project db", "project", pid, "error", err)
	}
	s.deps.Bus.Publish(events.NewEvent(events.EventProjectDeleted, pid, nil))

	state.ActiveProjectID = 0
	state.LastStatusMessageID = 0
	state.LastStatusText = ""
	SaveSession(s.deps.DB, state)
	s.forgetProject(pid, state)
	return nil
}

// forgetProject clears a deleted project from every other user's session, so
// nobody is left looking at a project that no longer exists.
func (s *BotService) forgetProject(pid int, except *UserState) {
	s.mu.RLock()
	var affected []*UserState
	for _, st := range s.users {
		if st != except && st.ActiveProjectID == pid {
			affected = append(affected, st)
		}
	}
	s.mu.RUnlock()
	for _, st := range affected {
		st.Mu.Lock()
		if st.ActiveProjectID == pid {
			st.ActiveProjectID = 0
			st.LastStatusMessageID = 0
			st.LastStatusText = ""
			SaveSession(s.deps.DB, st)
		}
		st.Mu.Unlock()
	}
}

// engineRunning reports whether the engine worker for a project is live,
// tolerating a nil engine (e.g. in tests).
func (s *BotService) engineRunning(projectID int) bool {
	return s.deps.Engine != nil && s.deps.Engine.IsRunning(projectID)
}

// modelDisplayName resolves a model's human-readable name; returns "-" if unknown.
func (s *BotService) modelDisplayName(modelID int) string {
	var name string
	err := s.deps.DB.DB.QueryRow("SELECT display_name FROM llm_models WHERE id = ?", modelID).Scan(&name)
	if err != nil || name == "" {
		return "-"
	}
	return name
}
