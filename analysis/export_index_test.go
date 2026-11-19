package analysis

import "testing"

// The bare logical name is THE import style: whenever byName resolves the
// target unambiguously, the suggester returns it, never a relative path.
func TestSuggestImportSpecifier_BareWhenResolvable(t *testing.T) {
	byName, _ := ProjectModuleMap([]string{"js/app.state.js", "js/app.net.js"})
	cases := []struct{ from, to, want string }{
		{"js/app.net.js", "js/app.state.js", "app.state"},
		{"js/app.state.js", "js/app.net.js", "app.net"},
	}
	for _, c := range cases {
		if got := SuggestImportSpecifier(c.from, c.to, byName); got != c.want {
			t.Errorf("SuggestImportSpecifier(%q,%q) = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}

// A basename collision is the one case ProjectModuleMap cannot expose as a bare
// name, so the suggester falls back to the relative form - which stays loadable
// via plain URL resolution.
func TestSuggestImportSpecifier_RelativeOnCollision(t *testing.T) {
	byName, _ := ProjectModuleMap([]string{"a/state.js", "b/state.js", "a/app.js"})
	cases := []struct{ from, to, want string }{
		{"a/app.js", "a/state.js", "./state.js"},
		{"a/app.js", "b/state.js", "../b/state.js"},
	}
	for _, c := range cases {
		if got := SuggestImportSpecifier(c.from, c.to, byName); got != c.want {
			t.Errorf("SuggestImportSpecifier(%q,%q) = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}

// A nil/empty index (caller without a module set) degrades to the relative
// form rather than guessing a bare name the importmap may not carry.
func TestSuggestImportSpecifier_RelativeWithoutIndex(t *testing.T) {
	cases := []struct{ from, to, want string }{
		{"/files/app.main.js", "/files/app.state.js", "./app.state.js"},
		{"/files/sub/a.js", "/files/b.js", "../b.js"},
		{"/files/a.js", "/files/sub/b.js", "./sub/b.js"},
	}
	for _, c := range cases {
		if got := SuggestImportSpecifier(c.from, c.to, nil); got != c.want {
			t.Errorf("SuggestImportSpecifier(%q,%q) = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}

// ModuleSpecifierIndex is the bodies-map convenience over ProjectModuleMap.
func TestModuleSpecifierIndex(t *testing.T) {
	byName := ModuleSpecifierIndex(map[string]string{
		"js/app.state.js": "export const state = {};",
		"js/app.net.js":   "",
	})
	if byName["app.state"] != "js/app.state.js" {
		t.Errorf(`byName["app.state"] = %q, want "js/app.state.js"`, byName["app.state"])
	}
	if byName["app.net"] != "js/app.net.js" {
		t.Errorf(`byName["app.net"] = %q, want "js/app.net.js"`, byName["app.net"])
	}
}
