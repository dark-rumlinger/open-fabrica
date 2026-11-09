package analysis

import (
	"strconv"
	"testing"
)

// arityKeys renders findings as "name/args/params" for order-stable comparison.
func arityKeys(ms []ArityMismatch) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name+"/"+strconv.Itoa(m.Args)+"/"+strconv.Itoa(m.Params))
	}
	return out
}

// TestArity_OverAppliedFixedArityFn covers arity over-application: a 1-parameter
// function called with two arguments, so the first argument binds to the parameter
// and the intended value is dropped, leaving a later call through it to throw.
func TestArity_OverAppliedFixedArityFn(t *testing.T) {
	src := `
function buildFlora(planet) {
  const name = genFloraName(planet, planet.rng());
  return name;
}
function genFloraName(rng) {
  return NAMES[Math.floor(rng() * NAMES.length)];
}
`
	got := arityKeys(ArityOverApplications("game.terrain.js", src))
	want := []string{"genFloraName/2/1"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestArity_Flags(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "arrow helper over-applied",
			src:  "const gen = (rng) => { return rng() + 1; };\ngen(obj, obj.rng());",
			want: []string{"gen/2/1"},
		},
		{
			name: "multi-param function over-applied",
			src:  "function mk(a, b) { return a + b; }\nmk(1, 2, 3);",
			want: []string{"mk/3/2"},
		},
		{
			name: "two bad call sites both reported",
			src:  "function f(a) { return a; }\nf(1, 2);\nf(3, 4);",
			want: []string{"f/2/1", "f/2/1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := arityKeys(ArityOverApplications("m.js", c.src))
			if len(got) != len(c.want) {
				t.Fatalf("want %v, got %v", c.want, got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("want %v, got %v", c.want, got)
				}
			}
		})
	}
}

func TestArity_NoFalsePositives(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"correct arity", "function f(a, b) { return a + b; }\nf(1, 2);"},
		{"too few is never flagged", "function f(a, b) { return a; }\nf(1);"},
		{"rest parameter is variadic", "function f(...xs) { return xs.length; }\nf(1, 2, 3);"},
		{"default parameter makes args optional", "function f(a, b = 1) { return a + b; }\nf(1, 2, 3);"},
		{"body reads arguments object", "function f(a) { return arguments.length; }\nf(1, 2, 3);"},
		{"spread argument, count unknown", "function f(a) { return a; }\nf(...list);"},
		{"spread mixed with positional", "function f(a) { return a; }\nf(x, ...list);"},
		{"method call is not a free function", "const o = { f(a) { return a; } };\no.f(1, 2, 3);"},
		// A destructuring parameter in the signature stops JSFunctions' backward
		// header scan at its `}`, so the function is not detected and not checked -
		// a safe miss (no false positive), never a false flag.
		{"destructuring param defeats detection (safe miss)", "function place({a, b}, c) { return a + b + c; }\nplace(1, 2, 3);"},
		{"unknown/global callee", "doThing(1, 2, 3);"},
		{"ambiguous: name declared twice", "function f(a) { return a; }\nfunction g() { const f = (x, y) => x + y; return f(1, 2); }"},
		{"imported name is not module-local", "import { f } from './x.js';\nf(1, 2, 3);"},
		{"definition header is not a call", "function f(a) { return a; }"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ArityOverApplications("m.js", c.src); len(got) != 0 {
				t.Fatalf("expected no findings, got %v", arityKeys(got))
			}
		})
	}
}

// A placeholder that is later swapped for the real function is not judged by the
// placeholder's arity.
func TestArityOverApplications_ReassignedBindingIsSkipped(t *testing.T) {
	body := "let pick = () => {};\nfunction wire() { pick = (a, b) => a + b; }\nfunction go() { pick(1, 2); }\n"
	if got := ArityOverApplications("x.js", body); len(got) != 0 {
		t.Fatalf("a reassigned binding must not be judged, got %+v", got)
	}
	body = "function pick() {}\nfunction go() { pick(1, 2); }\n"
	if got := ArityOverApplications("x.js", body); len(got) != 1 {
		t.Fatalf("a plain over-application is still reported, got %+v", got)
	}
}
