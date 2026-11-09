package bot

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"strings"
	"sync"
	"time"

	"openfabrica/events"
)

const (
	pairingTTL = 15 * time.Minute
	// Wrong invite codes allowed per chat per window before it is ignored.
	pairFailLimit  = 5
	pairFailWindow = time.Minute
)

type pairingCode struct {
	expires time.Time
}

type failWindow struct {
	count   int
	resetAt time.Time
}

var pairingEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// pairingStore holds the in-memory invite-code and wrong-code-attempt state. It
// owns its own lock, so pairing state is isolated from the rest of the bot.
type pairingStore struct {
	mu    sync.Mutex
	codes map[string]pairingCode
	fails map[string]*failWindow
}

func newPairingStore() *pairingStore {
	return &pairingStore{
		codes: make(map[string]pairingCode),
		fails: make(map[string]*failWindow),
	}
}

// NewPairingCode issues a single-use invite code. The person opens
// t.me/<bot>?start=<code>, and the bot adds their account to the allowlist.
func (s *BotService) NewPairingCode() (string, time.Time, error) { return s.pair.newCode() }

func (p *pairingStore) newCode() (string, time.Time, error) {
	raw := make([]byte, 7) // 56 bits, 12 base32 chars
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	code := pairingEncoding.EncodeToString(raw)
	exp := time.Now().Add(pairingTTL)

	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, v := range p.codes {
		if now.After(v.expires) {
			delete(p.codes, k)
		}
	}
	p.codes[code] = pairingCode{expires: exp}
	return code, exp, nil
}

// consume checks a code against every live invite in constant time per
// comparison and removes it on success.
func (p *pairingStore) consume(candidate string) bool {
	candidate = strings.ToUpper(strings.TrimSpace(candidate))
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	found := ""
	for code, v := range p.codes {
		match := subtle.ConstantTimeCompare([]byte(code), []byte(candidate)) == 1
		if match && now.Before(v.expires) {
			found = code
		}
	}
	if found == "" {
		return false
	}
	delete(p.codes, found)
	return true
}

// attemptAllowed applies the per-chat limit on wrong codes.
func (p *pairingStore) attemptAllowed(chatID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.fails[chatID]
	if w == nil || time.Now().After(w.resetAt) {
		return true
	}
	return w.count < pairFailLimit
}

func (p *pairingStore) recordFailure(chatID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	w := p.fails[chatID]
	if w == nil || now.After(w.resetAt) {
		w = &failWindow{resetAt: now.Add(pairFailWindow)}
		p.fails[chatID] = w
	}
	w.count++
	if len(p.fails) > 1000 {
		for k, v := range p.fails {
			if now.After(v.resetAt) {
				delete(p.fails, k)
			}
		}
	}
}

// startPayload extracts the argument of "/start <payload>" (also "/start@bot
// <payload>"). It returns "" for any other message.
func startPayload(text string) string {
	fields := strings.Fields(text)
	if len(fields) != 2 {
		return ""
	}
	cmd := strings.ToLower(fields[0])
	if at := strings.Index(cmd, "@"); at > 0 {
		cmd = cmd[:at]
	}
	if cmd != "/start" {
		return ""
	}
	return fields[1]
}

// tryPair handles a "/start <code>" message. It reports whether the message was
// fully handled; false means "carry on as a normal message" (an already-allowed
// user who opened a stale link just gets the menu).
func (s *BotService) tryPair(provider, chatID, userID, text string) bool {
	code := startPayload(text)
	if code == "" || userID == "" {
		return false
	}
	if !s.pair.attemptAllowed(chatID) {
		return true // over the limit: stay silent
	}

	v := s.voiceForUser(userID)
	if !s.pair.consume(code) {
		s.pair.recordFailure(chatID)
		if s.isAllowed(userID) {
			return false
		}
		if p, has := s.provider(provider); has {
			_, _ = p.SendMessage(chatID, v.tr("That invite link has expired or was already used. Ask whoever set up the bot for a new one."))
		}
		return true
	}

	if err := s.AddUser(userID, s.pendingName(userID)); err != nil {
		s.logger.Warn("pairing failed to add user", "error", err)
		if p, has := s.provider(provider); has {
			_, _ = p.SendMessage(chatID, v.tr("Something went wrong while connecting you. Please try again."))
		}
		return true
	}
	s.logger.Info("bot user paired", "userID", userID)
	s.keepSeenLanguage(userID)

	state := s.getOrCreateUser(provider, chatID)
	state.Mu.Lock()
	state.UserID = userID
	SaveSession(s.deps.DB, state)
	if p, has := s.provider(provider); has {
		_, _ = p.SendMessage(chatID, v.tr("✅ <b>You are connected.</b>\nBuild and manage your projects from here. I will message you when something needs your attention."))
	}
	s.showScreen(state, 0, s.mainMenuScreen(s.voice(state)))
	state.Mu.Unlock()

	s.deps.Bus.Publish(events.NewEvent(events.EventBotPaired, 0, map[string]any{
		"user_id": userID,
		"name":    s.loadNames()[userID],
	}))
	return true
}
