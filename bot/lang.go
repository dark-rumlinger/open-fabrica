package bot

import (
	"openfabrica/db/models"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/projectphase"
	"openfabrica/internal/replylang"
)

// voice renders the bot's own messages in one person's language. The English
// message is its identity, written exactly as the catalogs key it, so the same
// literal serves as the fallback when a language has no entry. Prompts and
// anything a model wrote never pass through here: they stay as they are.
type voice struct{ lang string }

// tr renders a plain-text message: button labels, toasts and anything else that
// is not sent in HTML parse mode.
func (v voice) tr(format string, args ...any) string {
	return msgcat.T(v.lang, format, args...)
}

// trh renders a message sent in HTML parse mode. String and error arguments are
// escaped, so a project name or an error text cannot inject markup, while the
// message's own tags stay intact. Numbers pass through untouched.
func (v voice) trh(format string, args ...any) string {
	if len(args) == 0 {
		return msgcat.T(v.lang, format)
	}
	escaped := make([]any, len(args))
	for i, a := range args {
		switch x := a.(type) {
		case string:
			escaped[i] = escapeHTML(x)
		case error:
			escaped[i] = escapeHTML(x.Error())
		default:
			escaped[i] = a
		}
	}
	return msgcat.T(v.lang, format, escaped...)
}

// plainRender is a render that has nothing to translate (a model's reply, a
// value from the database).
func plainRender(text string) func(voice) string {
	return func(voice) string { return text }
}

// lang is the catalog language this person's bot speaks. A project whose owner
// picked a reply language keeps its screens in that language, so the buttons
// match the builder's own lines. Otherwise it follows the language the person's
// Telegram app reports. "" is English.
func (s *BotService) lang(state *UserState) string {
	if state == nil {
		return ""
	}
	if state.ActiveProjectID != 0 {
		if p, err := models.GetProjectByID(s.deps.DB.DB, state.ActiveProjectID); err == nil {
			if code, ok := replylang.ParseLocale(models.ProjectReplyLanguage(p)); ok {
				return code
			}
		}
	}
	return s.userLang(state.UserID)
}

func (s *BotService) voice(state *UserState) voice { return voice{lang: s.lang(state)} }

// voiceForUser is the voice of a person who has no session yet (an invite, a
// stranger, a reply to a message with no text).
func (s *BotService) voiceForUser(userID string) voice { return voice{lang: s.userLang(userID)} }

// userLang is the language a person's Telegram app last reported, or "".
func (s *BotService) userLang(userID string) string { return s.langs.userLang(userID) }

// noteLanguage is the provider's "what language does this person use" hook: it
// parses the reported tag and records it (persisted for allowed people, in memory
// for everyone else, so a refusal or an invite reply can still speak their language).
func (s *BotService) noteLanguage(userID, tag string) {
	code, ok := replylang.ParseLocale(tag)
	if userID == "" || !ok {
		return
	}
	s.langs.note(userID, code, s.isAllowed(userID))
}

// keepSeenLanguage moves the language noted for a stranger onto disk once they
// have been added to the allowlist.
func (s *BotService) keepSeenLanguage(userID string) { s.langs.keepSeen(userID) }

// phaseLabel is a phase's name in the person's language. An unknown key keeps
// the English label the phase package supplied.
func (v voice) phaseLabel(p projectphase.Phase) string {
	switch p.Key {
	case projectphase.KeyBuilding:
		return v.tr("Building")
	case projectphase.KeyNeedsYou:
		return v.tr("Needs you")
	case projectphase.KeyLive:
		return v.tr("Live")
	case projectphase.KeyPaused:
		return v.tr("Paused")
	}
	return p.Label
}

// reasonLine is the one-line reason a phase gives, in the person's language, or
// "" when the phase says it all.
func (v voice) reasonLine(p projectphase.Phase) string {
	switch p.ReasonKey {
	case projectphase.ReasonAnswers:
		if p.Pending == 1 {
			return v.tr("1 question needs your answer")
		}
		return v.tr("%d questions need your answer", p.Pending)
	case projectphase.ReasonAnswersNone:
		return v.tr("Waiting for your answer")
	case projectphase.ReasonApproval:
		return v.tr("Waiting for your approval")
	case projectphase.ReasonEngineDown:
		return v.tr("The engine is shut down")
	case projectphase.ReasonStoppedOwner:
		return v.tr("Stopped by you")
	case projectphase.ReasonStoppedError:
		return v.tr("Stopped after an error")
	case projectphase.ReasonPausedOwner:
		return v.tr("Paused by you")
	}
	return p.Reason
}
