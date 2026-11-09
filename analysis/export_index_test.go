package analysis

import "testing"

// The bare logical name is THE import style: whenever byName resolves the
// target unambiguously, the suggester returns it, never a relative path.
func TestSuggestImportSpecifier_BareWhenResolvable(t *testing.T) {
	byName, _ := ProjectModuleMap([]string{"js/world.state.js", "js/world.net.js"})
	cases := []struct{ from, to, want string }{
		{"js/world.net.js", "js/world.state.js", "world.state"},
		{"js/world.state.js", "js/world.net.js", "world.net"},
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
	byName, _ := ProjectModuleMap([]string{"a/state.js", "b/state.js", "a/game.js"})
	cases := []struct{ from, to, want string }{
		{"a/game.js", "a/state.js", "./state.js"},
		{"a/game.js", "b/state.js", "../b/state.js"},
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
		{"/files/game.world.js", "/files/game.state.js", "./game.state.js"},
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
		"js/world.state.js": "export const state = {};",
		"js/world.net.js":   "",
	})
	if byName["world.state"] != "js/world.state.js" {
		t.Errorf(`byName["world.state"] = %q, want "js/world.state.js"`, byName["world.state"])
	}
	if byName["world.net"] != "js/world.net.js" {
		t.Errorf(`byName["world.net"] = %q, want "js/world.net.js"`, byName["world.net"])
	}
}
