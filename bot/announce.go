package bot

import (
	"fmt"
	"sync"
	"time"

	"openfabrica/db/models"
)

// announceGoLive shows the go-live card to everyone watching the project, and a
// short heads-up to everyone looking at another one.
func (s *BotService) announceGoLive(pid int, info *goLiveInfo) {
	project, err := models.GetProjectByID(s.deps.DB.DB, pid)
	if err != nil {
		return
	}

	s.mu.RLock()
	var watchers []*UserState
	for _, st := range s.users {
		if st.ActiveProjectID == pid {
			watchers = append(watchers, st)
		}
	}
	s.mu.RUnlock()
	for _, st := range watchers {
		if p, ok := s.provider(st.Provider); ok {
			sc := s.goLiveCard(s.voice(st), project, info)
			kb, err := buildInlineKeyboard(sc.labels, sc.cbs)
			if err != nil {
				continue
			}
			if _, err := p.SendCard(st.ChatID, sc.text, kb, false); err != nil {
				s.logger.Warn("go-live card send failed", "error", err)
			}
			st.LiveCardStale.Store(true)
		}
	}
	s.alertOthers(pid, "live", "🎉 <b>%s</b> is live!")
}

// alertCooldown stops a burst of similar events (a batch of questions) from
// sending one heads-up each.
const alertCooldown = 30 * time.Second

var (
	alertMu   sync.Mutex
	alertSeen = map[string]time.Time{}
)

// alertOthers tells people who are looking at a different project (or none) that
// this one needs attention, with a button to switch to it. Without this, opening
// a second project would silently mute the first.
func (s *BotService) alertOthers(pid int, kind, format string) {
	project, err := models.GetProjectByID(s.deps.DB.DB, pid)
	if err != nil {
		return
	}
	s.mu.RLock()
	var others []*UserState
	for _, st := range s.users {
		if st.ActiveProjectID != pid && s.isAllowed(st.UserID) {
			others = append(others, st)
		}
	}
	s.mu.RUnlock()

	for _, st := range others {
		key := fmt.Sprintf("%s|%s|%d|%s", st.Provider, st.ChatID, pid, kind)
		alertMu.Lock()
		last := alertSeen[key]
		fresh := time.Since(last) >= alertCooldown
		if fresh {
			// Entries older than the cooldown suppress nothing, so drop them to keep
			// the map from growing with every chat/project/kind ever alerted.
			for k, t := range alertSeen {
				if time.Since(t) >= alertCooldown {
					delete(alertSeen, k)
				}
			}
			alertSeen[key] = time.Now()
		}
		alertMu.Unlock()
		if !fresh {
			continue
		}
		if p, ok := s.provider(st.Provider); ok {
			v := s.voice(st)
			_ = p.SendQuestion(st.ChatID, v.trh(format, project.Name),
				[]string{v.tr("Open %s", truncate(project.Name, 24))}, []string{fmt.Sprintf("project:%d", pid)})
		}
	}
}
