package store

import (
	"strings"
	"testing"
)

// Every project JS file is served as an ES module, so a module's top-level
// functions are module-scoped and an inline on*= handler calling one throws
// "<name> is not defined" the moment it fires. The save gate must reject that
// shape at write time with the addEventListener recipe.
func TestValidateInlineHandlerScope_RejectsModuleScopedCall(t *testing.T) {
	body := `<h1>Game</h1><button onclick="startGame()">Play</button>`
	err := validateInlineHandlerScope(body)
	if err == nil {
		t.Fatal("expected rejection: startGame is module-scoped, never a window global")
	}
	for _, want := range []string{"startGame", "onclick", "addEventListener"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// The gate lists every offending handler in one error so the model fixes the
// whole class in one save, deduplicated per (attribute, identifier).
func TestValidateInlineHandlerScope_ListsAllOffenders(t *testing.T) {
	body := `<button onclick="addTask()">Add</button>
<form onsubmit="saveTask(event)"></form>
<button onclick="addTask()">Add again</button>`
	err := validateInlineHandlerScope(body)
	if err == nil {
		t.Fatal("expected rejection")
	}
	msg := err.Error()
	for _, want := range []string{"addTask", "saveTask", "onsubmit"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q: %v", want, msg)
		}
	}
	if strings.Count(msg, "addTask(") != 1 {
		t.Errorf("duplicate (attr, ident) pairs should be reported once: %v", msg)
	}
}

func TestValidateInlineHandlerScope_Allows(t *testing.T) {
	cases := []struct{ name, body string }{
		{"no handlers", `<h1>Hello</h1><p>Plain markup.</p>`},
		{"browser global call", `<button onclick="alert('hi')">Hi</button>`},
		{"member call resolves through its object", `<div onclick="app.start()">Go</div>`},
		{"navigation without a call", `<a onclick="location.href='/next'">Next</a>`},
		{"keyword before paren", `<button onclick="if (confirm('sure?')) { location.reload() }">Reset</button>`},
		{"declared in a same-body inline script", `<script>function toggleMenu() {}</script>
<button onclick="toggleMenu()">Menu</button>`},
		{"const arrow fn in inline script", `<script>const closeModal = () => {};</script>
<div onclick="closeModal()">x</div>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateInlineHandlerScope(c.body); err != nil {
				t.Errorf("expected pass, got: %v", err)
			}
		})
	}
}

// The gate is wired into ArtifactSave for every markup kind (page, layout,
// component) - a page save carrying a doomed inline handler must be refused
// before the row is written.
func TestArtifactSave_RejectsInlineHandlerOnPage(t *testing.T) {
	db := setupArtifactDB(t)
	a := &Artifact{
		Kind: "page", Path: "/todo",
		Body: `<button onclick="addTask()">Add</button>`,
	}
	if _, _, err := ArtifactSave(db, a, "test"); err == nil {
		t.Fatal("expected page save with module-scoped inline handler to be rejected")
	}
	if exists := ArtifactExists(db, "page", "/todo"); exists {
		t.Error("rejected save must not write the row")
	}
}
