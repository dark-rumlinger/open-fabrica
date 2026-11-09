package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWithHealthBypassesRouter: /health and /healthz must answer 200 without
// ever reaching the wrapped handler (which here would 500), proving the probe
// bypasses project resolution / rate limiting / auth.
func TestWithHealthBypassesRouter(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("wrapped handler reached for %s %s; health should short-circuit", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := withHealth(inner, func() error { return nil })

	for _, path := range []string{"/health", "/healthz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s Content-Type = %q, want application/json", path, ct)
		}
	}
}

// Non-health paths must fall through to the wrapped handler.
func TestWithHealthFallsThrough(t *testing.T) {
	reached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusTeapot)
	})
	h := withHealth(inner, func() error { return nil })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/app/page", nil))
	if !reached {
		t.Fatal("wrapped handler was not reached for a non-health path")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("fall-through status = %d, want 418", rec.Code)
	}
}

// TestWithHealthReadiness: /ready and /readyz reflect the readiness check,
// 200 when it passes, 503 when it fails, and never reach the wrapped handler.
func TestWithHealthReadiness(t *testing.T) {
	failing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("wrapped handler reached for %s %s; readiness should short-circuit", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	for _, path := range []string{"/ready", "/readyz"} {
		// Dependency healthy -> 200.
		okH := withHealth(failing, func() error { return nil })
		rec := httptest.NewRecorder()
		okH.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s (healthy) = %d, want 200", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s Content-Type = %q, want application/json", path, ct)
		}

		// Dependency down -> 503.
		downH := withHealth(failing, func() error { return errDependencyDown })
		rec = httptest.NewRecorder()
		downH.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s (down) = %d, want 503", path, rec.Code)
		}
	}
}

var errDependencyDown = errorString("dependency down")

type errorString string

func (e errorString) Error() string { return string(e) }
