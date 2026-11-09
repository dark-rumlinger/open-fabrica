package analysis

import (
	"reflect"
	"testing"
)

func TestExtractDynamicNamedImports(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []DynamicNamedImport
	}{
		{
			name: "then-callback destructure with rename",
			body: `foo(); import('./d.js').then(({ a, b: c }) => { use(a, c); });`,
			want: []DynamicNamedImport{{"./d.js", "a", "a"}, {"./d.js", "b", "c"}},
		},
		{
			name: "async then-callback",
			body: `import("./d.js").then(async ({ x }) => { await x(); });`,
			want: []DynamicNamedImport{{"./d.js", "x", "x"}},
		},
		{
			name: "await destructure",
			body: `const { x, y } = await import('./e.js');`,
			want: []DynamicNamedImport{{"./e.js", "x", "x"}, {"./e.js", "y", "y"}},
		},
		{
			name: "default and rest/computed are filtered to plain names",
			body: `const { default: d, z, ...rest } = await import('./e.js');`,
			want: []DynamicNamedImport{{"./e.js", "default", "d"}, {"./e.js", "z", "z"}},
		},
		{
			name: "destructure default value stripped",
			body: `const { a = 5 } = await import('./e.js');`,
			want: []DynamicNamedImport{{"./e.js", "a", "a"}},
		},
		{
			name: "opaque (variable) dynamic import is not matched",
			body: `const p = './e.js'; const { a } = await import(p);`,
			want: nil,
		},
		{
			name: "namespace-style then access is not a destructure",
			body: `import('./d.js').then(m => m.run());`,
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractDynamicNamedImports(c.body)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func issuesByName(iss []ImportIssue) map[string]ImportIssue {
	m := map[string]ImportIssue{}
	for _, is := range iss {
		m[is.Imported] = is
	}
	return m
}

// A representative bug: explorer.js destructures registerDiscovery
// from explorer.discovery.js (which only exports CREDIT_REWARDS); the function
// lives in explorer.state.js. registerDiscovery must flag (target explorer.discovery.js),
// CREDIT_REWARDS must stay clean.
func TestResolveDynamicNamedImports_WrongModule(t *testing.T) {
	bodies := map[string]string{
		"explorer.discovery.js": `export const CREDIT_REWARDS = {};`,
		"explorer.state.js":     `export function registerDiscovery(){} export const state = {};`,
		"explorer.js": `import { state } from './explorer.state.js';
function onLand(planet){
  import('./explorer.discovery.js').then(({ CREDIT_REWARDS, registerDiscovery }) => {
    registerDiscovery('planet', planet.id);
  });
}`,
	}
	got := issuesByName(ResolveDynamicNamedImports("explorer.js", bodies, AnalyzeModules(bodies)))
	rd, ok := got["registerDiscovery"]
	if !ok || rd.Reason != "dynamic_name_missing" || rd.Target != "explorer.discovery.js" {
		t.Fatalf("registerDiscovery should flag as dynamic_name_missing on explorer.discovery.js, got %+v", got)
	}
	if _, ok := got["CREDIT_REWARDS"]; ok {
		t.Errorf("CREDIT_REWARDS IS exported by the target and must not flag: %+v", got)
	}
}

func TestResolveDynamicNamedImports_AwaitFormAndRename(t *testing.T) {
	bodies := map[string]string{
		"x.js": `export const A = 1;`, // exports A, not B
		"f.js": `export async function go(){ const { A: a, B: b } = await import('./x.js'); return a + b; }`,
	}
	got := issuesByName(ResolveDynamicNamedImports("f.js", bodies, AnalyzeModules(bodies)))
	if is, ok := got["B"]; !ok || is.Reason != "dynamic_name_missing" || is.Local != "b" {
		t.Fatalf("B should flag (local b), got %+v", got)
	}
	if _, ok := got["A"]; ok {
		t.Errorf("A is exported and must not flag: %+v", got)
	}
}

// TestResolveDynamicNamedImports_MultiDeclaratorNoFalsePositive verifies that a
// non-first declarator destructured from a literal dynamic import is NOT flagged:
// the dynamic gate decides via the regex extractor, which must capture every declarator.
func TestResolveDynamicNamedImports_MultiDeclaratorNoFalsePositive(t *testing.T) {
	bodies := map[string]string{
		"m.js": `export const a = 1, b = 2;`,
		"f.js": `export async function go(){ const { b } = await import('./m.js'); return b; }`,
	}
	if iss := ResolveDynamicNamedImports("f.js", bodies, AnalyzeModules(bodies)); len(iss) != 0 {
		t.Fatalf("`b` is a real (second) declarator export and must not flag, got %+v", iss)
	}
}

func TestResolveDynamicNamedImports_DefaultNeverFlagged(t *testing.T) {
	bodies := map[string]string{
		"x.js": `export const A = 1;`,
		"f.js": `export async function go(){ const { default: d } = await import('./x.js'); return d; }`,
	}
	if iss := ResolveDynamicNamedImports("f.js", bodies, AnalyzeModules(bodies)); len(iss) != 0 {
		t.Fatalf("the default export is always available and must not flag, got %+v", iss)
	}
}

func TestResolveDynamicNamedImports_TargetNotSavedIsAdvisory(t *testing.T) {
	bodies := map[string]string{
		"f.js": `export async function go(){ const { A } = await import('./x.js'); return A; }`,
		// x.js intentionally absent
	}
	iss := ResolveDynamicNamedImports("f.js", bodies, AnalyzeModules(bodies))
	if len(iss) != 1 || iss[0].Reason != "dynamic_target_absent" {
		t.Fatalf("a not-yet-saved target must be advisory (dynamic_target_absent), got %+v", iss)
	}
}

func TestResolveDynamicNamedImports_ExportStarSilences(t *testing.T) {
	bodies := map[string]string{
		"impl.js":   `export const A = 1;`,
		"barrel.js": `export * from "three"; export const LOCAL = 1;`, // unknowable surface (external star)
		"f.js":      `export async function go(){ const { somethingFromThree } = await import('./barrel.js'); return somethingFromThree; }`,
	}
	if iss := ResolveDynamicNamedImports("f.js", bodies, AnalyzeModules(bodies)); len(iss) != 0 {
		t.Fatalf("a target with an external export * has an unknowable surface and must not flag, got %+v", iss)
	}
}

func TestResolveDynamicNamedImports_CleanWhenExported(t *testing.T) {
	bodies := map[string]string{
		"x.js": `export const A = 1; export const B = 2;`,
		"f.js": `export async function go(){ const { A, B } = await import('./x.js'); return A + B; }`,
	}
	if iss := ResolveDynamicNamedImports("f.js", bodies, AnalyzeModules(bodies)); len(iss) != 0 {
		t.Fatalf("all destructured names are exported; must be clean, got %+v", iss)
	}
}

func TestResolveDynamicNamedImports_CommentedIgnored(t *testing.T) {
	bodies := map[string]string{
		"x.js": `export const A = 1;`,
		"f.js": "export async function go(){ // const { gone } = await import('./x.js');\n return 1; }",
	}
	if iss := ResolveDynamicNamedImports("f.js", bodies, AnalyzeModules(bodies)); len(iss) != 0 {
		t.Fatalf("a commented-out dynamic import must not flag, got %+v", iss)
	}
}
