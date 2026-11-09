package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A local (project) fetch must not be walked to another host by a redirect.
func TestLocalClientRefusesRedirectsOffHost(t *testing.T) {
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("internal-only"))
	}))
	defer secret.Close()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/landing", http.StatusFound)
		case "/landing":
			w.Write([]byte("ok"))
		default:
			http.Redirect(w, r, secret.URL+"/admin", http.StatusFound)
		}
	}))
	defer app.Close()

	client := ssrfSafeClient(true)
	if resp, err := client.Get(app.URL + "/same"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("same-host redirect should be followed: %v", err)
	}
	if resp, err := client.Get(app.URL + "/escape"); err == nil {
		resp.Body.Close()
		t.Fatal("redirect to another host was followed")
	}
}
