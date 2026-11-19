package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

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
func (s *Session) Send(ctx context.Context, message string) (*SendResult, error) {
	return s.SendWithImages(ctx, message, nil)
}

// SendWithImages persists the owner's message (plus any image attachments saved to
// disk) and hands the message and images to the engine through the bus.
func (s *Session) SendWithImages(_ context.Context, message string, images []llm.MessageImage) (*SendResult, error) {
	savedContent := message
	if urls := s.saveChatImages(images); len(urls) > 0 {
		if raw, err := json.Marshal(urls); err == nil {
			savedContent = message + "\n\n<!--of-user-images:" + string(raw) + "-->"
		}
	}
	userMsg := llm.Message{Role: llm.RoleUser, Content: savedContent}
	if err := SaveMessage(s.projectDBWriter, s.ID, userMsg); err != nil {
		return nil, fmt.Errorf("chat: save user message: %w", err)
	}
	if s.deps.Bus != nil {
		payload := map[string]any{
			"session_id": s.ID,
			"role":       "user",
			"content":    message,
		}
		if len(images) > 0 {
			payload["images"] = images
		}
		s.deps.Bus.Publish(events.NewEvent(events.EventChatMessage, s.ProjectID, payload))
	}
	return &SendResult{}, nil
}

func (s *Session) saveChatImages(images []llm.MessageImage) []string {
	if len(images) == 0 || s.deps.ProjectDBManager == nil {
		return nil
	}
	dir := filepath.Join(s.deps.ProjectDBManager.ProjectDir(s.ProjectID), "chat_images")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil
	}
	ts := time.Now().UnixNano()
	var urls []string
	for i, img := range images {
		ext := imageExt(img.MIMEType)
		name := fmt.Sprintf("%d-%d.%s", ts, i, ext)
		if err := os.WriteFile(filepath.Join(dir, name), img.Data, 0644); err != nil {
			continue
		}
		urls = append(urls, fmt.Sprintf("/admin/api/projects/%d/chat-images/%s", s.ProjectID, name))
	}
	return urls
}

func imageExt(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return "jpg"
	}
}
