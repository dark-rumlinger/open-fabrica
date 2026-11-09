package bot

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"openfabrica/db"
)

// userNamesKey holds the display name of each allowlisted person, as a JSON
// object keyed by user id. A plain setting, so it needs no schema.
const userNamesKey = "telegram_user_names"

// nameStore holds display names: a persisted map for allowlisted people (loaded
// fresh from the setting on each read) and an in-memory map (seen) for senders not
// yet on the allowlist, so a pairing that follows can pick up their name. It owns
// its own lock and persistence.
type nameStore struct {
	mu   sync.Mutex
	db   *db.DB
	seen map[string]string
}

func newNameStore(database *db.DB) *nameStore {
	return &nameStore{db: database, seen: map[string]string{}}
}

// load returns the persisted display names.
func (n *nameStore) load() map[string]string {
	names := map[string]string{}
	if v, _ := n.db.GetSetting(userNamesKey); v != "" {
		_ = json.Unmarshal([]byte(v), &names)
	}
	return names
}

// remember persists a display name, capping the map and dropping the
// lexicographically-smallest ids when it overflows.
func (n *nameStore) remember(id, name string) {
	name = strings.TrimSpace(name)
	if id == "" || name == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	names := n.load()
	if names[id] == name {
		return
	}
	names[id] = name
	if len(names) > 200 {
		ids := make([]string, 0, len(names))
		for k := range names {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		for _, k := range ids[:len(ids)-200] {
			delete(names, k)
		}
	}
	if b, err := json.Marshal(names); err == nil {
		_ = n.db.SetSetting(userNamesKey, string(b))
	}
}

// note records a sender's display name: persisted for allowed people, held in
// memory for everyone else so a pairing that follows can pick it up.
func (n *nameStore) note(userID, name string, allowed bool) {
	if userID == "" || name == "" {
		return
	}
	if allowed {
		n.remember(userID, name)
		return
	}
	n.mu.Lock()
	if len(n.seen) > 500 {
		n.seen = map[string]string{}
	}
	n.seen[userID] = name
	n.mu.Unlock()
}

// pending returns the display name captured for a sender not yet on the allowlist.
func (n *nameStore) pending(userID string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.seen[userID]
}
