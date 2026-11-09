package bot

import (
	"encoding/json"
	"sync"

	"openfabrica/db"
)

// userLangsKey holds the language each allowed person's Telegram app reports, as
// a JSON object keyed by user id. Like the display names, it is a plain setting,
// so it needs no schema.
const userLangsKey = "telegram_user_langs"

const (
	// maxUserLangs bounds the persisted map, maxSeenLangs the in-memory one.
	maxUserLangs = 200
	maxSeenLangs = 500
)

// langStore holds the UI language each person's Telegram app reports: a persisted
// map (langs) for allowed people and an in-memory map (seen) for everyone else, so
// a refusal or an invite reply can already speak to a stranger in their language.
// It owns its own lock and its own persistence, isolating language state from the
// rest of the bot.
type langStore struct {
	mu     sync.Mutex
	db     *db.DB
	loaded bool
	langs  map[string]string
	seen   map[string]string
}

func newLangStore(database *db.DB) *langStore { return &langStore{db: database} }

// userLang is the language a person last reported, or "".
func (l *langStore) userLang(userID string) string {
	if userID == "" {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	if code := l.langs[userID]; code != "" {
		return code
	}
	return l.seen[userID]
}

func (l *langStore) loadLocked() {
	if l.loaded {
		return
	}
	l.loaded = true
	l.langs = map[string]string{}
	if l.seen == nil {
		l.seen = map[string]string{}
	}
	if v, _ := l.db.GetSetting(userLangsKey); v != "" {
		_ = json.Unmarshal([]byte(v), &l.langs)
	}
}

// note records a person's language (already a parsed locale). Allowed people are
// persisted; everyone else is held briefly in memory.
func (l *langStore) note(userID, code string, allowed bool) {
	l.mu.Lock()
	l.loadLocked()
	if !allowed {
		if len(l.seen) > maxSeenLangs {
			l.seen = map[string]string{}
		}
		l.seen[userID] = code
		l.mu.Unlock()
		return
	}
	l.promoteLocked(userID, code)
	l.mu.Unlock()
}

// keepSeen moves the language noted for a stranger onto disk once they have been
// added to the allowlist.
func (l *langStore) keepSeen(userID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	if code := l.seen[userID]; code != "" {
		l.promoteLocked(userID, code)
	}
}

func (l *langStore) promoteLocked(userID, code string) {
	if l.langs[userID] == code {
		return
	}
	l.langs[userID] = code
	if len(l.langs) > maxUserLangs {
		for id := range l.langs {
			if id != userID {
				delete(l.langs, id)
				break
			}
		}
	}
	if b, err := json.Marshal(l.langs); err == nil {
		_ = l.db.SetSetting(userLangsKey, string(b))
	}
}
