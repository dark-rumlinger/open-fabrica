package msgcat

import (
	"context"
	"net/http"

	"openfabrica/internal/replylang"
)

type ctxKey struct{}

// RequestLocale returns the catalog language a request asks for, from the
// interface language header. It is "" for English, for a request that did not say,
// and for a language with no catalog, which are all the cases that get the source
// text.
func RequestLocale(r *http.Request) string {
	return normalize(r.Header.Get(replylang.Header))
}

// Locale returns the language Middleware resolved for the request, or "".
func Locale(r *http.Request) string {
	lang, _ := r.Context().Value(ctxKey{}).(string)
	return lang
}

// Localize translates a finished message for the request's language. Use it for
// the few messages that leave a handler as a field of a JSON body rather than as
// an error response, which the writer translates by itself.
func Localize(r *http.Request, msg string) string {
	return Translate(Locale(r), msg)
}

// langProvider is implemented by the response writer Middleware installs.
type langProvider interface{ msgcatLang() string }

// LocalizeFor translates msg for the language of the request that w answers. It
// walks the Unwrap chain, so a writer wrapped by other middleware after this one
// still finds it. With no language in play it returns msg unchanged.
func LocalizeFor(w http.ResponseWriter, msg string) string {
	for w != nil {
		if p, ok := w.(langProvider); ok {
			return Translate(p.msgcatLang(), msg)
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return msg
		}
		w = u.Unwrap()
	}
	return msg
}

// Middleware resolves the request's language once. A request in English, or in a
// language with no catalog, passes through untouched: no wrapper, no context
// value, no cost.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := RequestLocale(r)
		if lang == "" {
			next.ServeHTTP(w, r)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, lang))
		base := &writer{ResponseWriter: w, lang: lang}
		// Offer Flush only when the underlying writer can, so a handler that checks
		// for http.Flusher gets the same answer it would have got without us.
		if f, ok := w.(http.Flusher); ok {
			next.ServeHTTP(&flushWriter{writer: base, flusher: f}, r)
			return
		}
		next.ServeHTTP(base, r)
	})
}

// writer is the response writer a localized request is served through. It adds no
// behavior of its own; it only remembers the language so the error helpers, which
// receive nothing but the writer, can translate. Unwrap keeps
// http.ResponseController (deadlines, hijacking) working through it.
type writer struct {
	http.ResponseWriter
	lang string
}

func (w *writer) msgcatLang() string { return w.lang }

func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// flushWriter is writer for a response that can stream.
type flushWriter struct {
	*writer
	flusher http.Flusher
}

func (w *flushWriter) Flush() { w.flusher.Flush() }
