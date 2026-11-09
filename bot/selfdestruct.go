package bot

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"openfabrica/internal/safego"
)

// selfDestructTTL is how long the final confirmation button stays valid.
const selfDestructTTL = 60 * time.Second

// SelfDestructReport is what the erase routine hands back for the last message.
type SelfDestructReport struct {
	// Failed names the steps that did not complete.
	Failed []string
	// Container is true when the app ran in a container, whose image and volume
	// can only be removed from the host.
	Container bool
}

// selfDestructState holds the pending confirmation and the hooks installed by the
// process that owns the data.
type selfDestructState struct {
	mu      sync.Mutex
	nonce   string
	chatID  string
	expires time.Time

	run     func() SelfDestructReport
	exit    func()
	started atomic.Bool
}

// SetSelfDestruct installs the erase routine and the process exit. Without it the
// command reports that the feature is unavailable.
func (s *BotService) SetSelfDestruct(run func() SelfDestructReport, exit func()) {
	s.sd.mu.Lock()
	defer s.sd.mu.Unlock()
	s.sd.run, s.sd.exit = run, exit
}

func (s *BotService) selfDestructAvailable() bool {
	s.sd.mu.Lock()
	defer s.sd.mu.Unlock()
	return s.sd.run != nil
}

// selfDestructWarnScreen is the first step: what will be erased, and a way out.
func (s *BotService) selfDestructWarnScreen(v voice) screen {
	if !s.selfDestructAvailable() {
		return screen{
			text:   v.tr("Self-destruct is not available in this build."),
			labels: []string{v.tr("☰ Menu")},
			cbs:    []string{"nav:menu"},
		}
	}
	return screen{
		text: v.tr("⚠️ <b>Self-destruct</b>\n\n" +
			"This erases Open Fabrica from the server: every project, database, upload, key and certificate, " +
			"the config files, and the program itself. The bot stops answering afterwards.\n\n" +
			"It cannot be undone and there is no backup unless you made one yourself."),
		labels: []string{v.tr("Continue"), v.tr("◀ Cancel")},
		cbs:    []string{"nav:selfdestruct2", "nav:menu"},
	}
}

// selfDestructFinalScreen arms a one-time token and shows the last question. The
// token is bound to this chat and expires, so an old or forwarded button does
// nothing.
func (s *BotService) selfDestructFinalScreen(state *UserState) screen {
	v := s.voice(state)
	if !s.selfDestructAvailable() {
		return s.selfDestructWarnScreen(v)
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return screen{
			text:   v.tr("Could not prepare the confirmation. Nothing was changed."),
			labels: []string{v.tr("☰ Menu")},
			cbs:    []string{"nav:menu"},
		}
	}
	nonce := hex.EncodeToString(buf)
	s.sd.mu.Lock()
	s.sd.nonce, s.sd.chatID, s.sd.expires = nonce, state.ChatID, time.Now().Add(selfDestructTTL)
	s.sd.mu.Unlock()
	return screen{
		text: v.tr("🛑 <b>Last chance</b>\n\n" +
			"Erase everything now? This button works for 60 seconds."),
		labels: []string{v.tr("Yes, erase everything"), v.tr("◀ Cancel")},
		cbs:    []string{"cfm:sd:" + nonce, "nav:menu"},
	}
}

// consumeSelfDestruct reports whether nonce is the live token for this chat, and
// spends it either way so a token can never be used twice.
func (s *BotService) consumeSelfDestruct(chatID, nonce string) bool {
	s.sd.mu.Lock()
	defer s.sd.mu.Unlock()
	ok := nonce != "" && s.sd.nonce == nonce && s.sd.chatID == chatID && time.Now().Before(s.sd.expires)
	s.sd.nonce, s.sd.chatID, s.sd.expires = "", "", time.Time{}
	return ok
}

// confirmSelfDestruct handles the final tap. It returns the toast text.
func (s *BotService) confirmSelfDestruct(state *UserState, messageID int, nonce string) string {
	v := s.voice(state)
	if !s.consumeSelfDestruct(state.ChatID, nonce) {
		s.send(state, v.tr("That confirmation expired, so nothing was erased. Send /selfdestruct to start again."))
		return v.tr("Expired")
	}
	if !s.sd.started.CompareAndSwap(false, true) {
		return v.tr("Already running")
	}
	p, ok := s.provider(state.Provider)
	if ok && messageID != 0 {
		_ = p.EditMessage(state.ChatID, messageID, v.tr("Erasing now..."))
	}
	chatID, provider := state.ChatID, state.Provider
	// The erase runs off this goroutine: the update that carried the tap must
	// finish first so its offset is saved and Telegram does not replay it. The
	// voice is fixed now, because the data it is read from is gone by the end.
	safego.Go("bot-selfdestruct", func() { s.executeSelfDestruct(v, provider, chatID, messageID) })
	return v.tr("Erasing")
}

func (s *BotService) executeSelfDestruct(v voice, provider, chatID string, messageID int) {
	s.sd.mu.Lock()
	run, exit := s.sd.run, s.sd.exit
	s.sd.mu.Unlock()

	p, ok := s.provider(provider)
	if ok {
		// Stop polling and wait for the in-flight update to finish, then confirm
		// the saved offset with Telegram. Sending stays possible afterwards.
		_ = p.Stop()
		if tp, isTG := p.(*TelegramProvider); isTG {
			tp.Acknowledge()
		}
	}

	report := run()

	if ok {
		_ = p.DeleteMessage(chatID, messageID)
		_, _ = p.SendMessage(chatID, selfDestructSummary(v, report))
	}
	if exit != nil {
		exit()
	}
}

// selfDestructSummary words the last message.
func selfDestructSummary(v voice, r SelfDestructReport) string {
	var b strings.Builder
	if len(r.Failed) == 0 {
		b.WriteString(v.tr("✅ <b>Erased.</b> Open Fabrica is gone from this server. Goodbye."))
	} else {
		b.WriteString(v.tr("⚠️ <b>Erased, with problems.</b> These steps did not finish, so remove what is left by hand:") + "\n")
		for _, f := range r.Failed {
			b.WriteString("• " + escapeHTML(f) + "\n")
		}
	}
	if r.Container {
		b.WriteString("\n\n" + v.tr("The container image and data volume still exist on the host. Remove them there with docker rm, docker rmi and docker volume rm."))
	}
	b.WriteString("\n\n" + v.tr("This chat's history stays on Telegram. Delete it from your side if you want it gone."))
	return b.String()
}
