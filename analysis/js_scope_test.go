package analysis

import (
	"sort"
	"strings"
	"testing"
)

// boundAt reports the names the scope tree resolves at the offset of the marker
// `/*HERE*/`, restricted to the probes asked about. It keeps the tree's contract
// - "what can this offset see?" - testable without exposing its internals.
func boundAt(t *testing.T, src string, probes ...string) []string {
	t.Helper()
	off := strings.Index(src, "/*HERE*/")
	if off < 0 {
		t.Fatalf("test source has no /*HERE*/ marker")
	}
	scopes := analyzeScopes(maskJS(src))
	var out []string
	for _, p := range probes {
		if scopes.resolves(p, off) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func TestScopes_ParamBindsOnlyInItsOwnFunction(t *testing.T) {
	// A name that is a parameter of one function and free in another - the shape a
	// flat name set cannot tell apart.
	src := `import { x } from './x.js';
function buildBooth(scene) { scene.add(x); }
function animate() { /*HERE*/ }`
	if got := boundAt(t, src, "scene"); len(got) != 0 {
		t.Errorf("animate() must not see buildBooth's parameter, got %v", got)
	}
	inner := `import { x } from './x.js';
function buildBooth(scene) { /*HERE*/ scene.add(x); }`
	if got := boundAt(t, inner, "scene"); len(got) != 1 {
		t.Errorf("buildBooth must see its own parameter, got %v", got)
	}
}

func TestScopes_Shapes(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		probe []string
		want  []string
	}{
		{
			"nested params with parens and defaults",
			`function f({ dt = getDefault() }, cb = () => run()) { /*HERE*/ }`,
			[]string{"dt", "cb"}, []string{"cb", "dt"},
		},
		{
			"arrow with expression body binds over the expression",
			`const f = ({ a, b }) => a(/*HERE*/) + b;`,
			[]string{"a", "b"}, []string{"a", "b"},
		},
		{
			"arrow expression params do not leak past the expression",
			`const f = ({ a, b }) => a() + b;
function other() { /*HERE*/ }`,
			[]string{"a", "b"}, nil,
		},
		{
			"bare-identifier arrow param",
			`const h = z => z(/*HERE*/);`,
			[]string{"z"}, []string{"z"},
		},
		{
			"block scope folds into the function",
			`function f() { if (a) { var y = 1; let w = 2; } /*HERE*/ }`,
			[]string{"y", "w"}, []string{"w", "y"},
		},
		{
			"nested closure sees the outer local",
			`function outer() { let v; return function () { /*HERE*/ v = 1; }; }`,
			[]string{"v"}, []string{"v"},
		},
		{
			"iife hoists its inner declaration",
			`(function () { /*HERE*/ inner(); function inner() {} })();`,
			[]string{"inner"}, []string{"inner"},
		},
		{
			"catch binds its parameter",
			`try { risky(); } catch (err) { /*HERE*/ }`,
			[]string{"err"}, []string{"err"},
		},
		{
			"catch binding does not leak out",
			`try { risky(); } catch (err) {}
function after() { /*HERE*/ }`,
			[]string{"err"}, nil,
		},
		{
			"class body binds fields and methods",
			`class E { hp = 100; step() {} /*HERE*/ }`,
			[]string{"hp", "step"}, []string{"hp", "step"},
		},
		{
			"getter and setter params",
			`class E { set size(v) { /*HERE*/ } }`,
			[]string{"v"}, []string{"v"},
		},
		{
			"generator and async params",
			`async function a(p) { await p; }
function* g(q) { /*HERE*/ yield q; }`,
			[]string{"q", "p"}, []string{"q"},
		},
		{
			"module-level for head binds its declarator",
			`for (let i = 0; i < 3; i++) { /*HERE*/ }`,
			[]string{"i"}, []string{"i"},
		},
		{
			"object method shorthand binds its param",
			`const o = { draw(px) { /*HERE*/ return px; } };`,
			[]string{"px"}, []string{"px"},
		},
		{
			"imports and top-level declarations are module scope",
			`import def from './x.js';
import * as ns from './y.js';
import { a, b as c } from './z.js';
const top = 1;
function fn() { /*HERE*/ }`,
			[]string{"def", "ns", "a", "c", "top", "fn"},
			[]string{"a", "c", "def", "fn", "ns", "top"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := boundAt(t, c.src, c.probe...)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("bound = %v, want %v", got, c.want)
			}
		})
	}
}
