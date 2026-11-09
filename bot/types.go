package bot

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"openfabrica/db"
	"openfabrica/engine"
	"openfabrica/events"
	"openfabrica/security"
)

// Provider abstracts a messaging platform (Telegram, Slack, etc.).
// Each provider handles transport-specific concerns (polling, webhooks, message
// formatting) and delegates business logic to the BotService via OnMessage.
type Provider interface {
	Name() string
	Start(ctx context.Context) error
	Stop() error
	// SendMessage sends a message; returns the platform message ID (0 if unknown).
	SendMessage(chatID string, text string) (int, error)
	SendQuestion(chatID string, question string, labels []string, callbackData []string) error
	// EditMessage replaces the text of a previously sent message. Used to coalesce
	// rapid status updates into a single bubble.
	EditMessage(chatID string, messageID int, text string) error
	// EditMessageKeyboard replaces both the text and inline keyboard of a
	// previously sent message. Used by the menu to navigate between screens
	// in place instead of posting a new bubble per tap.
	EditMessageKeyboard(chatID string, messageID int, text string, replyMarkup map[string]any) error
	// DeleteMessage removes a previously sent message. Used to scrub a secret the
	// user pasted into the chat once it's safely stored.
	DeleteMessage(chatID string, messageID int) error
	// SendChatAction shows a transient status (e.g. "typing") so the user knows
	// work is in flight. Best-effort: failures are non-fatal.
	SendChatAction(chatID string, action string) error
	// SendCard posts a message with an inline keyboard and returns its id. When
	// silent is true it does not notify the recipient: used for the live status
	// card, whose (re)posts should not buzz the phone.
	SendCard(chatID string, text string, replyMarkup map[string]any, silent bool) (int, error)
	// SendDocument uploads a file (used for full LLM call records).
	SendDocument(chatID, filename string, data []byte, caption string) error
}

// OnMessageFunc is the callback providers call when a message arrives.
// messageID is the platform message id (used to scrub a pasted secret).
type OnMessageFunc func(provider, chatID, userID string, messageID int, text string)

// OnCallbackFunc is the callback providers call when an inline button is pressed.
// messageID is the id of the message the button belongs to, so the handler can
// edit that same message in place. Returning a non-empty string surfaces a toast.
type OnCallbackFunc func(provider, chatID, userID string, messageID int, callbackData string) string

// BotDeps holds dependencies injected from main.go.
type BotDeps struct {
	DB               *db.DB
	ProjectDBManager *db.ProjectDBManager
	Engine           *engine.Engine
	Bus              *events.Bus
	Encryptor        *security.Encryptor
	Logger           *slog.Logger
	// AdminBaseURL is the externally reachable admin UI origin (e.g.
	// "https://host:port"), used to build "Open in admin" deep links. Empty
	// disables deep links.
	AdminBaseURL string
	// TLSDisabled mirrors the server setting; when true no project is served over
	// HTTPS, so the bot does not offer public links.
	TLSDisabled bool
}

// UserState tracks which project a bot user is currently interacting with.
type UserState struct {
	Provider        string
	ChatID          string
	UserID          string
	ActiveProjectID int
	Muted           bool
	PendingAction   string
	PendingActionAt time.Time
	PendingData     map[string]string
	// verb is how much the bot narrates: quiet, normal (live card) or verbose
	// (live card plus a line per LLM call). Read from event goroutines, so atomic.
	verb atomic.Int32
	// LiveCardMessageID is this person's live status card. Stale is set when other
	// messages have been posted below it, so the next update reposts it at the
	// bottom (guarded separately from Mu: it is flipped by broadcast paths).
	LiveCardMessageID int
	LiveCardText      string
	LiveCardAt        time.Time
	LiveCardStale     atomic.Bool
	// LastStatusMessageID is the most recent coalesce-eligible status message
	// sent to this user; non-zero means we can edit it instead of posting anew.
	LastStatusMessageID int
	LastStatusAt        time.Time
	LastStatusText      string
	// Mu serializes HandleIncoming / HandleCallback for a single user, preventing
	// races on PendingAction and friends when callbacks and messages overlap.
	Mu sync.Mutex
}
