package chat

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/providers/llm"
)

// One agent answers the owner: the engine. A chat message is persisted here and
// published as EventChatMessage; the engine worker picks it up (a LIVE-stage session
// on a live, paused, idle or errored project, or a note injected into the running
// build session at its next turn) and publishes the reply on the same timeline. There
// is deliberately no second tool loop in this package: a parallel agent had its own,
// thinner prompt and could edit files while the build session held them.

const historyLimit = 50

// SessionDeps bundles the external dependencies needed by a chat session.
type SessionDeps struct {
	DB               *sql.DB // global database (users, providers, projects)
	ProjectDBManager *db.ProjectDBManager
	Bus              *events.Bus
	Logger           *slog.Logger
}

// Session represents an ongoing chat conversation for a specific project.
type Session struct {
	ID              string
	ProjectID       int
	projectDBWriter *sql.DB // per-project write pool
	deps            SessionDeps
}

// NewSession creates a new Session bound to the given project.
func NewSession(id string, projectID int, deps SessionDeps) (*Session, error) {
	projectDB, err := deps.ProjectDBManager.Open(projectID)
	if err != nil {
		return nil, fmt.Errorf("chat: open project db %d: %w", projectID, err)
	}
	return &Session{
		ID:              id,
		ProjectID:       projectID,
		projectDBWriter: projectDB.Writer(),
		deps:            deps,
	}, nil
}

// ToolActivity records one tool execution within a Send call so the frontend
// can render its card after the fact, without needing to stream the call.
type ToolActivity struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// SendResult is the synchronous result of Session.Send. The engine's reply arrives
// asynchronously on the project timeline, so Send itself carries no text.
type SendResult struct {
	Text  string         `json:"text"`
	Tools []ToolActivity `json:"tools"`
}

// Send persists the owner's message and hands it to the engine through the bus.
func (s *Session) Send(_ context.Context, message string) (*SendResult, error) {
	userMsg := llm.Message{Role: llm.RoleUser, Content: message}
	if err := SaveMessage(s.projectDBWriter, s.ID, userMsg); err != nil {
		return nil, fmt.Errorf("chat: save user message: %w", err)
	}
	if s.deps.Bus != nil {
		s.deps.Bus.Publish(events.NewEvent(events.EventChatMessage, s.ProjectID, map[string]any{
			"session_id": s.ID,
			"role":       "user",
			"content":    message,
		}))
	}
	return &SendResult{}, nil
}
