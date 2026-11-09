package bot

import (
	"errors"
	"strings"
)

var errNotRunning = errors.New("telegram bot is not running")

// ConnectionStatus is what the admin setup page shows about the Telegram bot.
type ConnectionStatus struct {
	// Configured is true when a bot token is saved.
	Configured bool `json:"configured"`
	// Running is true when the bot is registered and polling.
	Running bool `json:"running"`
	// Healthy is true when the last contact with Telegram succeeded.
	Healthy  bool   `json:"healthy"`
	Detail   string `json:"detail,omitempty"`
	Username string `json:"username,omitempty"`
	BotName  string `json:"bot_name,omitempty"`
	DeepLink string `json:"deep_link,omitempty"`
	// LastPoll is the unix time of the last successful poll (0 if never).
	LastPoll int64      `json:"last_poll"`
	Users    []UserInfo `json:"users"`
}

// Status reports the current state of the Telegram connection.
func (s *BotService) Status() ConnectionStatus {
	st := ConnectionStatus{
		Configured: LoadBotToken(s.deps.DB, s.deps.Encryptor) != "",
		Users:      s.Users(),
	}
	stats := GetStats()

	if p, ok := s.provider("telegram"); ok {
		st.Running = true
		if tp, ok := p.(*TelegramProvider); ok {
			st.Username, st.BotName = tp.BotInfo()
		}
		st.Healthy = stats.Healthy
		st.Detail = stats.HealthDetail
		st.LastPoll = stats.LastPollUnix
	}
	if st.Username != "" {
		st.DeepLink = BotLink(st.Username, "")
	}
	return st
}

// BotLink builds a t.me deep link, with an optional /start payload.
func BotLink(username, payload string) string {
	username = strings.TrimPrefix(username, "@")
	link := "https://t.me/" + username
	if payload != "" {
		link += "?start=" + payload
	}
	return link
}

// SendTest sends a short hello to one person, to confirm delivery end to end.
func (s *BotService) SendTest(userID string) error {
	p, ok := s.provider("telegram")
	if !ok {
		return errNotRunning
	}
	// In Telegram a private chat shares its id with the user.
	_, err := p.SendMessage(userID, s.voiceForUser(userID).tr("👋 <b>Test message</b>\nThe connection works. Send /menu to get started."))
	return err
}
