package bot

import (
	"errors"
	"strings"
)

const allowedUsersKey = "telegram_allowed_users"

// UserInfo describes one person allowed to use the bot.
type UserInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func splitCSV(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsID(list []string, id string) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

func withoutID(list []string, id string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

func (s *BotService) csvSetting(key string) []string {
	v, _ := s.deps.DB.GetSetting(key)
	return splitCSV(v)
}

func (s *BotService) saveCSV(key string, list []string) error {
	return s.deps.DB.SetSetting(key, strings.Join(list, ","))
}

// Users lists everyone on the allowlist with their display name and role.
func (s *BotService) Users() []UserInfo {
	allowed := s.csvSetting(allowedUsersKey)
	names := s.loadNames()
	out := make([]UserInfo, 0, len(allowed))
	for _, id := range allowed {
		out = append(out, UserInfo{ID: id, Name: names[id]})
	}
	return out
}

// AddUser puts a person on the allowlist. Everyone on it has full control.
func (s *BotService) AddUser(id, name string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("user id required")
	}
	s.usersMu.Lock()
	allowed := s.csvSetting(allowedUsersKey)
	if !containsID(allowed, id) {
		if err := s.saveCSV(allowedUsersKey, append(allowed, id)); err != nil {
			s.usersMu.Unlock()
			return err
		}
	}
	s.usersMu.Unlock()
	if name != "" {
		s.rememberName(id, name)
	}
	return nil
}

// RemoveUser takes a person off the allowlist and detaches their sessions.
func (s *BotService) RemoveUser(id string) error {
	s.usersMu.Lock()
	allowed := s.csvSetting(allowedUsersKey)
	if !containsID(allowed, id) {
		s.usersMu.Unlock()
		return errors.New("unknown user")
	}
	if err := s.saveCSV(allowedUsersKey, withoutID(allowed, id)); err != nil {
		s.usersMu.Unlock()
		return err
	}
	s.usersMu.Unlock()

	s.mu.RLock()
	sessions := make([]*UserState, 0, len(s.users))
	for _, st := range s.users {
		sessions = append(sessions, st)
	}
	s.mu.RUnlock()
	for _, st := range sessions {
		st.Mu.Lock()
		if st.UserID == id {
			st.ActiveProjectID = 0
			st.PendingAction = ""
			st.PendingData = nil
			SaveSession(s.deps.DB, st)
		}
		st.Mu.Unlock()
	}
	return nil
}

// --- display names (state + persistence live in namestore.go) ---

func (s *BotService) loadNames() map[string]string { return s.names.load() }
func (s *BotService) rememberName(id, name string) { s.names.remember(id, name) }

// noteUser is the provider's "who is talking" hook: allowed people are remembered
// on disk, everyone else held briefly in memory so a pairing that follows can pick
// up their name.
func (s *BotService) noteUser(userID, name string) { s.names.note(userID, name, s.isAllowed(userID)) }

// pendingName returns the display name captured for a sender not yet on the allowlist.
func (s *BotService) pendingName(userID string) string { return s.names.pending(userID) }
