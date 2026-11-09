package analysis

import "testing"

// AddNamedImports merges a forgotten name into an EXISTING import line from the same
// specifier (the partial-import case the platform now owns instead of handing back a
// "add it to that line" reject), and prepends a fresh line only when none exists.
func TestAddNamedImports_MergesIntoExistingLine(t *testing.T) {
	body := "import { a } from './util.js';\nconst x = b();\n"
	got := AddNamedImports(body, map[string][]string{"./util.js": {"b"}})
	want := "import { a, b } from './util.js';\nconst x = b();\n"
	if got != want {
		t.Fatalf("merge:\n got %q\nwant %q", got, want)
	}
}

func TestAddNamedImports_PrependsFreshLine(t *testing.T) {
	body := "const x = c();\n"
	got := AddNamedImports(body, map[string][]string{"other": {"c"}})
	want := "import { c } from 'other';\nconst x = c();\n"
	if got != want {
		t.Fatalf("fresh:\n got %q\nwant %q", got, want)
	}
}

func TestAddNamedImports_NoDuplicateWhenPresent(t *testing.T) {
	body := "import { a, b } from './util.js';\n"
	got := AddNamedImports(body, map[string][]string{"./util.js": {"b"}})
	if got != body {
		t.Fatalf("already-present name must not duplicate:\n got %q\nwant %q", got, body)
	}
}

func TestAddNamedImports_PreservesDefaultPrefixAndAlias(t *testing.T) {
	body := "import def, { a as x } from './util.js';\n"
	got := AddNamedImports(body, map[string][]string{"./util.js": {"b"}})
	want := "import def, { a as x, b } from './util.js';\n"
	if got != want {
		t.Fatalf("default prefix / alias must survive a merge:\n got %q\nwant %q", got, want)
	}
}

func TestImportedSpecFrom(t *testing.T) {
	bodies := map[string]string{
		"page.js": "import { a } from './util.js';\n",
		"util.js": "export function a(){}\n",
	}
	if got := ImportedSpecFrom("page.js", "util.js", bodies); got != "./util.js" {
		t.Fatalf("ImportedSpecFrom = %q, want ./util.js", got)
	}
	if got := ImportedSpecFrom("util.js", "page.js", bodies); got != "" {
		t.Fatalf("no import edge should yield empty spec, got %q", got)
	}
}
