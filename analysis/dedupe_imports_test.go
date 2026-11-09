package analysis

import (
	"strings"
	"testing"
)

// DedupeNamedImports removes ONLY exact-duplicate named imports (same specifier,
// same imported name, same local binding) - the weak-model "imported it at the top
// and again at the bottom" shape - and leaves every genuine conflict in place.
func TestDedupeNamedImports(t *testing.T) {
	t.Run("exact duplicate collapses to the first, code preserved", func(t *testing.T) {
		body := "import { gridToWorld } from './terrain.js';\n" +
			"export function a(){ return gridToWorld(1); }\n" +
			"import { gridToWorld } from './terrain.js';\n"
		out, removed := DedupeNamedImports(body)
		if got := strings.Count(out, "import { gridToWorld } from './terrain.js'"); got != 1 {
			t.Fatalf("duplicate import should collapse to one, got %d:\n%s", got, out)
		}
		if len(removed) != 1 {
			t.Fatalf("want 1 removed label, got %v", removed)
		}
		if !strings.Contains(out, "export function a()") {
			t.Fatalf("non-import code must be preserved, got:\n%s", out)
		}
	})

	t.Run("clean body is returned untouched", func(t *testing.T) {
		body := "import { a } from './x.js';\nimport { b } from './y.js';\n"
		out, removed := DedupeNamedImports(body)
		if out != body || len(removed) != 0 {
			t.Fatalf("clean body must be untouched, got removed=%v body:\n%s", removed, out)
		}
	})

	t.Run("same name from a DIFFERENT source is a real conflict, left alone", func(t *testing.T) {
		body := "import { x } from './a.js';\nimport { x } from './b.js';\n"
		out, removed := DedupeNamedImports(body)
		if out != body || len(removed) != 0 {
			t.Fatalf("same-name-different-source must not collapse; got removed=%v", removed)
		}
	})

	t.Run("same name under a DIFFERENT alias is a distinct binding, left alone", func(t *testing.T) {
		body := "import { clock } from './s.js';\nimport { clock as _clk } from './s.js';\n"
		out, removed := DedupeNamedImports(body)
		if out != body || len(removed) != 0 {
			t.Fatalf("a distinct local binding must not be collapsed; got removed=%v", removed)
		}
	})

	t.Run("duplicate inside a multi-name statement drops only the dup", func(t *testing.T) {
		body := "import { a } from './x.js';\nimport { a, c } from './x.js';\n"
		out, removed := DedupeNamedImports(body)
		if len(removed) != 1 {
			t.Fatalf("want 1 removed, got %v", removed)
		}
		if got := strings.Count(out, "import { a } from './x.js'"); got != 1 {
			t.Fatalf("the first import of a must survive exactly once, got %d:\n%s", got, out)
		}
		if !strings.Contains(out, "import { c } from './x.js'") {
			t.Fatalf("the non-duplicate name c must survive its statement, got:\n%s", out)
		}
	})
}
