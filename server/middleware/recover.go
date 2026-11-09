package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover returns middleware that catches panics in downstream handlers, logs
// them with stack trace + request context, and writes a generic 500. Wire this
// just inside Logging so the log line still records the 500 status. The error
// body is intentionally generic; the stack goes to the logger only.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the documented escape hatch for
				// handlers that want to bail without writing a response;
				// don't treat it as a panic.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				logger.Error("panic recovered",
					"panic", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"request_id", GetRequestID(r.Context()),
					"stack", string(debug.Stack()),
				)
				// Best-effort 500. If headers were already flushed (streaming
				// handler), WriteHeader is a no-op and the client just sees
				// a truncated response.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal server error"}`))
			}()
			next.ServeHTTP(w, r)
		})
	}
}
