package middleware

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

type requestIDKey struct{}

// maxRequestIDLen bounds a client-supplied X-Request-ID that is reflected back
// into the response header and logs.
const maxRequestIDLen = 128

// RequestID generates a UUID request ID for each request and adds it to the
// context and the X-Request-ID response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		// A client-supplied id is reflected into the response and logs, so
		// reject anything over the length cap or carrying control/non-ASCII
		// characters (header-splitting / log-injection surface) and mint a
		// fresh one instead.
		if id == "" || len(id) > maxRequestIDLen || !isSafeRequestID(id) {
			id = uuid.New().String()
		}

		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isSafeRequestID allows only printable ASCII without spaces, so a reflected id
// cannot inject headers or break log lines.
func isSafeRequestID(s string) bool {
	for _, c := range s {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}

// GetRequestID extracts the request ID from the context.
func GetRequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
