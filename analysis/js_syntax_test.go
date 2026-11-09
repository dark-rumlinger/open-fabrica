package analysis

import "testing"

// TestJSSyntax pins the parser's behaviour on the shapes the readiness gate and
// the write gate depend on - in particular whether esbuild treats a brace-list
// export of an undeclared name as a syntax error (it does), which decides whether
// the parse pass alone covers that defect.
func TestJSSyntax(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantClean bool
	}{
		{"clean module", "const a = 1;\nexport { a };\n", true},
		{"reexport of imported", "import { x } from './y.js';\nexport { x };\n", true},
		{"reexport from source", "export { x } from './y.js';\n", true},
		{"decl export", "export const a = 1;\nexport function f() {}\n", true},
		{"export undefined name", "const a = 1;\nexport { a, missing };\n", false},
		{"truncated export block", "const a = 1;\nexport {\n  a,\n", false},
		{"unterminated function", "function f() {\n", false},
	}
	for _, c := range cases {
		r := JSSyntax("js/m.js", c.body)
		if !r.Gated {
			t.Fatalf("%s: expected a JS path to be gated", c.name)
		}
		if r.Clean != c.wantClean {
			t.Errorf("%s: Clean=%v want %v (detail=%q)", c.name, r.Clean, c.wantClean, r.Detail)
		}
	}

	if r := JSSyntax("css/main.css", "this is not js"); r.Gated {
		t.Errorf("css path should not be gated")
	}
}
