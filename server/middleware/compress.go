package middleware

import (
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// Compress gzip/deflate-encodes text-ish responses for clients that advertise
// support via Accept-Encoding. text/event-stream is deliberately absent and
// hijacked connections are left untouched, so SSE and WebSocket streaming
// (the tlsproxy's immediate-flush behavior) is preserved.
func Compress(next http.Handler) http.Handler {
	return chimw.Compress(5,
		"text/html",
		"text/css",
		"text/plain",
		"text/javascript",
		"application/javascript",
		"application/json",
		"application/xml",
		"image/svg+xml",
	)(next)
}
