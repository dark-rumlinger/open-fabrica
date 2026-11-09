// Package httpx holds the canonical HTTP response helpers shared by the admin
// and public servers: one JSON writer, one error envelope, and one body-capped
// JSON decoder. Every server response funnels through here, so a second copy of
// a writer is never the reason two endpoints answer in different shapes.
package httpx

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"openfabrica/internal/msgcat"
)

// ExtendIO pushes the connection's read and write deadlines out to d from now.
// The servers' blanket 30 s read / 120 s write timeouts suit ordinary API calls
// but cut large uploads, project exports and big file downloads mid-stream.
// Best effort: a writer without deadline support (tests, wrappers) is left as is.
func ExtendIO(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(d)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
}

// MaxBodySize caps how many bytes a JSON request body may carry. Generous
// enough for the largest artifact bodies (page/component/layout/file content)
// while preventing an oversized payload from buffering unbounded into memory.
// Bulk import/export paths set their own, larger limits.
const MaxBodySize = 10 << 20 // 10 MB

// ErrorResponse is the single JSON error envelope every endpoint returns.
type ErrorResponse struct {
	Error string `json:"error"`
}

// WriteJSON writes v as a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes a JSON error response of the form {"error": msg}. When the
// request was served through msgcat.Middleware the message is translated into the
// caller's language here, which is why handlers keep writing plain English.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, ErrorResponse{Error: msgcat.LocalizeFor(w, msg)})
}

// DecodeJSON decodes a JSON request body into v, capping the body at
// MaxBodySize. On failure it writes a 400 error response and returns false
// (the caller should simply return).
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodySize)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// WantsHTML reports whether the client prefers an HTML response over JSON,
// based on the Accept header.
func WantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
