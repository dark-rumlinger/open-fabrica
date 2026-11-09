package engine

import "testing"

// dropRedundantLibraryTasks removes a no-paths/no-edits "register the libraries"
// task (the build agent only thrashes on it, since libraries in
// external_libraries are auto-registered) and scrubs its id from remaining deps,
// while leaving real build tasks - and library tasks when nothing is declared -
// untouched.
func TestDropRedundantLibraryTasks(t *testing.T) {
	t.Run("drops the three-libs antipattern and scrubs deps", func(t *testing.T) {
		p := &Plan{
			ExternalLibraries: []LibrarySpec{{Name: "three.js", ModuleName: "three", Kind: "module"}},
			Tasks: []PlanTaskSpec{
				{ID: "three-libs", Title: "Register three.js core + addons",
					DoneWhen: "external_libraries lists three (module) and three/addons/"},
				{ID: "state", Title: "State leaf", Paths: []string{"/files/game.state.js"}},
				{ID: "terrain", Title: "Terrain", Paths: []string{"/files/game.terrain.js"},
					Deps: []string{"state", "three-libs"}},
			},
		}
		dropRedundantLibraryTasks(p)

		if len(p.Tasks) != 2 {
			t.Fatalf("expected the library task dropped (2 left), got %d: %+v", len(p.Tasks), p.Tasks)
		}
		for _, tk := range p.Tasks {
			if tk.ID == "three-libs" {
				t.Fatal("three-libs should have been dropped")
			}
			for _, d := range tk.Deps {
				if d == "three-libs" {
					t.Errorf("task %q still deps on the dropped three-libs", tk.ID)
				}
			}
		}
	})

	t.Run("keeps a task that owns files even if it mentions a library", func(t *testing.T) {
		p := &Plan{
			ExternalLibraries: []LibrarySpec{{Name: "three.js", ModuleName: "three", Kind: "module"}},
			Tasks: []PlanTaskSpec{
				{ID: "render", Title: "Render loop using three.js", Paths: []string{"/files/render.js"}},
			},
		}
		dropRedundantLibraryTasks(p)
		if len(p.Tasks) != 1 {
			t.Fatalf("a task that owns a file must be kept, got %+v", p.Tasks)
		}
	})

	t.Run("keeps a library task when nothing is declared (build must add it)", func(t *testing.T) {
		p := &Plan{
			Tasks: []PlanTaskSpec{
				{ID: "libs", Title: "Add the charting library", DoneWhen: "external_libraries lists chart.js"},
			},
		}
		dropRedundantLibraryTasks(p)
		if len(p.Tasks) != 1 {
			t.Fatalf("with no external_libraries declared, the library task must stay, got %+v", p.Tasks)
		}
	})

	t.Run("keeps a task whose prose only shares a word with a module name", func(t *testing.T) {
		p := &Plan{
			ExternalLibraries: []LibrarySpec{{Name: "three.js", ModuleName: "three", Kind: "module"}},
			Tasks: []PlanTaskSpec{
				{ID: "featured", Title: "Featured products", Intent: "show three featured products on the homepage",
					DoneWhen: "the homepage renders a featured section"},
			},
		}
		dropRedundantLibraryTasks(p)
		if len(p.Tasks) != 1 {
			t.Fatalf("ordinary prose containing %q must not read as a library task, got %+v", "three", p.Tasks)
		}
	})

	t.Run("keeps a no-paths PWA task (not library-themed)", func(t *testing.T) {
		p := &Plan{
			ExternalLibraries: []LibrarySpec{{Name: "three.js", ModuleName: "three", Kind: "module"}},
			Tasks: []PlanTaskSpec{
				{ID: "pwa", Title: "Enable PWA / installable offline", Tools: []string{"manage_project"},
					DoneWhen: "manifest.json and service worker exist; the app installs and runs offline"},
			},
		}
		dropRedundantLibraryTasks(p)
		if len(p.Tasks) != 1 {
			t.Fatalf("a no-paths PWA task must be kept, got %+v", p.Tasks)
		}
	})
}
