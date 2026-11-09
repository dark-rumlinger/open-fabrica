package store

import "testing"

// TestCanonicalFilePath pins that every URL-style file reference the model emits
// collapses to the one bare path the of_files table keys on, while a path that is
// already bare - or a file legitimately named "files.js" - is left intact.
func TestCanonicalFilePath(t *testing.T) {
	cases := map[string]string{
		"/files/viewport.js":  "viewport.js",
		"files/viewport.js":   "viewport.js",
		"/viewport.js":        "viewport.js",
		"viewport.js":         "viewport.js",
		"/files/js/scene.js":  "js/scene.js",
		"files/js/scene.js":   "js/scene.js",
		"js/scene.js":         "js/scene.js",
		"files.js":            "files.js",            // not a "files/" prefix, left intact
		"css/files/theme.css": "css/files/theme.css", // interior "files/" untouched
	}
	for in, want := range cases {
		if got := CanonicalFilePath(in); got != want {
			t.Errorf("CanonicalFilePath(%q) = %q, want %q", in, got, want)
		}
	}
}
