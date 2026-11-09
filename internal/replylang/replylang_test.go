package replylang

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseLocale(t *testing.T) {
	cases := []struct {
		in   string
		code string
		ok   bool
	}{
		{"es", "es", true},
		{"ES", "es", true},
		{" zh-CN ", "zh", true},
		{"pt_BR", "pt", true},
		{"ar", "ar", true},
		{"", "", false},
		{"xx", "", false},
		{"match", "", false},
		{"english", "", false},
		{"es; drop table", "", false},
	}
	for _, c := range cases {
		code, ok := ParseLocale(c.in)
		if code != c.code || ok != c.ok {
			t.Errorf("ParseLocale(%q) = %q, %v; want %q, %v", c.in, code, ok, c.code, c.ok)
		}
	}
}

func TestNormalizePreference(t *testing.T) {
	for _, in := range []string{"", Match, "MATCH", " fr ", "ja", "zh-CN"} {
		if _, ok := NormalizePreference(in); !ok {
			t.Errorf("NormalizePreference(%q) rejected a valid preference", in)
		}
	}
	for _, in := range []string{"klingon", "match me", "e", "es es"} {
		if _, ok := NormalizePreference(in); ok {
			t.Errorf("NormalizePreference(%q) accepted an invalid preference", in)
		}
	}
	if got, _ := NormalizePreference(" MATCH "); got != Match {
		t.Errorf("match was not canonicalised: %q", got)
	}
	if got, _ := NormalizePreference("pt_BR"); got != "pt" {
		t.Errorf("a tag was not reduced to its code: %q", got)
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		pref, locale string
		want         string
		decided      bool
	}{
		// Following the interface: the interface language wins unless it is English.
		{Interface, "es", "es", true},
		{Interface, "zh-CN", "zh", true},
		{Interface, "ar", "ar", true},
		{Interface, "en", "", true},
		// Nothing known about the interface: no decision, so a stored value survives.
		{Interface, "", "", false},
		{Interface, "xx", "", false},
		// An explicit choice ignores the interface language entirely.
		{Match, "es", Match, true},
		{Match, "", Match, true},
		{"de", "es", "de", true},
		{"de", "", "de", true},
		{"en", "es", "en", true},
		{"en", "en", "en", true},
		// A damaged row decides nothing rather than storing an invalid directive.
		{"klingon", "es", "", false},
	}
	for _, c := range cases {
		got, decided := Resolve(c.pref, c.locale)
		if got != c.want || decided != c.decided {
			t.Errorf("Resolve(%q, %q) = %q, %v; want %q, %v", c.pref, c.locale, got, decided, c.want, c.decided)
		}
	}
}

// Every directive Resolve can produce must be one Line understands, or the engine
// would store a value it then silently renders as nothing.
func TestResolveOutputsAreRenderable(t *testing.T) {
	prefs := []string{Interface, Match}
	for _, l := range Languages() {
		prefs = append(prefs, l.Code)
	}
	for _, pref := range prefs {
		for _, l := range append(Languages(), Language{}) {
			d, decided := Resolve(pref, l.Code)
			if !decided || d == "" {
				if Line(d) != "" || Reminder(d) != "" {
					t.Errorf("empty directive rendered text for pref %q locale %q", pref, l.Code)
				}
				continue
			}
			if Line(d) == "" || Reminder(d) == "" {
				t.Errorf("directive %q (pref %q, locale %q) renders nothing", d, pref, l.Code)
			}
		}
	}
}

func TestLine(t *testing.T) {
	if Line("") != "" {
		t.Error("no directive must render no line")
	}
	if Line("klingon") != "" {
		t.Error("an unknown directive must render no line")
	}
	es := Line("es")
	if !strings.HasPrefix(es, "- Reply language: Spanish.") {
		t.Errorf("named line should lead with the language: %q", es)
	}
	match := Line(Match)
	if !strings.Contains(match, "language of their latest message") {
		t.Errorf("match line should say to follow the latest message: %q", match)
	}
	for name, line := range map[string]string{"es": es, "match": match} {
		if strings.ContainsRune(line, '\n') {
			t.Errorf("%s line must be a single bullet: %q", name, line)
		}
		for _, must := range []string{"tool arguments", "identifiers", "in English", "site's own content"} {
			if !strings.Contains(line, must) {
				t.Errorf("%s line lost %q: %q", name, must, line)
			}
		}
		if strings.ContainsRune(line, '—') {
			t.Errorf("%s line contains an em dash", name)
		}
	}
}

func TestReminder(t *testing.T) {
	if Reminder("") != "" || Reminder("klingon") != "" {
		t.Error("no valid directive must render no reminder")
	}
	if got := Reminder("ja"); !strings.Contains(got, "Japanese") {
		t.Errorf("reminder should name the language: %q", got)
	}
	if got := Reminder(Match); !strings.Contains(got, "owner wrote") {
		t.Errorf("match reminder should refer to the message: %q", got)
	}
}

func TestLanguagesIsACopy(t *testing.T) {
	ls := Languages()
	ls[0].Code = "zz"
	if Languages()[0].Code == "zz" {
		t.Error("Languages exposed its backing slice")
	}
}

// The admin UI offers a language in its interface only if the server accepts it
// as a reply language. Reading the client list keeps the two from drifting.
func TestMatchesAdminUILanguages(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "admin", "app", "core", "i18n.js"))
	if err != nil {
		t.Fatalf("read i18n.js: %v", err)
	}
	block := regexp.MustCompile(`(?s)export const SUPPORTED = \[(.*?)\];`).FindSubmatch(src)
	if block == nil {
		t.Fatal("SUPPORTED list not found in i18n.js")
	}
	var ui []string
	for _, m := range regexp.MustCompile(`code:\s*'([a-z]+)'`).FindAllSubmatch(block[1], -1) {
		ui = append(ui, string(m[1]))
	}
	if len(ui) == 0 {
		t.Fatal("no UI languages parsed")
	}
	server := map[string]bool{}
	for _, l := range Languages() {
		server[l.Code] = true
	}
	for _, code := range ui {
		if !server[code] {
			t.Errorf("UI language %q is not a supported reply language", code)
		}
		delete(server, code)
	}
	for code := range server {
		t.Errorf("reply language %q has no matching UI language", code)
	}
}
