package public

import (
	"strings"
	"testing"
)

// TestIsActiveContentType pins which types are treated as script-capable and
// therefore neutralised (served as downloads) when they hold untrusted user
// uploads. Getting this wrong reopens stored XSS via uploaded SVG/HTML.
func TestIsActiveContentType(t *testing.T) {
	active := []string{
		"text/html", "text/html; charset=utf-8", "image/svg+xml",
		"application/xhtml+xml", "text/xml", "application/xml",
		"application/javascript", "text/javascript", "  IMAGE/SVG+XML  ",
	}
	for _, ct := range active {
		if !isActiveContentType(ct) {
			t.Errorf("isActiveContentType(%q) = false; want true (script-capable)", ct)
		}
	}
	inert := []string{
		"image/png", "image/jpeg", "image/gif", "image/webp",
		"application/pdf", "text/plain", "application/octet-stream",
		"video/mp4", "audio/mpeg", "",
	}
	for _, ct := range inert {
		if isActiveContentType(ct) {
			t.Errorf("isActiveContentType(%q) = true; want false (inert)", ct)
		}
	}
}

// TestUploadServingDecision mirrors the gate in ServeFile: only files under
// uploads/ whose resolved type is script-capable get neutralised. Engine assets
// (not under uploads/) must stay inline even when they are JS/SVG.
func TestUploadServingDecision(t *testing.T) {
	cases := []struct {
		path       string
		neutralise bool
	}{
		{"uploads/avatar.svg", true},
		{"uploads/t3/page.html", true},
		{"uploads/photo.png", false},
		{"uploads/doc.pdf", false},
		{"app.js", false},         // engine asset, trusted
		{"icons/logo.svg", false}, // engine asset, trusted
		{"styles.css", false},
	}
	for _, c := range cases {
		ct := mimeByExtension(c.path)
		got := strings.HasPrefix(c.path, "uploads/") && isActiveContentType(ct)
		if got != c.neutralise {
			t.Errorf("serving decision for %q (ct=%q) = %v; want %v", c.path, ct, got, c.neutralise)
		}
	}
}

func TestMimeByExtension(t *testing.T) {
	cases := map[string]string{
		"a.svg":  "image/svg+xml",
		"a.html": "text/html",
		"a.js":   "application/javascript",
		"a.png":  "image/png",
		"a.bin":  "", // unknown -> empty so caller falls back
	}
	for name, want := range cases {
		if got := mimeByExtension(name); got != want {
			t.Errorf("mimeByExtension(%q) = %q; want %q", name, got, want)
		}
	}
}
