// Package replylang decides which language the assistant writes to the owner in,
// and renders the instruction that says so.
//
// Three values are involved, and keeping them apart is the point of the package:
//
//   - A preference is what a user chose in their account: "" (follow the
//     interface language), "match" (answer in whatever language they write), or a
//     language code.
//   - A locale is the interface language the browser reports with each request.
//   - A directive is the resolved result: "" (no instruction, the model decides as it
//     always has), "match", or a language code. The directive is what gets stored on
//     the project and read back when a prompt is built.
//
// Only the owner-facing prose follows the directive. Tool calls, tool arguments,
// code, identifiers and structured keys stay English; nothing here touches the
// system prompt, tool schemas or contracts.
package replylang

import "strings"

// Interface is the preference that follows the interface language.
const Interface = ""

// Match is the preference (and directive) that answers in the language the owner
// writes in.
const Match = "match"

// Header is the request header the admin client uses to report its interface
// language.
const Header = "X-Of-Locale"

// Language is one language the assistant can be asked to write in.
type Language struct {
	Code string // lowercase primary subtag, the same code the admin UI uses
	Name string // English name, which is what the instruction to the model uses
}

// languages is the closed set of codes accepted anywhere in the system. Adding a
// language here is the only server-side change needed to offer it.
var languages = []Language{
	{"en", "English"},
	{"es", "Spanish"},
	{"fr", "French"},
	{"de", "German"},
	{"pt", "Portuguese"},
	{"uk", "Ukrainian"},
	{"ru", "Russian"},
	{"zh", "Chinese"},
	{"ja", "Japanese"},
	{"hi", "Hindi"},
	{"ar", "Arabic"},
}

// Languages returns the supported languages in display order. The slice is a copy.
func Languages() []Language {
	out := make([]Language, len(languages))
	copy(out, languages)
	return out
}

func nameOf(code string) (string, bool) {
	for _, l := range languages {
		if l.Code == code {
			return l.Name, true
		}
	}
	return "", false
}

// ParseLocale reduces a locale or language tag ("es", "zh-CN", "pt_BR") to a
// supported code. It reports false for anything else, including the empty string,
// so a missing or hostile header is simply ignored.
func ParseLocale(tag string) (string, bool) {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexAny(tag, "-_"); i >= 0 {
		tag = tag[:i]
	}
	if _, ok := nameOf(tag); !ok {
		return "", false
	}
	return tag, true
}

// NormalizePreference validates a stored or submitted preference and returns its
// canonical form. Surrounding space and case are forgiven; an unknown value is not.
func NormalizePreference(pref string) (string, bool) {
	pref = strings.ToLower(strings.TrimSpace(pref))
	if pref == Interface || pref == Match {
		return pref, true
	}
	return ParseLocale(pref)
}

// Resolve turns a user's preference and the request's interface language into the
// directive to store. The second result is false when there is not enough
// information to decide, and the caller must then leave the stored directive alone:
// the preference follows the interface but the request did not say which interface
// it came from (a script, an old client), or the preference is not a valid value.
//
// "Same as interface" with an English interface resolves to no directive rather
// than to English: the assistant keeps answering in the language the owner writes
// in, exactly as it did before this setting existed. Choosing English explicitly
// does force English.
func Resolve(pref, locale string) (directive string, ok bool) {
	pref, valid := NormalizePreference(pref)
	if !valid {
		return "", false
	}
	if pref != Interface {
		return pref, true
	}
	code, known := ParseLocale(locale)
	if !known {
		return "", false
	}
	if code == "en" {
		return "", true
	}
	return code, true
}

// keepEnglish is the half of the instruction that does not vary by language.
const keepEnglish = "Keep tool calls, tool arguments, code, identifiers, file names, JSON keys, enum values and the task titles and intents in a build plan in English. " +
	"This covers what you say to the owner only; the language of the site's own content follows what the owner asks for."

// Line renders the owner-context bullet for a directive, or "" when there is
// nothing to say.
func Line(directive string) string {
	if directive == Match {
		return "- Reply language: the owner's own. Write everything the owner reads (chat replies, questions, summaries, descriptions) " +
			"in the language of their latest message, and keep using their language when a message is too short to tell, such as \"ok\". " +
			keepEnglish
	}
	name, ok := nameOf(directive)
	if !ok {
		return ""
	}
	return "- Reply language: " + name + ". Write everything the owner reads (chat replies, questions, summaries, descriptions) in " + name + ". " +
		keepEnglish
}

// Reminder is the short form carried on a message delivered into a session that is
// already running, whose opening context was written before the directive changed.
// It is empty when there is no directive.
func Reminder(directive string) string {
	if directive == Match {
		return "(reply in the language the owner wrote this in)"
	}
	if name, ok := nameOf(directive); ok {
		return "(reply to the owner in " + name + ")"
	}
	return ""
}
