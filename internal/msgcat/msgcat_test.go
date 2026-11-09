package msgcat

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/internal/replylang"
)

// useCatalogs swaps in test catalogs for one test.
func useCatalogs(t *testing.T, byLang map[string]map[string]string) {
	t.Helper()
	load() // settle the real load first so it cannot overwrite the swap
	saved := loaded
	loaded = build(byLang)
	t.Cleanup(func() { loaded = saved })
}

var testCatalogs = map[string]map[string]string{
	"es": {
		"project not found":                     "proyecto no encontrado",
		"failed to create project database: %v": "no se pudo crear la base de datos del proyecto: %v",
		"%d questions need your answer":         "%d preguntas necesitan tu respuesta",
		"%q is not a valid mode":                "%q no es un modo válido",
		"Try %d of %d. Retrying now.":           "Intento %[1]d de %[2]d. Reintentando.",
		"stage %s failed after %d tries":        "%[2]d intentos fallidos en la etapa %[1]s",
		"failed to save %s":                     "no se pudo guardar %s",
		"failed to save %s to disk":             "no se pudo guardar %s en el disco",
		"port %d: %w":                           "puerto %d: %s",
		"progress is 100%% done":                "el progreso está al 100%% completado",
		"100% sure":                             "100% seguro",
		"untranslated here":                     "",
		"Hello, {name}":                         "Hola, {name}",
	},
	"fr": {
		"project not found": "projet introuvable",
	},
}

func TestTranslateExact(t *testing.T) {
	useCatalogs(t, testCatalogs)
	cases := []struct{ lang, in, want string }{
		{"es", "project not found", "proyecto no encontrado"},
		{"fr", "project not found", "projet introuvable"},
		{"en", "project not found", "project not found"},
		{"", "project not found", "project not found"},
		{"xx", "project not found", "project not found"},
		{"de", "project not found", "project not found"}, // no catalog for the language
		{"es", "something nobody wrote down", "something nobody wrote down"},
		{"es", "", ""},
		{"es", "100% sure", "100% seguro"},               // a bare percent sign is plain text
		{"es", "untranslated here", "untranslated here"}, // empty entry means no translation
		{"zh-CN", "project not found", "project not found"},
		{"ES", "project not found", "proyecto no encontrado"},
	}
	for _, c := range cases {
		if got := Translate(c.lang, c.in); got != c.want {
			t.Errorf("Translate(%q, %q) = %q, want %q", c.lang, c.in, got, c.want)
		}
	}
}

func TestTranslateTemplates(t *testing.T) {
	useCatalogs(t, testCatalogs)
	cases := []struct{ in, want string }{
		{"failed to create project database: disk full", "no se pudo crear la base de datos del proyecto: disk full"},
		// The recovered value may span lines and contain colons.
		{"failed to create project database: a: b\nc", "no se pudo crear la base de datos del proyecto: a: b\nc"},
		{"3 questions need your answer", "3 preguntas necesitan tu respuesta"},
		{`"turbo" is not a valid mode`, `"turbo" no es un modo válido`},
		{"Try 2 of 5. Retrying now.", "Intento 2 de 5. Reintentando."},
		// A translation may reorder its values.
		{"stage build failed after 3 tries", "3 intentos fallidos en la etapa build"},
		// The more specific template wins.
		{"failed to save page to disk", "no se pudo guardar page en el disco"},
		{"failed to save page", "no se pudo guardar page"},
		// A literal percent sign survives.
		{"progress is 100% done", "el progreso está al 100% completado"},
		// Text that merely resembles a template is left alone.
		{"3 questions need your answers please", "3 questions need your answers please"},
	}
	for _, c := range cases {
		if got := Translate("es", c.in); got != c.want {
			t.Errorf("Translate(es, %q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// A message that wraps another message translates both halves.
func TestTranslateNestedMessages(t *testing.T) {
	useCatalogs(t, testCatalogs)
	if got := Translate("es", "port 80: project not found"); got != "puerto 80: proyecto no encontrado" {
		t.Errorf("nested: got %q", got)
	}
	// Text that is not a known message stays as it was.
	if got := Translate("es", "port 80: connection reset by peer"); got != "puerto 80: connection reset by peer" {
		t.Errorf("unknown inner text: got %q", got)
	}
}

func TestTranslateKeepsMessagesTheLanguageLacks(t *testing.T) {
	useCatalogs(t, testCatalogs)
	// fr knows the plain message but not the templates; the template falls back
	// to English instead of borrowing another language.
	if got := Translate("fr", "3 questions need your answer"); got != "3 questions need your answer" {
		t.Errorf("got %q", got)
	}
}

func TestT(t *testing.T) {
	useCatalogs(t, testCatalogs)
	cases := []struct {
		lang, format string
		args         []any
		want         string
	}{
		{"es", "Try %d of %d. Retrying now.", []any{2, 5}, "Intento 2 de 5. Reintentando."},
		{"en", "Try %d of %d. Retrying now.", []any{2, 5}, "Try 2 of 5. Retrying now."},
		{"", "Try %d of %d. Retrying now.", []any{2, 5}, "Try 2 of 5. Retrying now."},
		{"fr", "Try %d of %d. Retrying now.", []any{2, 5}, "Try 2 of 5. Retrying now."},
		{"es", "stage %s failed after %d tries", []any{"build", 3}, "3 intentos fallidos en la etapa build"},
		{"es", "project not found", nil, "proyecto no encontrado"},
		// No args: plain text, so a percent sign is not a directive.
		{"en", "100% sure", nil, "100% sure"},
		{"es", "unknown message", nil, "unknown message"},
	}
	for _, c := range cases {
		if got := T(c.lang, c.format, c.args...); got != c.want {
			t.Errorf("T(%q, %q, %v) = %q, want %q", c.lang, c.format, c.args, got, c.want)
		}
	}
}

func TestSameVerbs(t *testing.T) {
	ok := [][2]string{
		{"a %s b", "x %s y"},
		{"a %v", "x %s"},
		{"%d of %d", "%[2]d / %[1]d"},
		{"no verbs", "sin verbos"},
		{"100%% sure", "100%% seguro"},
		{"%q", "«%q»"},
	}
	for _, c := range ok {
		if !SameVerbs(c[0], c[1]) {
			t.Errorf("SameVerbs(%q, %q) = false, want true", c[0], c[1])
		}
	}
	bad := [][2]string{
		{"a %s", "a"},
		{"a", "a %s"},
		{"%d", "%s"},
		{"%s %s", "%s"},
		{"%s", "%q"},
		{"%s", "%[2]s"},
		{"%s", "%z"},
		{"%s", "trailing %"},
	}
	for _, c := range bad {
		if SameVerbs(c[0], c[1]) {
			t.Errorf("SameVerbs(%q, %q) = true, want false", c[0], c[1])
		}
	}
}

func TestRequestLocale(t *testing.T) {
	useCatalogs(t, testCatalogs)
	mk := func(header, query string) *http.Request {
		r := httptest.NewRequest("GET", "/x?"+query, nil)
		if header != "" {
			r.Header.Set(replylang.Header, header)
		}
		return r
	}
	cases := []struct {
		header, query, want string
	}{
		{"es", "", "es"},
		{"ES", "", "es"},
		{"", "locale=es", ""}, // only the header counts
		{"en", "", ""},
		{"", "", ""},
		{"de", "", ""}, // supported UI language, but no catalog
		{"klingon", "", ""},
	}
	for _, c := range cases {
		if got := RequestLocale(mk(c.header, c.query)); got != c.want {
			t.Errorf("RequestLocale(header %q, query %q) = %q, want %q", c.header, c.query, got, c.want)
		}
	}
}

// flusherRecorder is httptest.ResponseRecorder, which already flushes.
func TestMiddlewareLocalizesErrorsAndPreservesStreaming(t *testing.T) {
	useCatalogs(t, testCatalogs)
	var sawLocale string
	var flusher bool
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawLocale = Locale(r)
		_, flusher = w.(http.Flusher)
		w.Write([]byte(LocalizeFor(w, "project not found")))
		w.(http.Flusher).Flush()
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(replylang.Header, "es")
	h.ServeHTTP(rec, req)
	if sawLocale != "es" || rec.Body.String() != "proyecto no encontrado" || !flusher || !rec.Flushed {
		t.Errorf("locale=%q body=%q flusher=%v flushed=%v", sawLocale, rec.Body.String(), flusher, rec.Flushed)
	}

	// English: the handler sees the original writer and no locale.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if sawLocale != "" || rec.Body.String() != "project not found" {
		t.Errorf("english request: locale=%q body=%q", sawLocale, rec.Body.String())
	}
}

// nonFlusher is a response writer with no Flush.
type nonFlusher struct{ http.ResponseWriter }

func TestMiddlewareDoesNotInventFlush(t *testing.T) {
	useCatalogs(t, testCatalogs)
	var flusher bool
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, flusher = w.(http.Flusher)
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(replylang.Header, "es")
	h.ServeHTTP(nonFlusher{httptest.NewRecorder()}, req)
	if flusher {
		t.Error("a writer that cannot flush must not start looking like one")
	}
}

// outer is a middleware wrapper that knows nothing about msgcat but unwraps.
type outer struct{ http.ResponseWriter }

func (o outer) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func TestLocalizeForFindsTheWriterThroughOtherWrappers(t *testing.T) {
	useCatalogs(t, testCatalogs)
	var got string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = LocalizeFor(outer{outer{w}}, "project not found")
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(replylang.Header, "fr")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "projet introuvable" {
		t.Errorf("got %q", got)
	}
	if LocalizeFor(httptest.NewRecorder(), "project not found") != "project not found" {
		t.Error("a plain writer must leave the message alone")
	}
	if LocalizeFor(nil, "x") != "x" {
		t.Error("a nil writer must not panic")
	}
}

func TestLocalizeUsesTheRequestContext(t *testing.T) {
	useCatalogs(t, testCatalogs)
	var got string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = Localize(r, "project not found")
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(replylang.Header, "es")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "proyecto no encontrado" {
		t.Errorf("got %q", got)
	}
	if Localize(httptest.NewRequest("GET", "/x", nil), "project not found") != "project not found" {
		t.Error("a request outside the middleware must be left alone")
	}
}

// The real catalogs must load, and a language file must be named for a language
// the product knows.
func TestRealCatalogsLoad(t *testing.T) {
	known := map[string]bool{}
	for _, l := range replylang.Languages() {
		known[l.Code] = true
	}
	for _, lang := range Languages() {
		if !known[lang] || lang == "en" {
			t.Errorf("catalog file %s.json is not for a supported non-English language", lang)
		}
	}
	if len(Languages()) == 0 {
		t.Fatal("no catalogs embedded")
	}
	_ = bufio.NewReader(strings.NewReader("")) // keep imports honest across edits
}
