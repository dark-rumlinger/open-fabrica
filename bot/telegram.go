package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"openfabrica/internal/msgcat"
	"openfabrica/internal/replylang"
	"openfabrica/internal/safego"
	"openfabrica/internal/stringutil"
)

// Telegram limits: 4096 chars per message, 64 bytes per callback_data.
const (
	tgMaxMessageLen  = 4000
	tgMaxCallbackLen = 64
)

// OffsetStore persists the Telegram long-poll offset so a restart resumes from
// the last confirmed update instead of replaying the backlog Telegram still
// holds (~24h). Implemented by BotService over the settings table.
type OffsetStore interface {
	LoadOffset() int
	SaveOffset(offset int)
}

// TelegramProvider implements the Provider interface for Telegram Bot API.
// Uses long-polling (no webhook server needed).
type TelegramProvider struct {
	token      string
	baseURL    string
	client     *http.Client
	onMessage  OnMessageFunc
	onCallback OnCallbackFunc
	offsets    OffsetStore

	lifeMu  sync.Mutex // guards cancel/done: written in Start's goroutine, read in Stop
	cancel  context.CancelFunc
	done    chan struct{} // closed when the poll loop has fully exited
	stopped atomic.Bool   // set by Stop; guards the pre-setup start race

	warnedMu    sync.Mutex
	warnedChats map[int64]bool // group chats already told "DMs only" (anti-loop)

	infoMu sync.RWMutex
	info   tgBotInfo // bot identity from the startup getMe; zero until connected

	// onSeen, when set, is told who is talking before their message is handled,
	// so display names can be remembered for the admin user list.
	onSeen func(userID, name string)

	// onLanguage, when set, is told the language each sender's Telegram app uses,
	// so the bot can answer in it.
	onLanguage func(userID, language string)

	// onUnsupported is told when someone sends something that is not text (a
	// photo, voice note, file), so they get an answer instead of silence.
	onUnsupported func(chatID, userID string)

	logger *slog.Logger
}

// NewTelegramProvider creates a Telegram bot provider. offsets may be nil, in
// which case the offset resets to 0 on each start (safe, but replays whatever
// backlog Telegram still holds).
func NewTelegramProvider(token string, onMessage OnMessageFunc, onCallback OnCallbackFunc, offsets OffsetStore) *TelegramProvider {
	return &TelegramProvider{
		token:       token,
		baseURL:     "https://api.telegram.org/bot" + token,
		client:      &http.Client{Timeout: 35 * time.Second},
		onMessage:   onMessage,
		onCallback:  onCallback,
		offsets:     offsets,
		warnedChats: make(map[int64]bool),
		logger:      slog.Default().With("component", "telegram"),
	}
}

func (t *TelegramProvider) Name() string { return "telegram" }

// SetUserSeen registers a callback invoked with each sender's ID and display name.
func (t *TelegramProvider) SetUserSeen(fn func(userID, name string)) { t.onSeen = fn }

// SetLanguageSeen registers a callback invoked with each sender's ID and the
// language code their Telegram app reports.
func (t *TelegramProvider) SetLanguageSeen(fn func(userID, language string)) { t.onLanguage = fn }

// SetUnsupported registers a callback for messages that carry no text.
func (t *TelegramProvider) SetUnsupported(fn func(chatID, userID string)) { t.onUnsupported = fn }

// BotInfo returns the bot's identity (username and display name). Both are empty
// until the startup token check succeeds.
func (t *TelegramProvider) BotInfo() (username, name string) {
	t.infoMu.RLock()
	defer t.infoMu.RUnlock()
	return t.info.Username, t.info.FirstName
}

// Start begins long-polling for updates. It blocks until ctx is cancelled and
// signals completion via t.done so Stop can wait out the in-flight poll.
func (t *TelegramProvider) Start(ctx context.Context) error {
	// Publish cancel/done under lifeMu so Stop (a different goroutine) never races
	// the write. Folding the stopped check into the locked section also closes the
	// Stop-before-Start window: a rotated-out provider can't keep a second poller
	// alive.
	t.lifeMu.Lock()
	if t.stopped.Load() {
		t.lifeMu.Unlock()
		return context.Canceled
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	t.cancel, t.done = cancel, done
	t.lifeMu.Unlock()
	defer close(done)

	// Startup self-test: validate the token up front so a bad token surfaces as
	// one clear error instead of an endless getUpdates backoff loop.
	info, err := t.getMeWithRetry(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		setBotHealth(false, "token check failed: "+redactToken(err.Error(), t.token))
		t.logger.Error("telegram token check failed; bot disabled until reconfigured", "error", redactErr(err, t.token))
		return err
	}
	t.infoMu.Lock()
	t.info = *info
	t.infoMu.Unlock()
	setBotHealth(true, "")
	t.logger.Info("telegram bot connected", "username", info.Username)

	// Clear any stale webhook so long-polling isn't rejected with 409 Conflict
	// (a token that still has a webhook set can't use getUpdates). Best effort;
	// keep the backlog (drop_pending_updates:false) since we resume via the offset.
	if _, err := t.apiCallCtx(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}); err != nil {
		t.logger.Warn("telegram deleteWebhook failed (continuing)", "error", redactErr(err, t.token))
	}

	t.setCommands()
	t.setProfile()
	t.logger.Info("telegram bot started, polling for updates")

	offset := 0
	if t.offsets != nil {
		offset = t.offsets.LoadOffset()
	}
	backoff := time.Second
	inBackoff := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		updates, err := t.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Honor Telegram's requested pause on a 429; otherwise fall back to our
			// own exponential backoff.
			delay := backoff
			var apiErr *tgAPIError
			if errors.As(err, &apiErr) && apiErr.retryAfter > 0 {
				delay = apiErr.retryAfter
			}
			if !inBackoff {
				detail := redactErr(err, t.token)
				if strings.Contains(err.Error(), "409") {
					detail += " (another instance is polling this bot token)"
				}
				t.logger.Warn("telegram getUpdates failed, entering backoff", "error", detail, "delay", delay)
				inBackoff = true
			}
			setBotBackoff(delay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			backoff = nextBackoff(backoff)
			continue
		}

		if inBackoff {
			t.logger.Info("telegram getUpdates recovered")
			inBackoff = false
		}
		backoff = time.Second
		setBotBackoff(0)
		markBotPoll()

		offset = t.processUpdates(updates, offset)
	}
}

// processUpdates dispatches each update and then persists the poll offset, in
// that order. Returns the offset to poll from next.
//
// Ordering matters: saving AFTER dispatch means a process crash mid-handling
// replays the update on restart (at-least-once) instead of dropping it. The
// answer/form flows carry idempotency guards for this. Each dispatch runs under
// safego.Run (synchronous panic recovery) so one poison update can't kill the
// poll loop; because a recovered panic still advances the offset, a persistently
// crashing update is skipped rather than replayed into a crash loop.
func (t *TelegramProvider) processUpdates(updates []tgUpdate, offset int) int {
	for _, update := range updates {
		safego.Run("telegram-dispatch", func() { t.dispatch(update) })
		offset = update.UpdateID + 1
		if t.offsets != nil {
			t.offsets.SaveOffset(offset)
		}
	}
	return offset
}

// dispatch routes one update to the message/callback handlers, enforcing
// private-chat-only access first.
func (t *TelegramProvider) dispatch(update tgUpdate) {
	if update.Message != nil && update.Message.Text != "" {
		if !t.ensurePrivate(update.Message.Chat, tgLang(update.Message.From)) {
			return
		}
		chatID := fmt.Sprintf("%d", update.Message.Chat.ID)
		userID := ""
		if update.Message.From != nil {
			userID = fmt.Sprintf("%d", update.Message.From.ID)
		}
		incMessagesIn()
		if t.onSeen != nil && userID != "" {
			t.onSeen(userID, update.Message.From.displayName())
		}
		if t.onLanguage != nil && userID != "" {
			t.onLanguage(userID, update.Message.From.LanguageCode)
		}
		t.onMessage("telegram", chatID, userID, update.Message.MessageID, update.Message.Text)
	}

	if m := update.Message; m != nil && m.Text == "" && m.From != nil && t.onUnsupported != nil {
		if t.ensurePrivate(m.Chat, tgLang(m.From)) {
			t.onUnsupported(fmt.Sprintf("%d", m.Chat.ID), fmt.Sprintf("%d", m.From.ID))
		}
	}

	if cq := update.CallbackQuery; cq != nil {
		switch {
		case cq.Message == nil:
			// Telegram omits `message` when the originating message is older than
			// ~48h. We can't route the tap, but we must still answer it so the
			// button's loading spinner clears instead of hanging forever.
			t.answerCallback(cq.ID, msgcat.T(tgLang(cq.From), "This menu has expired - send /menu to reopen."))
		case !t.ensurePrivate(cq.Message.Chat, tgLang(cq.From)):
			t.answerCallback(cq.ID, "")
		default:
			chatID := fmt.Sprintf("%d", cq.Message.Chat.ID)
			userID := ""
			if cq.From != nil {
				userID = fmt.Sprintf("%d", cq.From.ID)
				if t.onLanguage != nil {
					t.onLanguage(userID, cq.From.LanguageCode)
				}
			}
			incMessagesIn()
			toast := t.onCallback("telegram", chatID, userID, cq.Message.MessageID, cq.Data)
			t.answerCallback(cq.ID, toast)
		}
	}
}

// ensurePrivate drops group/channel traffic: this bot is a single-owner control
// surface, and a group would leak project status to every member. Each
// offending chat is warned exactly once to avoid a notification loop.
func (t *TelegramProvider) ensurePrivate(chat tgChat, lang string) bool {
	if chat.Type == "" || chat.Type == "private" {
		return true
	}
	t.warnedMu.Lock()
	warned := t.warnedChats[chat.ID]
	if !warned {
		t.warnedChats[chat.ID] = true
	}
	t.warnedMu.Unlock()
	if !warned {
		_, _ = t.sendOne(fmt.Sprintf("%d", chat.ID), msgcat.T(lang, "This bot only works in a direct message. Please message me privately."), nil)
	}
	return false
}

// nextBackoff returns the next polling backoff, capped at 30s.
func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > 30*time.Second {
		return 30 * time.Second
	}
	return next
}

// Stop cancels the polling loop and waits (bounded) for it to exit, so a
// replacement provider never races a still-live poller onto the same token
// (Telegram answers a second poller with 409 Conflict).
func (t *TelegramProvider) Stop() error {
	t.stopped.Store(true)
	t.lifeMu.Lock()
	cancel, done := t.cancel, t.done
	t.lifeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(40 * time.Second):
			t.logger.Warn("telegram poll loop did not stop within 40s")
		}
	}
	return nil
}

// SendMessage sends a text message, splitting at natural boundaries if it
// exceeds Telegram's per-message length cap. Returns the message ID of the
// last chunk (the one most useful for subsequent EditMessage calls).
func (t *TelegramProvider) SendMessage(chatID string, text string) (int, error) {
	chunks := splitMessage(text, tgMaxMessageLen)
	var lastID int
	for _, chunk := range chunks {
		id, err := t.sendOne(chatID, chunk, nil)
		if err != nil {
			incSendFailures()
			return lastID, err
		}
		lastID = id
	}
	incMessagesOut()
	return lastID, nil
}

// SendQuestion sends a message with inline keyboard buttons. Labels and
// callbackData must have equal length; each callback string must be <= 64
// bytes (Telegram limit). Layout is two buttons per row.
func (t *TelegramProvider) SendQuestion(chatID, question string, labels, callbackData []string) error {
	keyboard, err := buildInlineKeyboard(labels, callbackData)
	if err != nil {
		return err
	}
	if _, err := t.sendOne(chatID, question, keyboard); err != nil {
		incSendFailures()
		return err
	}
	incMessagesOut()
	return nil
}

// buildInlineKeyboard turns parallel label/callback slices into a Telegram
// inline_keyboard markup, two buttons per row. Each callback_data must be <= 64
// bytes (Telegram limit). Shared by SendQuestion and EditMessageKeyboard.
func buildInlineKeyboard(labels, callbackData []string) (map[string]any, error) {
	if len(labels) != len(callbackData) {
		return nil, fmt.Errorf("telegram: labels/callbackData length mismatch (%d vs %d)", len(labels), len(callbackData))
	}
	var rows [][]map[string]string
	var row []map[string]string
	for i, label := range labels {
		cb := callbackData[i]
		if !strings.HasPrefix(cb, "url:") && len(cb) > tgMaxCallbackLen {
			return nil, fmt.Errorf("telegram: callback_data %q exceeds %d bytes", cb, tgMaxCallbackLen)
		}
		if strings.HasPrefix(cb, "url:") {
			// A link button opens the address instead of calling back.
			row = append(row, map[string]string{"text": label, "url": strings.TrimPrefix(cb, "url:")})
		} else {
			row = append(row, map[string]string{"text": label, "callback_data": cb})
		}
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if row != nil {
		rows = append(rows, row)
	}
	return map[string]any{"inline_keyboard": rows}, nil
}

// EditMessage replaces the text of a previously sent message.
func (t *TelegramProvider) EditMessage(chatID string, messageID int, text string) error {
	if messageID == 0 {
		return fmt.Errorf("telegram: EditMessage requires a non-zero messageID")
	}
	if len(text) > tgMaxMessageLen {
		text = text[:tgMaxMessageLen]
	}
	err := t.editOnce(chatID, messageID, text, true)
	if err != nil && isParseError(err) {
		// HTML entities couldn't be parsed (e.g. truncation split a tag); retry
		// as plaintext so the status bubble still updates.
		return t.editOnce(chatID, messageID, text, false)
	}
	return err
}

func (t *TelegramProvider) editOnce(chatID string, messageID int, text string, html bool) error {
	return t.editKeyboardOnce(chatID, messageID, text, nil, html)
}

// EditMessageKeyboard replaces both the text and the inline keyboard of a
// previously sent message. Used by the menu to move between screens in place.
// On a parse rejection it retries as plaintext. A "message is not modified"
// reply (tapping a button that re-renders identical content) is reported via
// isNotModifiedErr so callers can treat it as a no-op success.
func (t *TelegramProvider) EditMessageKeyboard(chatID string, messageID int, text string, replyMarkup map[string]any) error {
	if messageID == 0 {
		return fmt.Errorf("telegram: EditMessageKeyboard requires a non-zero messageID")
	}
	if len(text) > tgMaxMessageLen {
		text = text[:tgMaxMessageLen]
	}
	err := t.editKeyboardOnce(chatID, messageID, text, replyMarkup, true)
	if err != nil && isParseError(err) {
		return t.editKeyboardOnce(chatID, messageID, text, replyMarkup, false)
	}
	return err
}

func (t *TelegramProvider) editKeyboardOnce(chatID string, messageID int, text string, replyMarkup map[string]any, html bool) error {
	body := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	if html {
		body["parse_mode"] = "HTML"
	}
	if replyMarkup != nil {
		body["reply_markup"] = replyMarkup
	}
	_, err := t.apiCall("editMessageText", body)
	return err
}

// isNotModifiedErr reports whether a Telegram edit failed only because the new
// content is identical to the current message (a benign no-op).
func isNotModifiedErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not modified")
}

// DeleteMessage removes a previously sent message (best effort).
func (t *TelegramProvider) DeleteMessage(chatID string, messageID int) error {
	if messageID == 0 {
		return nil
	}
	_, err := t.apiCall("deleteMessage", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	})
	return err
}

// SendChatAction shows a transient status such as "typing". Best effort.
func (t *TelegramProvider) SendChatAction(chatID string, action string) error {
	if action == "" {
		action = "typing"
	}
	_, err := t.apiCall("sendChatAction", map[string]any{
		"chat_id": chatID,
		"action":  action,
	})
	return err
}

// sendOne sends a single message chunk and returns the platform message ID. On
// a Telegram "can't parse entities" rejection it retries once without
// parse_mode, so a malformed-HTML chunk degrades to plaintext instead of
// vanishing entirely.
func (t *TelegramProvider) sendOne(chatID, text string, replyMarkup map[string]any) (int, error) {
	return t.sendOneOpts(chatID, text, replyMarkup, false)
}

func (t *TelegramProvider) sendOneOpts(chatID, text string, replyMarkup map[string]any, silent bool) (int, error) {
	id, err := t.postMessage(chatID, text, replyMarkup, true, silent)
	if err != nil && isParseError(err) {
		t.logger.Debug("telegram parse error, retrying as plaintext", "error", redactErr(err, t.token))
		return t.postMessage(chatID, text, replyMarkup, false, silent)
	}
	return id, err
}

// SendCard posts a message with an inline keyboard, optionally without a
// notification, and returns the new message id.
func (t *TelegramProvider) SendCard(chatID, text string, replyMarkup map[string]any, silent bool) (int, error) {
	if len(text) > tgMaxMessageLen {
		text = text[:tgMaxMessageLen]
	}
	id, err := t.sendOneOpts(chatID, text, replyMarkup, silent)
	if err != nil {
		incSendFailures()
		return 0, err
	}
	incMessagesOut()
	return id, nil
}

// SendDocument uploads data as a file attachment.
func (t *TelegramProvider) SendDocument(chatID, filename string, data []byte, caption string) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", chatID)
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	part, err := mw.CreateFormFile("document", filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest("POST", t.baseURL+"/sendDocument", &buf)
	if err != nil {
		return fmt.Errorf("telegram sendDocument: failed to build request: %s", redactToken(err.Error(), t.token))
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := t.client.Do(req)
	if err != nil {
		incSendFailures()
		return fmt.Errorf("telegram sendDocument request failed: %s", redactToken(err.Error(), t.token))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env tgErrorEnvelope
	_ = json.Unmarshal(body, &env)
	if resp.StatusCode != 200 || !env.OK {
		incSendFailures()
		return fmt.Errorf("telegram sendDocument returned %d: %s", resp.StatusCode, redactToken(env.Description, t.token))
	}
	incMessagesOut()
	return nil
}

func (t *TelegramProvider) postMessage(chatID, text string, replyMarkup map[string]any, html, silent bool) (int, error) {
	body := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if silent {
		body["disable_notification"] = true
	}
	if html {
		body["parse_mode"] = "HTML"
	}
	if replyMarkup != nil {
		body["reply_markup"] = replyMarkup
	}
	data, err := t.apiCall("sendMessage", body)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Result.MessageID, nil
}

// isParseError reports whether a Telegram error is an HTML-entity parse
// rejection, which we recover from by resending without parse_mode.
func isParseError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "parse entities")
}

// --- Telegram API internals ---

type tgUpdate struct {
	UpdateID      int              `json:"update_id"`
	Message       *tgMessage       `json:"message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

type tgUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
	// LanguageCode is the IETF tag of the person's Telegram app ("de", "pt-br").
	LanguageCode string `json:"language_code"`
}

// tgLang is the catalog language a Telegram user's app reports, or "" (English)
// when it is missing or has no catalog.
func tgLang(u *tgUser) string {
	if u == nil {
		return ""
	}
	code, ok := replylang.ParseLocale(u.LanguageCode)
	if !ok {
		return ""
	}
	return code
}

// displayName is the friendliest label Telegram gave us for a person.
func (u *tgUser) displayName() string {
	if u == nil {
		return ""
	}
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" && u.Username != "" {
		name = "@" + u.Username
	}
	return name
}

type tgMessage struct {
	MessageID int     `json:"message_id"`
	Text      string  `json:"text"`
	Chat      tgChat  `json:"chat"`
	From      *tgUser `json:"from"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgCallbackQuery struct {
	ID      string     `json:"id"`
	Data    string     `json:"data"`
	Message *tgMessage `json:"message"`
	From    *tgUser    `json:"from"`
}

type tgBotInfo struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// getMeWithRetry validates the token. Telegram rejecting it (401/404) is final, but
// a network failure is not: a laptop that boots before its Wi-Fi is up would
// otherwise leave a dead bot until the next restart. Network failures are retried
// with a growing delay, and the health line says so while it waits.
func (t *TelegramProvider) getMeWithRetry(ctx context.Context) (*tgBotInfo, error) {
	delay := 5 * time.Second
	for {
		info, err := t.getMe(ctx)
		if err == nil {
			return info, nil
		}
		var apiErr *tgAPIError
		if errors.As(err, &apiErr) && (apiErr.status == 401 || apiErr.status == 404) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		setBotHealth(false, "cannot reach Telegram, retrying: "+redactToken(err.Error(), t.token))
		t.logger.Warn("telegram unreachable at startup; retrying", "in", delay, "error", redactErr(err, t.token))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < time.Minute {
			delay *= 2
		}
	}
}

func (t *TelegramProvider) getMe(ctx context.Context) (*tgBotInfo, error) {
	data, err := t.apiCallCtx(ctx, "getMe", map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result tgBotInfo `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("telegram: invalid getMe response: %w", err)
	}
	return &resp.Result, nil
}

func (t *TelegramProvider) getUpdates(ctx context.Context, offset int) ([]tgUpdate, error) {
	body := map[string]any{
		"offset":          offset,
		"timeout":         30,
		"allowed_updates": []string{"message", "callback_query"},
	}

	data, err := t.apiCallCtx(ctx, "getUpdates", body)
	if err != nil {
		return nil, err
	}

	var resp struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("telegram: invalid response: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram: API returned ok=false")
	}
	return resp.Result, nil
}

// Acknowledge tells Telegram the saved poll offset has been handled, so the last
// update is not delivered again. Best effort, and only meaningful once polling
// has stopped.
func (t *TelegramProvider) Acknowledge() {
	if t.offsets == nil {
		return
	}
	offset := t.offsets.LoadOffset()
	if offset <= 0 {
		return
	}
	_, _ = t.apiCall("getUpdates", map[string]any{"offset": offset, "timeout": 0, "limit": 1})
}

func (t *TelegramProvider) answerCallback(callbackID, text string) {
	body := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		// Telegram caps callback answer text at 200 chars.
		text = stringutil.Truncate(text, 200)
		body["text"] = text
	}
	if _, err := t.apiCall("answerCallbackQuery", body); err != nil {
		t.logger.Warn("telegram answerCallbackQuery failed", "error", redactErr(err, t.token))
	}
}

// profileLanguages lists the languages the bot's command list and profile text
// are published in: the default (English, for everyone Telegram has no better
// match for) and each language with a catalog.
func profileLanguages() []string {
	return append([]string{""}, msgcat.Languages()...)
}

// withLanguage adds Telegram's language_code to a profile call, unless it is the
// default set.
func withLanguage(body map[string]any, lang string) map[string]any {
	if lang != "" {
		body["language_code"] = lang
	}
	return body
}

func (t *TelegramProvider) setCommands() {
	for _, lang := range profileLanguages() {
		_, err := t.apiCall("setMyCommands", withLanguage(map[string]any{
			"commands": []map[string]string{
				{"command": "menu", "description": msgcat.T(lang, "Home")},
				{"command": "status", "description": msgcat.T(lang, "Phase, progress and usage")},
				{"command": "questions", "description": msgcat.T(lang, "What is waiting for you")},
				{"command": "log", "description": msgcat.T(lang, "Every LLM call, with details")},
				{"command": "site", "description": msgcat.T(lang, "Publish and open your site")},
				{"command": "pause", "description": msgcat.T(lang, "Pause the builder")},
				{"command": "resume", "description": msgcat.T(lang, "Resume the builder")},
				{"command": "stop", "description": msgcat.T(lang, "Stop the current work")},
				{"command": "projects", "description": msgcat.T(lang, "Switch project")},
				{"command": "alerts", "description": msgcat.T(lang, "How chatty I am")},
				{"command": "help", "description": msgcat.T(lang, "What I can do")},
				{"command": "selfdestruct", "description": msgcat.T(lang, "Erase everything from the server")},
				{"command": "cancel", "description": msgcat.T(lang, "Cancel current step")},
			},
		}, lang))
		if err != nil {
			t.logger.Warn("telegram setMyCommands failed", "language", lang, "error", redactErr(err, t.token))
		}
	}
}

// setProfile fills in the bot's profile text and menu button so it looks
// finished the first time someone opens it. Best effort.
func (t *TelegramProvider) setProfile() {
	type call struct {
		method string
		body   map[string]any
	}
	var calls []call
	for _, lang := range profileLanguages() {
		calls = append(calls,
			call{"setMyDescription", withLanguage(map[string]any{
				"description": msgcat.T(lang, "Build and manage your Open Fabrica projects from your phone. Chat with the builder, answer its questions and follow progress live."),
			}, lang)},
			call{"setMyShortDescription", withLanguage(map[string]any{
				"short_description": msgcat.T(lang, "Build and manage Open Fabrica projects"),
			}, lang)},
		)
	}
	calls = append(calls, call{"setChatMenuButton", map[string]any{
		"menu_button": map[string]any{"type": "commands"},
	}})
	for _, c := range calls {
		if _, err := t.apiCall(c.method, c.body); err != nil {
			t.logger.Debug("telegram profile call failed", "method", c.method, "error", redactErr(err, t.token))
		}
	}
}

func (t *TelegramProvider) apiCall(method string, body map[string]any) ([]byte, error) {
	return t.apiCallRetry(context.Background(), method, body)
}

// apiCallRetry wraps apiCallCtx with bounded retries for transient failures:
// HTTP 429 (honoring Telegram's retry_after) and 5xx. All outbound sends go
// through here so a rate-limit or blip doesn't silently drop a user-facing
// message. getUpdates and getMe deliberately call apiCallCtx directly.
// getUpdates has its own poll-loop backoff, and getMe is a one-shot startup
// probe. A 429 means the request was rejected, so a retry can't double-send; a
// 5xx retry carries a small duplicate risk on the rare "delivered but 500 on the
// response", which is an acceptable trade for a low-volume control bot.
func (t *TelegramProvider) apiCallRetry(ctx context.Context, method string, body map[string]any) ([]byte, error) {
	const maxAttempts = 3
	for attempt := 1; ; attempt++ {
		data, err := t.apiCallCtx(ctx, method, body)
		if err == nil || attempt >= maxAttempts {
			return data, err
		}
		var apiErr *tgAPIError
		if !errors.As(err, &apiErr) || !isRetryable(apiErr.status) {
			// Permanent: parse error, 400, not-modified, or a transport error
			// (not a *tgAPIError). Let the caller handle it.
			return data, err
		}
		wait := apiErr.retryAfter
		if wait <= 0 {
			wait = time.Duration(attempt) * 500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return data, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// isRetryable reports whether an HTTP status warrants a retry.
func isRetryable(status int) bool {
	return status == 429 || (status >= 500 && status <= 599)
}

func (t *TelegramProvider) apiCallCtx(ctx context.Context, method string, body map[string]any) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", t.baseURL+"/"+method, bytes.NewReader(jsonBody))
	if err != nil {
		// The error embeds the request URL (which contains the bot token); never
		// surface it raw.
		return nil, fmt.Errorf("telegram %s: failed to build request: %s", method, redactToken(err.Error(), t.token))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		// client.Do returns a *url.Error wrapping the full URL incl. the token.
		return nil, fmt.Errorf("telegram %s request failed: %s", method, redactToken(err.Error(), t.token))
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("telegram %s: read response failed: %s", method, redactToken(err.Error(), t.token))
	}

	if resp.StatusCode != 200 {
		snippet := strings.TrimSpace(string(data))
		snippet = stringutil.Truncate(snippet, 200)
		var env tgErrorEnvelope
		_ = json.Unmarshal(data, &env)
		desc := env.Description
		if desc == "" {
			desc = snippet // non-JSON body, e.g. a proxy error page
		}
		status := resp.StatusCode
		if env.ErrorCode != 0 {
			status = env.ErrorCode
		}
		return data, &tgAPIError{
			method:     method,
			status:     status,
			retryAfter: time.Duration(env.Parameters.RetryAfter) * time.Second,
			desc:       redactToken(desc, t.token),
		}
	}

	// A 200 OK can still carry ok:false (e.g. message-not-modified on edits, or
	// "can't parse entities" on a malformed HTML body).
	var env tgErrorEnvelope
	if json.Unmarshal(data, &env) == nil && !env.OK {
		status := resp.StatusCode
		if env.ErrorCode != 0 {
			status = env.ErrorCode
		}
		return data, &tgAPIError{
			method:     method,
			status:     status,
			retryAfter: time.Duration(env.Parameters.RetryAfter) * time.Second,
			desc:       redactToken(env.Description, t.token),
		}
	}

	return data, nil
}

// tgErrorEnvelope is the shape Telegram returns for a failed call, on either a
// non-200 or a 200-with-ok:false body.
type tgErrorEnvelope struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"` // seconds Telegram asks us to wait (429)
	} `json:"parameters"`
}

// tgAPIError carries the effective status code and Telegram's retry_after so
// apiCallRetry can tell a transient 429/5xx (retry) from a permanent 400 (don't)
// and honor the server's requested pause. Its Error() still embeds the
// description, so the isParseError/isNotModifiedErr string matchers keep working.
type tgAPIError struct {
	method     string
	status     int
	retryAfter time.Duration // from parameters.retry_after; 0 if absent
	desc       string        // already token-redacted
}

func (e *tgAPIError) Error() string {
	return fmt.Sprintf("telegram API %s returned %d: %s", e.method, e.status, e.desc)
}

// redactToken removes the bot token from a string before it can reach a log,
// response, or proxy. The token appears inside the request URL path.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<redacted>")
}

func redactErr(err error, token string) string {
	if err == nil {
		return ""
	}
	return redactToken(err.Error(), token)
}
