package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openfabrica/internal/httpx"
)

// A handler that calls httpx.ExtendIO must keep its connection past the server's
// blanket WriteTimeout even when the response writer has been wrapped by the
// production middleware (compression, request logging). The wrappers have to pass
// deadline control through to the real connection, or the extension is silently a
// no-op and long exports and uploads still die at the default timeout.
func TestExtendIOSurvivesTheMiddlewareChain(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("extend") == "1" {
			httpx.ExtendIO(w, 10*time.Second)
		}
		time.Sleep(600 * time.Millisecond) // longer than the server's WriteTimeout below
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "finished")
	})
	chain := Compress(SecurityHeaders(Logging(slog.New(slog.NewTextHandler(io.Discard, nil)))(handler)))

	srv := httptest.NewUnstartedServer(chain)
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	defer srv.Close()

	get := func(query string) (string, error) {
		resp, err := http.Get(srv.URL + "/?" + query)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}

	// Sanity: without the extension this server really does cut the response, so the
	// test below proves something.
	if body, err := get("extend=0"); err == nil && body == "finished" {
		t.Fatal("the control request survived the WriteTimeout; the test cannot tell anything")
	}
	if body, err := get("extend=1"); err != nil || body != "finished" {
		t.Fatalf("extended request: body %q, err %v; the deadline extension did not reach the connection", body, err)
	}
}
