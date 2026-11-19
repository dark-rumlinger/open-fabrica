package content

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openfabrica/tools/tooltest"
)

func withTestFetchClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	origClient := webFetchClientFactory
	origValidate := webFetchValidateURL
	webFetchClientFactory = func() *http.Client { return srv.Client() }
	webFetchValidateURL = func(string) error { return nil }
	t.Cleanup(func() {
		webFetchClientFactory = origClient
		webFetchValidateURL = origValidate
	})
}

func TestWebFetch_ExtractsHTMLAndPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html>
		<head><title>Go Guide</title><style>.x{color:red}</style></head>
		<body>
			<nav>Home | About</nav>
			<main>
				<h1>Effective Go</h1>
				<p>Read the <a href="https://go.dev/doc/">official docs</a> and use <code>gofmt</code>.</p>
				<pre><code>fmt.Println("hi")</code></pre>
			</main>
			<footer>Copyright 2026</footer>
			<script>console.log("ignore")</script>
		</body></html>`))
	}))
	defer srv.Close()
	withTestFetchClient(t, srv)

	h := tooltest.New(t)
	res := h.MustOK("manage_web_fetch", map[string]any{
		"action":    "fetch",
		"url":       srv.URL + "/guide",
		"max_chars": 500,
	})
	data := res.Data.(map[string]any)
	if data["title"] != "Go Guide" {
		t.Errorf("title = %v, want 'Go Guide'", data["title"])
	}
	content, _ := data["content"].(string)
	if !strings.Contains(content, "# Effective Go") {
		t.Errorf("expected heading in content, got:\n%s", content)
	}
	if !strings.Contains(content, "[official docs](https://go.dev/doc/)") {
		t.Errorf("expected markdown link in content, got:\n%s", content)
	}
	if strings.Contains(content, "Home | About") || strings.Contains(content, "Copyright 2026") || strings.Contains(content, "console.log") {
		t.Errorf("expected nav/footer/script stripped, got:\n%s", content)
	}
}

func TestWebFetch_ParsesRSSFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
		<rss version="2.0"><channel>
			<title>Release Notes</title>
			<item>
				<title>v1.2.0</title>
				<link>https://example.com/v1.2.0</link>
				<pubDate>Tue, 18 Nov 2026 10:00:00 GMT</pubDate>
				<description>&lt;p&gt;Added web fetch.&lt;/p&gt;</description>
			</item>
		</channel></rss>`))
	}))
	defer srv.Close()
	withTestFetchClient(t, srv)

	h := tooltest.New(t)
	res := h.MustOK("manage_web_fetch", map[string]any{
		"action": "fetch",
		"url":    srv.URL + "/feed.xml",
	})
	data := res.Data.(map[string]any)
	if data["title"] != "Release Notes" {
		t.Errorf("title = %v", data["title"])
	}
	content, _ := data["content"].(string)
	if !strings.Contains(content, "**v1.2.0**") || !strings.Contains(content, "Added web fetch.") {
		t.Errorf("unexpected feed content:\n%s", content)
	}
}

func TestWebFetch_RejectsBadURLAndUnsupportedContent(t *testing.T) {
	h := tooltest.New(t)
	res := h.MustFail("manage_web_fetch", map[string]any{
		"action": "fetch",
		"url":    "file:///etc/passwd",
	}, "")
	if !strings.Contains(res.Error, "http or https") {
		t.Errorf("unexpected error: %q", res.Error)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0x00, 0x01, 0x02})
	}))
	defer srv.Close()
	withTestFetchClient(t, srv)

	res = h.MustFail("manage_web_fetch", map[string]any{
		"action": "fetch",
		"url":    srv.URL + "/bin",
	}, "")
	if !strings.Contains(res.Error, "unsupported content-type") {
		t.Errorf("unexpected error: %q", res.Error)
	}
}

func TestWebFetch_RetriesOn429(t *testing.T) {
	origWait := webFetchDefaultRetryWait
	webFetchDefaultRetryWait = 5 * time.Millisecond
	t.Cleanup(func() { webFetchDefaultRetryWait = origWait })

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("recovered content"))
	}))
	defer srv.Close()
	withTestFetchClient(t, srv)

	h := tooltest.New(t)
	res := h.MustOK("manage_web_fetch", map[string]any{
		"action": "fetch",
		"url":    srv.URL + "/rate-limited",
	})
	data := res.Data.(map[string]any)
	if data["content"] != "recovered content" {
		t.Errorf("content = %v, want 'recovered content'", data["content"])
	}
	if hits.Load() != 2 {
		t.Errorf("server hits = %d, want 2", hits.Load())
	}
}

func TestWebFetch_DetectsBotInterstitial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html><title>Just a moment...</title><body>Enable JavaScript and cookies to continue (cf-browser-verification)</body></html>`))
	}))
	defer srv.Close()
	withTestFetchClient(t, srv)

	h := tooltest.New(t)
	res := h.MustFail("manage_web_fetch", map[string]any{
		"action": "fetch",
		"url":    srv.URL + "/protected",
	}, "")
	if !strings.Contains(res.Error, "anti-bot challenge") {
		t.Errorf("unexpected error: %q", res.Error)
	}
}

func TestWebFetch_ExtractsDesignTokensNavAndFeeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/styles.css" {
			w.Header().Set("Content-Type", "text/css")
			_, _ = w.Write([]byte(`:root { --brand-red: #cc0000; --bg: #ffffff; } body { font-family: 'Inter', sans-serif; color: #111111; background: #ffffff; }`))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html>
		<head>
			<title>News Portal</title>
			<meta name="theme-color" content="#cc0000">
			<meta name="description" content="Latest breaking news">
			<link rel="alternate" type="application/rss+xml" title="Top Stories" href="/rss.xml">
			<link rel="stylesheet" href="/styles.css">
			<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Merriweather:wght@700">
		</head>
		<body>
			<header><nav><a href="/world">World</a><a href="/tech">Tech</a></nav></header>
			<main><h1>Headlines</h1><p>` + strings.Repeat("Breaking news story content. ", 10) + `</p></main>
		</body></html>`))
	}))
	defer srv.Close()
	withTestFetchClient(t, srv)

	h := tooltest.New(t)
	res := h.MustOK("manage_web_fetch", map[string]any{
		"action":         "fetch",
		"url":            srv.URL + "/",
		"extract_design": true,
	})
	data := res.Data.(map[string]any)
	if data["description"] != "Latest breaking news" {
		t.Errorf("description = %v", data["description"])
	}
	navs, ok := data["nav_links"].([]linkItem)
	if !ok || len(navs) != 2 {
		t.Fatalf("nav_links = %#v, want 2 items", data["nav_links"])
	}
	feeds, ok := data["feeds"].([]linkItem)
	if !ok || len(feeds) != 1 {
		t.Fatalf("feeds = %#v, want 1 item", data["feeds"])
	}
	design, ok := data["design"].(map[string]any)
	if !ok {
		t.Fatalf("missing design map: %#v", data)
	}
	if design["theme_color"] != "#cc0000" {
		t.Errorf("theme_color = %v", design["theme_color"])
	}
	fonts := design["fonts"].([]string)
	if len(fonts) < 2 {
		t.Errorf("expected Merriweather + Inter fonts, got %v", fonts)
	}
	vars := design["css_vars"].(map[string]string)
	if vars["--brand-red"] != "#cc0000" {
		t.Errorf("css_vars = %#v", vars)
	}
}

func TestWebFetch_DetectsConsentGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/privacy-gate/consent?next=/", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Privacy Gate</title></head><body><script>initConsent()</script></body></html>`))
	}))
	defer srv.Close()
	withTestFetchClient(t, srv)

	h := tooltest.New(t)
	res := h.MustOK("manage_web_fetch", map[string]any{
		"action": "fetch",
		"url":    srv.URL + "/",
	})
	data := res.Data.(map[string]any)
	if data["consent_gate"] != true {
		t.Errorf("expected consent_gate=true, got %#v", data)
	}
}

func TestWebDownload_SavesGLBAndAudioToFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lantern.glb":
			w.Header().Set("Content-Type", "model/gltf-binary")
			_, _ = w.Write([]byte("glTF\x02\x00\x00\x00fake-glb-bytes"))
		case "/rain.mp3":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("ID3fake-mp3-bytes"))
		}
	}))
	defer srv.Close()
	withTestFetchClient(t, srv)

	h := tooltest.New(t)
	res := h.MustOK("manage_web_fetch", map[string]any{
		"action":   "download",
		"url":      srv.URL + "/lantern.glb",
		"filename": "/files/models/lantern.glb",
	})
	data := res.Data.(map[string]any)
	if data["path"] != "/files/models/lantern.glb" || data["kind"] != "model" {
		t.Errorf("unexpected download result: %#v", data)
	}

	var storedPath string
	if err := h.DB().QueryRow("SELECT path FROM of_files WHERE kind='file' AND path='models/lantern.glb'").Scan(&storedPath); err != nil {
		t.Fatalf("expected models/lantern.glb in of_files: %v", err)
	}

	res2 := h.MustOK("manage_web_fetch", map[string]any{
		"action":   "download",
		"url":      srv.URL + "/rain.mp3",
		"filename": "rain.mp3",
	})
	data2 := res2.Data.(map[string]any)
	if data2["path"] != "/files/rain.mp3" || data2["kind"] != "audio" {
		t.Errorf("unexpected audio download result: %#v", data2)
	}
}

func TestWebDownload_RejectsDisallowedExtensionAndHTMLPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>404 Not Found</body></html>"))
	}))
	defer srv.Close()
	withTestFetchClient(t, srv)

	h := tooltest.New(t)
	res := h.MustFail("manage_web_fetch", map[string]any{
		"action":   "download",
		"url":      srv.URL + "/evil.sh",
		"filename": "evil.sh",
	}, "")
	if !strings.Contains(res.Error, "unsupported file extension") {
		t.Errorf("unexpected error for .sh: %q", res.Error)
	}

	res2 := h.MustFail("manage_web_fetch", map[string]any{
		"action":   "download",
		"url":      srv.URL + "/lantern.glb",
		"filename": "lantern.glb",
	}, "")
	if !strings.Contains(res2.Error, "does not match model asset") {
		t.Errorf("unexpected error for HTML payload: %q", res2.Error)
	}
}
