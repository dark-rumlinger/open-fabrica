package httpauth

import (
	"net/http"
	"testing"
)

func TestSetBearer(t *testing.T) {
	t.Run("non-empty key sets header", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "http://x", nil)
		SetBearer(req, "sk-123")
		if got := req.Header.Get("Authorization"); got != "Bearer sk-123" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer sk-123")
		}
	})

	t.Run("empty key leaves header unset", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "http://x", nil)
		SetBearer(req, "")
		if _, ok := req.Header["Authorization"]; ok {
			t.Fatalf("Authorization header should be absent for a blank key, got %q", req.Header.Get("Authorization"))
		}
	})
}
