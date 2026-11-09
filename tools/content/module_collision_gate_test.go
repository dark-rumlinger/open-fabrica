package content

import (
	"strings"
	"testing"

	"openfabrica/tools"
)

func collisionCtx(t *testing.T) *tools.ToolContext {
	t.Helper()
	return &tools.ToolContext{DB: setupBulkDB(t), ReadTracker: tools.NewReadTracker()}
}

// The collision exists the moment the second file lands, when the model is holding both
// filenames and a rename costs one call. Leaving it to the post-build validator means the
// whole app is written against a specifier that was never in the importmap.
func TestSaveRejectsCollidingModuleBasename(t *testing.T) {
	ctx := collisionCtx(t)
	if res, err := (&ArtSave{Kind: "file", FilePath: "js/app.js", Body: "export const a = 1;\n"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed module: err=%v res=%+v", err, res)
	}

	res, err := (&ArtSave{Kind: "file", FilePath: "js/vendor/app.js", Body: "export const b = 2;\n"}).Run(ctx)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if res.Success {
		t.Fatal("a second module with the same basename must be refused - neither would resolve")
	}
	if res.Code != "colliding_module_name" {
		t.Errorf("code = %q, want colliding_module_name", res.Code)
	}
	// The two unresolvable-specifier cases need OPPOSITE fixes, so the message has to say
	// which one this is and name the file already holding the specifier.
	for _, want := range []string{"js/app.js", "rename", "Do NOT declare it as a library"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("message should mention %q, got: %s", want, res.Error)
		}
	}
}

// Distinct basenames are the normal split shape and must stay unobstructed - this gate
// sits on the path every subsystem leaf is written through.
func TestSaveAllowsDistinctModuleBasenames(t *testing.T) {
	ctx := collisionCtx(t)
	for _, p := range []string{"js/app.js", "js/app.state.js", "js/app.hud.js"} {
		res, err := (&ArtSave{Kind: "file", FilePath: p, Body: "export const x = 1;\n"}).Run(ctx)
		if err != nil || !res.Success {
			t.Fatalf("saving %q must succeed: err=%v res=%+v", p, err, res)
		}
	}
}

// Re-saving an existing file is not a collision with itself, and must never be: the gate
// would otherwise trap every edit to a file that is already in a collision - the exact
// repair path out of one.
func TestSaveResaveOfExistingModuleNotTrapped(t *testing.T) {
	ctx := collisionCtx(t)
	if res, err := (&ArtSave{Kind: "file", FilePath: "js/app.js", Body: "export const a = 1;\n"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed: err=%v res=%+v", err, res)
	}
	res, err := (&ArtSave{Kind: "file", FilePath: "js/app.js", Body: "export const a = 2;\n"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("re-saving the same path must succeed: err=%v res=%+v", err, res)
	}
}

// CSS shares no importmap namespace, so a same-basename stylesheet is not a collision.
func TestSaveCSSBasenameIsNotAModuleCollision(t *testing.T) {
	ctx := collisionCtx(t)
	if res, err := (&ArtSave{Kind: "file", FilePath: "js/app.js", Body: "export const a = 1;\n"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed: err=%v res=%+v", err, res)
	}
	res, err := (&ArtSave{Kind: "file", FilePath: "css/app.css", Body: ".a { color: red; }\n"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("a stylesheet sharing the basename must save: err=%v res=%+v", err, res)
	}
}
