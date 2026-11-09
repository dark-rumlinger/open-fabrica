package analysis

import (
	"reflect"
	"testing"
)

func TestExtractImportSpecifiers(t *testing.T) {
	body := `
import { a } from './a.js';
import './side-effect.js';
const m = await import('./dyn.js');
export { x } from './reexport.js';
import * as THREE from 'three';
// import './commented.js' is still lexically matched (caller classifies)
`
	got := ExtractImportSpecifiers(body)
	want := []string{"./a.js", "./side-effect.js", "./dyn.js", "./reexport.js", "three", "./commented.js"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractImportSpecifiers:\n got  %v\n want %v", got, want)
	}
}

func TestBuildModuleGraph(t *testing.T) {
	bodies := map[string]string{
		"js/main.js":  "import { a } from './a.js';\nimport { b } from './sub/b.js';\nimport * as THREE from 'three';\nimport '/files/global.js';",
		"js/a.js":     "import { leaf } from './leaf';\nimport { a } from './a.js';", // extension completion + self-edge
		"js/leaf.js":  "export const leaf = 1;",
		"js/sub/b.js": "export const b = 1;",
	}
	g := BuildModuleGraph(bodies)

	wantNodes := []string{"js/a.js", "js/leaf.js", "js/main.js", "js/sub/b.js"}
	if !reflect.DeepEqual(g.Nodes, wantNodes) {
		t.Fatalf("Nodes:\n got  %v\n want %v", g.Nodes, wantNodes)
	}
	// main resolves ./a.js and ./sub/b.js; bare 'three' and absolute /files/... dropped.
	if !reflect.DeepEqual(g.Edges["js/main.js"], []string{"js/a.js", "js/sub/b.js"}) {
		t.Errorf("main edges: %v", g.Edges["js/main.js"])
	}
	// a.js: extensionless './leaf' completes to js/leaf.js; self-import kept.
	if !reflect.DeepEqual(g.Edges["js/a.js"], []string{"js/leaf.js", "js/a.js"}) {
		t.Errorf("a.js edges: %v", g.Edges["js/a.js"])
	}
}

func TestExtractNamedExports(t *testing.T) {
	body := `export function foo() {}
export async function* gen() {}
export class Bar {}
export const x = 1, y = 2;
export let z;
export { a, b as c } from './m.js';
export { d };
export default function main() {}
export * from './all.js';`
	got := ExtractNamedExports(body)
	want := []string{"foo", "gen", "Bar", "x", "y", "z", "a", "c", "d"}
	// `export const x = 1, y = 2` captures BOTH declarators (x and y);
	// `export let z` captures z; default is excluded; export * contributes no
	// named export.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractNamedExports:\n got  %v\n want %v", got, want)
	}
}

// TestExtractNamedExports_MultiDeclarator pins that every binding in a
// comma-separated declarator list is captured, and a comma inside an
// initializer (a call, an array) never splits a declarator.
func TestExtractNamedExports_MultiDeclarator(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"simple", `export const a = 1, b = 2, c = 3;`, []string{"a", "b", "c"}},
		{"no-init", `export let x, y, z;`, []string{"x", "y", "z"}},
		{"comma-in-call", `export const a = f(1, 2), b = g();`, []string{"a", "b"}},
		{"comma-in-array", `export const a = [1, 2, 3], b = 4;`, []string{"a", "b"}},
		{"multiline", "export const a = 1,\n  b = 2;", []string{"a", "b"}},
		{"obj-destructure", `export const { a, b: c, d = 5 } = obj;`, []string{"a", "c", "d"}},
		{"arr-destructure", `export const [a, , b, ...rest] = arr;`, []string{"a", "b", "rest"}},
		{"nested-destructure", `export const { a: { b }, c: [d] } = obj;`, []string{"b", "d"}},
		{"no-semicolon", "export const a = 1\nconst hidden = 2", []string{"a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractNamedExports(tc.body); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestExtractNamedExports_NoOverExtract guards against the dangerous direction -
// claiming a module exports a name it does not (which would let a genuinely bad
// import slip past the dynamic-import gate).
func TestExtractNamedExports_NoOverExtract(t *testing.T) {
	// A local (non-exported) const must not leak into the export set.
	body := `const a = 1, b = 2;
export const c = 3;`
	got := ExtractNamedExports(body)
	want := []string{"c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("over-extracted: got %v, want %v", got, want)
	}
}

func TestExtractNamedImports(t *testing.T) {
	body := `import { a, b as c } from './m.js';
import Def, { d } from './n.js';
import * as NS from './ns.js';
import Only from './only.js';`
	got := ExtractNamedImports(body)
	want := []NamedImport{
		{Spec: "./m.js", Imported: "a", Local: "a"},
		{Spec: "./m.js", Imported: "b", Local: "c"},
		{Spec: "./n.js", Imported: "d", Local: "d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractNamedImports:\n got  %v\n want %v", got, want)
	}
}

func TestResolveImport(t *testing.T) {
	tests := []struct {
		from, spec, want string
		ok               bool
	}{
		{"js/main.js", "./terrain/noise.js", "js/terrain/noise.js", true},
		{"js/glider/mesh.js", "../terrain/noise.js", "js/terrain/noise.js", true},
		{"main.js", "/files/js/util.js", "js/util.js", true},
		{"main.js", "/js/util.js", "js/util.js", true},
		{"main.js", "./util.js?v=abc", "util.js", true},
		// The wrong-directory footgun: faithfully reported, not silently fixed.
		{"js/main.js", "./js/pieces.js", "js/js/pieces.js", true},
		// Bare specifiers have no project path.
		{"js/main.js", "three", "", false},
		{"js/main.js", "@scope/pkg", "", false},
	}
	for _, tt := range tests {
		got, ok := ResolveImport(tt.from, tt.spec)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ResolveImport(%q, %q) = (%q, %v), want (%q, %v)",
				tt.from, tt.spec, got, ok, tt.want, tt.ok)
		}
	}
}

func TestIsRelativeSpecifier(t *testing.T) {
	for spec, want := range map[string]bool{
		"./a.js":  true,
		"../a.js": true,
		"/a.js":   false,
		"three":   false,
	} {
		if got := IsRelativeSpecifier(spec); got != want {
			t.Errorf("IsRelativeSpecifier(%q) = %v, want %v", spec, got, want)
		}
	}
}

func TestExtractDynamicImportArgs_Classification(t *testing.T) {
	body := "a(); import('./lit.js'); import(`./tmpl/${n}.js`); import('a' + b); import(modVar);"
	got := ExtractDynamicImportArgs(body)
	if len(got) != 4 {
		t.Fatalf("expected 4 dynamic import args, got %d: %+v", len(got), got)
	}
	if !got[0].Literal {
		t.Errorf("'./lit.js' should be a literal arg, got %+v", got[0])
	}
	for i := 1; i < 4; i++ {
		if got[i].Literal {
			t.Errorf("arg %d (%q) should be classified opaque, got literal", i, got[i].Raw)
		}
	}
}
