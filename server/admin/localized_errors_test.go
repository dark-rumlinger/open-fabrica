package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"openfabrica/internal/msgcat"
	"openfabrica/internal/portmap"
	"openfabrica/internal/replylang"
	"openfabrica/security"
)

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

// The sign-in guard answers before any admin handler runs, so it is the first
// message a signed-out person sees. It must arrive in their language.
func TestAuthGuardErrorsAreTranslated(t *testing.T) {
	jwt := security.NewJWTManager("test-jwt-secret-at-least-32-bytes-long", time.Hour)
	h := msgcat.Middleware(jwt.Authenticator(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an unauthenticated request reached the handler")
	})))

	for _, lang := range []string{"es", "fr", "ru", "ja", "ar"} {
		req := httptest.NewRequest("GET", "/admin/api/projects", nil)
		req.Header.Set(replylang.Header, lang)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d", lang, rec.Code)
		}
		want := msgcat.Catalog(lang)["missing authorization"]
		if want == "" || errorOf(t, rec) != want {
			t.Errorf("%s: got %q, want %q", lang, errorOf(t, rec), want)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/projects", nil))
	if got := errorOf(t, rec); got != "missing authorization" {
		t.Errorf("english: got %q", got)
	}
}

// Handlers keep writing plain English through writeError and serverError.
func TestHandlerHelpersAreTranslated(t *testing.T) {
	e := newReplyTestEnv(t)
	h := msgcat.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plain":
			writeError(w, http.StatusNotFound, "project not found")
		case "/server":
			e.deps.serverError(w, "failed to list projects", nil)
		}
	}))
	for path, source := range map[string]string{"/plain": "project not found", "/server": "failed to list projects"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set(replylang.Header, "pt")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		want := msgcat.Catalog("pt")[source]
		if want == "" || errorOf(t, rec) != want {
			t.Errorf("%s: got %q, want %q", path, errorOf(t, rec), want)
		}
	}
}

// The provider test endpoints answer 200 with the problem in the body, so they
// translate it themselves.
func TestLocalizeCoversJSONBodies(t *testing.T) {
	var got string
	h := msgcat.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = msgcat.Localize(r, "no models configured for this provider")
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(replylang.Header, "ja")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if want := msgcat.Catalog("ja")["no models configured for this provider"]; want == "" || got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The middleware is only effective if the admin router installs it. The router
// needs a full server to build, so pin the line instead.
func TestAdminRouterInstallsTheTranslator(t *testing.T) {
	src, err := os.ReadFile("../server.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "s.adminRouter.Use(msgcat.Middleware)") {
		t.Error("server.go no longer installs msgcat.Middleware on the admin router")
	}
}

func localizedRequest(path, lang string) *http.Request {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set(replylang.Header, lang)
	return r
}

// The domain check explains itself in the caller's language, and the value it
// quotes stays as typed.
func TestDomainValidationIsTranslated(t *testing.T) {
	var out struct {
		Valid bool   `json:"valid"`
		Error string `json:"error"`
	}
	run := func(lang string) string {
		var got string
		h := msgcat.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			(&ProjectsHandler{}).ValidateDomain(w, r)
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localizedRequest("/admin/api/domain/validate?domain=my%20shop.example", lang))
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		got = out.Error
		return got
	}
	if got := run("en"); got != "domain 'my shop.example' contains spaces" {
		t.Fatalf("english: %q", got)
	}
	if got := run("de"); got != "die Domain „my shop.example“ enthält Leerzeichen" {
		t.Errorf("german: %q", got)
	}
}

func TestRouterStatusReasonIsTranslatedInFull(t *testing.T) {
	var got string
	h := msgcat.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := localizedPortStatus(r, portmap.Status{Error: "port 80: the router already forwards this port to another device"})
		got = st.Error
	}))
	h.ServeHTTP(httptest.NewRecorder(), localizedRequest("/x", "es"))
	if want := "puerto 80: el router ya reenvía este puerto a otro dispositivo"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLocalizeAllKeepsNilNil(t *testing.T) {
	r := localizedRequest("/x", "fr")
	if localizeAll(r, nil) != nil {
		t.Error("a nil list must stay nil")
	}
	h := msgcat.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := localizeAll(r, []string{"public CSP is the permissive default", "something else"})
		if got[0] == "public CSP is the permissive default" || got[1] != "something else" {
			t.Errorf("got %q", got)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), r)
}
