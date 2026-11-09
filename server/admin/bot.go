package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/bot"
	"openfabrica/events"
	"openfabrica/internal/qrcode"
)

// BotHandler exposes admin endpoints related to chat-bot providers (Telegram, etc.).
type BotHandler struct {
	deps *Deps
}

// TestTelegram verifies a Telegram bot token by calling the getMe endpoint
// and returns the bot's identity so the admin can confirm the right bot is
// wired up. If the request body omits the token, the saved setting is used,
// which lets users hit "Test" after a save without re-pasting.
func (h *BotHandler) TestTelegram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	// Body is optional; ignore decode errors entirely.
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBodySize)
	_ = json.NewDecoder(r.Body).Decode(&body)

	token := strings.TrimSpace(body.Token)
	if token == "" || token == bot.TokenMask {
		token = bot.LoadBotToken(h.deps.DB, h.deps.Encryptor)
	}
	if token == "" {
		writeError(w, http.StatusBadRequest, "No bot token provided or saved.")
		return
	}

	info, err := telegramGetMe(r.Context(), token)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"id":         info.ID,
		"username":   info.Username,
		"first_name": info.FirstName,
	})
}

// --- Setup flow: status, token, invite links, people ---

// botService returns the running bot service, or answers 503 when the server
// was built without one (tests, minimal setups).
func (h *BotHandler) botService(w http.ResponseWriter) *bot.BotService {
	if h.deps.Bot == nil {
		writeError(w, http.StatusServiceUnavailable, "The messaging bot is not available.")
		return nil
	}
	return h.deps.Bot
}

// Status reports whether the bot is connected and who is allowed to use it.
func (h *BotHandler) Status(w http.ResponseWriter, r *http.Request) {
	svc := h.botService(w)
	if svc == nil {
		return
	}
	writeJSON(w, http.StatusOK, svc.Status())
}

// SetToken validates a bot token with Telegram, stores it encrypted, and
// hot-reloads the bot so it starts polling right away.
func (h *BotHandler) SetToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" || token == bot.TokenMask {
		writeError(w, http.StatusBadRequest, "Paste the token BotFather gave you.")
		return
	}

	info, err := telegramGetMe(r.Context(), token)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := bot.SaveBotToken(h.deps.DB, h.deps.Encryptor, token); err != nil {
		h.deps.serverError(w, "failed to save bot token", err)
		return
	}
	h.deps.Bus.Publish(events.NewEvent(events.EventSettingsChanged, 0, nil))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"username":   info.Username,
		"first_name": info.FirstName,
		"deep_link":  bot.BotLink(info.Username, ""),
	})
}

// Disconnect removes the bot token and stops the bot. People stay on the
// allowlist so reconnecting later needs no re-pairing.
func (h *BotHandler) Disconnect(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.DB.SetSetting(bot.TokenSettingKey, ""); err != nil {
		h.deps.serverError(w, "failed to clear bot token", err)
		return
	}
	h.deps.Bus.Publish(events.NewEvent(events.EventSettingsChanged, 0, nil))
	writeStatus(w, "disconnected")
}

// Pair issues a single-use invite link for one person to open in Telegram.
func (h *BotHandler) Pair(w http.ResponseWriter, r *http.Request) {
	svc := h.botService(w)
	if svc == nil {
		return
	}
	username := svc.Status().Username
	if username == "" {
		token := bot.LoadBotToken(h.deps.DB, h.deps.Encryptor)
		if token == "" {
			writeError(w, http.StatusBadRequest, "Save a bot token first.")
			return
		}
		info, err := telegramGetMe(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusBadGateway, "Telegram did not accept the saved token: "+err.Error())
			return
		}
		username = info.Username
	}

	code, expires, err := svc.NewPairingCode()
	if err != nil {
		h.deps.serverError(w, "failed to create invite", err)
		return
	}
	link := bot.BotLink(username, code)
	writeJSON(w, http.StatusOK, map[string]any{
		"code":       code,
		"username":   username,
		"deep_link":  link,
		"qr":         qrcode.DataURI(link),
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

// RemoveUser revokes one person's access.
func (h *BotHandler) RemoveUser(w http.ResponseWriter, r *http.Request) {
	svc := h.botService(w)
	if svc == nil {
		return
	}
	if err := svc.RemoveUser(chi.URLParam(r, "userID")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, svc.Status())
}

// SendTest sends a hello message to one allowed person.
func (h *BotHandler) SendTest(w http.ResponseWriter, r *http.Request) {
	svc := h.botService(w)
	if svc == nil {
		return
	}
	var body struct {
		UserID string `json:"user_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	allowed := false
	for _, u := range svc.Users() {
		if u.ID == body.UserID {
			allowed = true
		}
	}
	if !allowed {
		writeError(w, http.StatusBadRequest, "That person is not connected yet.")
		return
	}
	if err := svc.SendTest(body.UserID); err != nil {
		writeError(w, http.StatusBadGateway, "Could not deliver the message: "+err.Error())
		return
	}
	writeStatus(w, "sent")
}

type telegramBotInfo struct {
	ID        int64
	Username  string
	FirstName string
}

// telegramGetMe calls Telegram's getMe API with a short timeout. Returns a
// trimmed error message on non-OK responses so the UI can show it as-is.
func telegramGetMe(ctx context.Context, token string) (*telegramBotInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.telegram.org/bot"+token+"/getMe",
		bytes.NewReader([]byte("{}")))
	if err != nil {
		// Never surface the raw error: it (and client.Do's *url.Error below)
		// embeds the request URL, which contains the bot token. TestTelegram
		// echoes this string straight to the client, so a transport failure
		// would leak the token into responses, logs, and proxies.
		return nil, errString("failed to build Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errString("failed to contact Telegram")
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var parsed struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			ID        int64  `json:"id"`
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	if !parsed.OK {
		desc := strings.TrimSpace(parsed.Description)
		if desc == "" {
			desc = "Telegram rejected the token (HTTP " + http.StatusText(resp.StatusCode) + ")"
		}
		return nil, errString(desc)
	}
	return &telegramBotInfo{
		ID:        parsed.Result.ID,
		Username:  parsed.Result.Username,
		FirstName: parsed.Result.FirstName,
	}, nil
}

// errString is a tiny stringly-typed error used to forward Telegram's own
// description to the API client without dragging in fmt.
type errString string

func (e errString) Error() string { return string(e) }
