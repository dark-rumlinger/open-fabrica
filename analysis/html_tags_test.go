package analysis

import "testing"

func TestExternalURLIsDynamicCSS(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		// Content-negotiated font endpoints: dynamic.
		{"https://fonts.googleapis.com/css2?family=Orbitron:wght@400;700&display=swap", true},
		{"https://fonts.googleapis.com/css?family=Roboto", true},
		{"https://fonts.bunny.net/css?family=inter", true},
		{"HTTPS://FONTS.GOOGLEAPIS.COM/CSS2?family=Inter", true}, // case-insensitive
		// Ordinary, stable assets: not dynamic (SRI stays valid).
		{"https://cdn.jsdelivr.net/npm/lib@1.2.3/dist/theme.css", false},
		{"https://unpkg.com/three@0.170.0/build/three.module.js", false},
		{"https://fonts.gstatic.com/s/orbitron/v31/font.woff2", false}, // the actual font file is immutable
		{"/files/app.css", false},
	}
	for _, c := range cases {
		if got := ExternalURLIsDynamicCSS(c.url); got != c.want {
			t.Errorf("ExternalURLIsDynamicCSS(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestExternalURLIsFontFile(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		// Raw font binaries: font files.
		{"https://fonts.gstatic.com/s/pressstart2p/v15/e3t4euO8T-267oIAQAu6jDQyK3nVivM.woff2", true},
		{"https://fonts.gstatic.com/s/vt323/v17/pxiKyp0ihIEF2isfFJU.woff2", true},
		{"https://cdn.jsdelivr.net/fontsource/fonts/inter@latest/latin-400-normal.woff", true},
		{"https://example.com/fonts/MyFont.ttf", true},
		{"https://example.com/fonts/MyFont.otf", true},
		{"https://example.com/fonts/legacy.eot", true},
		{"https://cdn.example.com/font.woff2?v=3", true}, // query after suffix
		{"HTTPS://FONTS.GSTATIC.COM/s/x.WOFF2", true},    // case-insensitive
		// Stylesheets and scripts: NOT font files.
		{"https://fonts.googleapis.com/css2?family=Press+Start+2P&display=swap", false},
		{"https://cdn.jsdelivr.net/npm/lib@1.2.3/dist/theme.css", false},
		{"https://unpkg.com/three@0.160.0/build/three.module.js", false},
		{"https://cdn.jsdelivr.net/npm/marked@12.0.2/marked.min.js", false},
		{"", false},
	}
	for _, c := range cases {
		if got := ExternalURLIsFontFile(c.url); got != c.want {
			t.Errorf("ExternalURLIsFontFile(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestFontFileMIMEType(t *testing.T) {
	cases := map[string]string{
		"https://fonts.gstatic.com/s/x/y.woff2": "font/woff2",
		"https://example.com/a.woff":            "font/woff",
		"https://example.com/a.ttf":             "font/ttf",
		"https://example.com/a.otf":             "font/otf",
		"https://example.com/a.eot":             "application/vnd.ms-fontobject",
		"https://example.com/a.woff2?v=1":       "font/woff2",
		"https://example.com/not-a-font.js":     "",
	}
	for url, want := range cases {
		if got := FontFileMIMEType(url); got != want {
			t.Errorf("FontFileMIMEType(%q) = %q, want %q", url, got, want)
		}
	}
}
