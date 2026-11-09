// Package msgcat translates the messages the Go side writes for people: API error
// messages, and the lines the engine says in chat.
//
// The English text is the message's identity. A catalog (locales/<code>.json) maps
// an English message, written exactly as it appears in the source, to its
// translation; there is no English catalog because English is the source. A
// message that has no entry in a language is shown in English, never as a key.
//
// Messages may be templates in the fmt.Sprintf style (see template.go). That is
// what lets one catalog serve two callers:
//
//   - T renders a template the caller already holds, for text the engine
//     composes itself: msgcat.T(lang, "Try %d of %d", n, max).
//   - Translate takes a message that is already formatted, as the HTTP layer sees
//     an error after the handler built it, and recognises which template made it.
//
// A coverage test (coverage_test.go) reads the source for every message that
// reaches a person and fails when one is missing from any catalog, so a new
// message cannot ship untranslated by accident.
//
// What this package never touches: prompts, tool schemas, tool results, and
// anything else a model reads. Those stay English by contract.
package msgcat

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"openfabrica/internal/replylang"
)

//go:embed locales/*.json
var localeFS embed.FS

// catalogs holds the parsed locale files and the matchers built from the union of
// their messages.
type catalogs struct {
	byLang   map[string]map[string]string
	exact    map[string]bool // plain-text messages known to any catalog
	matchers []*matcher      // templates, most specific first
}

var (
	loadOnce sync.Once
	loaded   *catalogs
)

func load() *catalogs {
	loadOnce.Do(func() {
		byLang := map[string]map[string]string{}
		entries, err := localeFS.ReadDir("locales")
		if err != nil {
			panic(fmt.Sprintf("msgcat: read locales: %v", err))
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			raw, err := localeFS.ReadFile("locales/" + name)
			if err != nil {
				panic(fmt.Sprintf("msgcat: read %s: %v", name, err))
			}
			m := map[string]string{}
			if err := json.Unmarshal(raw, &m); err != nil {
				panic(fmt.Sprintf("msgcat: parse %s: %v", name, err))
			}
			byLang[strings.TrimSuffix(name, ".json")] = m
		}
		loaded = build(byLang)
	})
	return loaded
}

// build indexes a set of catalogs: plain messages for exact lookup, templates as
// matchers.
func build(byLang map[string]map[string]string) *catalogs {
	c := &catalogs{byLang: byLang, exact: map[string]bool{}}
	seen := map[string]bool{}
	for _, m := range byLang {
		for source := range m {
			seen[source] = true
		}
	}
	for source := range seen {
		// Anything with a percent sign is read as a template: %% is how a format
		// string spells a literal percent. One that does not parse (a bare "100% sure")
		// is plain text.
		if strings.Contains(source, "%") {
			if m, ok := newMatcher(source); ok {
				c.matchers = append(c.matchers, m)
				continue
			}
		}
		c.exact[source] = true
	}
	// The most specific template wins when two could match the same message:
	// "failed to save %s" must not shadow "failed to save %s to disk".
	sort.SliceStable(c.matchers, func(i, j int) bool {
		if c.matchers[i].weight != c.matchers[j].weight {
			return c.matchers[i].weight > c.matchers[j].weight
		}
		return c.matchers[i].source < c.matchers[j].source
	})
	return c
}

// Languages returns the language codes that have a catalog file, sorted.
func Languages() []string {
	c := load()
	out := make([]string, 0, len(c.byLang))
	for l := range c.byLang {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// Catalog returns the translations for a language, keyed by English message. The
// map is the live one and must not be modified.
func Catalog(lang string) map[string]string {
	return load().byLang[lang]
}

// normalize reduces a language or locale tag to a catalog code, or "" for
// English and anything without a catalog.
func normalize(lang string) string {
	code, ok := replylang.ParseLocale(lang)
	if !ok || code == "en" {
		return ""
	}
	if _, has := load().byLang[code]; !has {
		return ""
	}
	return code
}

// T renders a message in lang. format is the English message, written the way
// fmt.Sprintf takes it; args fill its verbs. With no catalog entry for lang the
// English is rendered, so a missing translation degrades to readable English.
//
// With no args the message is returned as written: it is plain text, and a
// percent sign in it is not a directive.
func T(lang, format string, args ...any) string {
	text := format
	if code := normalize(lang); code != "" {
		if tr, ok := load().byLang[code][format]; ok && tr != "" {
			text = tr
		}
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// Translate maps a finished message to lang. It recognises a message by exact text
// or by the template that produced it, and returns anything else unchanged, which
// is the right outcome for text that came from a model, a database, or the
// operating system.
//
// A text value inside a recognised template is translated too when it is itself a
// known message, so "port 80: <router message>" comes out in one language.
func Translate(lang, msg string) string {
	code := normalize(lang)
	if code == "" || msg == "" {
		return msg
	}
	return translate(code, msg, true)
}

func translate(code, msg string, nested bool) string {
	c := load()
	table := c.byLang[code]
	if c.exact[msg] {
		if tr := table[msg]; tr != "" {
			return tr
		}
		return msg
	}
	for _, m := range c.matchers {
		args, ok := m.match(msg)
		if !ok {
			continue
		}
		if tr := table[m.source]; tr != "" {
			if nested {
				for i, a := range args {
					if s, isText := a.(string); isText {
						args[i] = translate(code, s, false)
					}
				}
			}
			return render(tr, args)
		}
	}
	return msg
}

// Mark returns s unchanged. It tags a message that is written in one place and
// shown from another (a table of lines, a message a function returns) so the
// coverage test knows it needs a translation. The wrapped text must be the
// message exactly as it will be shown, or a template when it takes arguments.
func Mark(s string) string { return s }
