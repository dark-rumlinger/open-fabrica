package analysis

import "testing"

// CollidingModuleName is what lets the save gate refuse a name BEFORE the whole app is
// written, using the same index the importmap and the post-build validator resolve
// through - so a gate that refuses a name and a runtime that drops it cannot disagree.
func TestCollidingModuleName(t *testing.T) {
	cases := []struct {
		name     string
		existing []string
		newPath  string
		want     string
	}{
		{"distinct basenames", []string{"js/app.js", "js/app.state.js"}, "js/app.hud.js", ""},
		{"first module in the project", nil, "js/app.js", ""},
		{"same basename in another directory", []string{"js/app.js"}, "js/vendor/app.js", "app"},
		{"re-save of the same path is not a self-collision", []string{"js/app.js"}, "js/app.js", ""},
		{"dotted names stay distinct", []string{"js/world.state.js"}, "js/world.net.js", ""},
		{"mjs collides with js on the logical name", []string{"js/app.js"}, "js/app.mjs", "app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CollidingModuleName(tc.existing, tc.newPath); got != tc.want {
				t.Errorf("CollidingModuleName(%v, %q) = %q, want %q", tc.existing, tc.newPath, got, tc.want)
			}
		})
	}
}

// A rejection has to NAME the file to rename, or the model has to go looking for it.
func TestCollidingModulePaths(t *testing.T) {
	paths := []string{"js/app.js", "js/vendor/app.js", "js/app.state.js"}
	got := CollidingModulePaths(paths, "app")
	if len(got) != 2 || got[0] != "js/app.js" || got[1] != "js/vendor/app.js" {
		t.Errorf("CollidingModulePaths = %v, want both app.js paths in sorted order", got)
	}
}
