package analysis

import (
	"strings"
	"testing"
)

// TestRelocateNamedImports covers the wrong-source import surgery across the
// shapes that matter, plus the invariants (aliased names and unrelated
// statements untouched).
func TestRelocateNamedImports(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		moves []ImportRelocation
		want  []string // substrings that MUST be present
		nope  []string // substrings that must NOT be present
	}{
		{
			name:  "sole name moves to a fresh line, empty source statement dropped",
			body:  "import { applyRemoteAvatar } from './world.state.js';\nfoo();",
			moves: []ImportRelocation{{Imported: "applyRemoteAvatar", FromSpec: "./world.state.js", ToSpec: "./world.avatars.js"}},
			want:  []string{"import { applyRemoteAvatar } from './world.avatars.js';"},
			nope:  []string{"applyRemoteAvatar } from './world.state.js'", "{ applyRemoteAvatar } from './world.state.js'"},
		},
		{
			name:  "mixed line keeps the valid names, moves only the wrong one",
			body:  "import { setRoster, applyRemoteAvatar, toast } from './world.state.js';\n",
			moves: []ImportRelocation{{Imported: "applyRemoteAvatar", FromSpec: "./world.state.js", ToSpec: "./world.avatars.js"}},
			want: []string{
				"from './world.state.js'", "setRoster", "toast",
				"import { applyRemoteAvatar } from './world.avatars.js';",
			},
			nope: []string{"applyRemoteAvatar, toast", "setRoster, applyRemoteAvatar"},
		},
		{
			name: "merges into an existing statement for the target module (no duplicate line)",
			body: "import { applyRemoteAvatar } from './world.state.js';\n" +
				"import { renderAvatar } from './world.avatars.js';\n",
			moves: []ImportRelocation{{Imported: "applyRemoteAvatar", FromSpec: "./world.state.js", ToSpec: "./world.avatars.js"}},
			want:  []string{"applyRemoteAvatar", "renderAvatar", "from './world.avatars.js'"},
			// exactly one avatars.js import line remains
			nope: []string{"from './world.state.js'"},
		},
		{
			name:  "aliased and unrelated names are carried through untouched",
			body:  "import { a as b, keepMe } from './x.js';\nimport { wrong } from './x.js';\n",
			moves: []ImportRelocation{{Imported: "wrong", FromSpec: "./x.js", ToSpec: "./y.js"}},
			want:  []string{"a as b", "keepMe", "import { wrong } from './y.js';"},
			nope:  []string{"{ wrong } from './x.js'"},
		},
		{
			name:  "no matching relocation leaves the body unchanged",
			body:  "import { a } from './x.js';\n",
			moves: []ImportRelocation{{Imported: "z", FromSpec: "./q.js", ToSpec: "./r.js"}},
			want:  []string{"import { a } from './x.js';"},
			nope:  []string{"./r.js"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RelocateNamedImports(tc.body, tc.moves)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("want %q in output, got:\n%s", w, got)
				}
			}
			for _, n := range tc.nope {
				if strings.Contains(got, n) {
					t.Errorf("did NOT want %q in output, got:\n%s", n, got)
				}
			}
			// A merge must not leave two import lines for the same target module.
			if strings.HasPrefix(tc.name, "merges into an existing statement") {
				if strings.Count(got, "from './world.avatars.js'") != 1 {
					t.Errorf("expected exactly one avatars.js import line, got:\n%s", got)
				}
			}
		})
	}
}
