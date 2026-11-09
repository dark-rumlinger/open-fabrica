package engine

import (
	"strings"
	"testing"
)

// deriveModuleName turns a library name into the bare import specifier for the
// common "named after the specifier" case, and refuses to guess a junk specifier
// from a descriptive name.
func TestDeriveModuleName(t *testing.T) {
	cases := map[string]string{
		"three.js":        "three",
		"three":           "three",
		"three@0.170.0":   "three",
		"@babylonjs/core": "@babylonjs/core",
		"app.module.js":   "app",
		"Three.js engine": "", // descriptive prose, not a specifier
		"":                "",
	}
	for in, want := range cases {
		if got := deriveModuleName(in); got != want {
			t.Errorf("deriveModuleName(%q) = %q, want %q", in, got, want)
		}
	}
}

// fillModuleNames guarantees a kind="module" library carries a module_name (the
// importmap key) when the planner forgot it, derives nothing for classic libs,
// and never clobbers a module_name that is already set.
func TestFillModuleNames(t *testing.T) {
	p := &Plan{ExternalLibraries: []LibrarySpec{
		{Name: "three.js", URL: "u", Kind: "module"},                    // missing -> derived "three"
		{Name: "marked", URL: "u", Kind: "classic"},                     // classic -> untouched
		{Name: "d3", URL: "u", Kind: "module", ModuleName: "d3-custom"}, // set -> untouched
		{Name: "Some 3D Engine", URL: "u", Kind: "module"},              // descriptive -> left empty
	}}
	fillModuleNames(p)
	if p.ExternalLibraries[0].ModuleName != "three" {
		t.Errorf("module lib missing module_name should derive \"three\", got %q", p.ExternalLibraries[0].ModuleName)
	}
	if p.ExternalLibraries[1].ModuleName != "" {
		t.Errorf("classic lib must not get a module_name, got %q", p.ExternalLibraries[1].ModuleName)
	}
	if p.ExternalLibraries[2].ModuleName != "d3-custom" {
		t.Errorf("an explicit module_name must not be clobbered, got %q", p.ExternalLibraries[2].ModuleName)
	}
	if p.ExternalLibraries[3].ModuleName != "" {
		t.Errorf("a descriptive name must stay empty (no junk specifier), got %q", p.ExternalLibraries[3].ModuleName)
	}
}

// fillModuleNames promotes an ES-module library that omitted `kind`: an ESM URL
// (/esm/, .mjs, +esm) with no kind would otherwise default to classic, stay out
// of the importmap, and throw "Failed to resolve module specifier" at runtime
// (the simplex-noise live-build failure). A font/CSS URL with no kind stays
// classic, and an explicit classic is never overridden. General - any library.
func TestFillModuleNames_PromotesESMURL(t *testing.T) {
	p := &Plan{ExternalLibraries: []LibrarySpec{
		{Name: "simplex-noise", URL: "https://cdn.jsdelivr.net/npm/simplex-noise@4.0.1/dist/esm/simplex-noise.js"}, // ESM URL, no kind -> promote
		{Name: "Orbitron", URL: "https://fonts.googleapis.com/css2?family=Orbitron&display=swap"},                  // font -> stays classic
		{Name: "marked", URL: "https://cdn.jsdelivr.net/npm/marked/marked.min.js", Kind: "classic"},                // explicit classic -> untouched
		{Name: "d3", URL: "https://cdn.jsdelivr.net/npm/d3@7/+esm"},                                                // +esm endpoint -> promote
	}}
	fillModuleNames(p)
	if p.ExternalLibraries[0].Kind != "module" || p.ExternalLibraries[0].ModuleName != "simplex-noise" {
		t.Errorf("ESM-URL lib must be promoted to module with module_name \"simplex-noise\", got kind=%q name=%q",
			p.ExternalLibraries[0].Kind, p.ExternalLibraries[0].ModuleName)
	}
	if strings.EqualFold(p.ExternalLibraries[1].Kind, "module") {
		t.Errorf("a font URL must stay classic, got kind=%q", p.ExternalLibraries[1].Kind)
	}
	if p.ExternalLibraries[2].Kind != "classic" {
		t.Errorf("an explicit classic must not be overridden, got kind=%q", p.ExternalLibraries[2].Kind)
	}
	if p.ExternalLibraries[3].Kind != "module" || p.ExternalLibraries[3].ModuleName != "d3" {
		t.Errorf("a +esm URL must be promoted to module with derived name, got kind=%q name=%q",
			p.ExternalLibraries[3].Kind, p.ExternalLibraries[3].ModuleName)
	}
}

// ParsePlan (via canonicalizePlan) fills the importmap-critical module_name so a
// planner that declares three.js as a module but forgets the specifier still
// produces a usable importmap entry.
func TestParsePlan_FillsModuleName(t *testing.T) {
	raw := `{"goal":"g","tasks":[{"title":"t"}],
		"external_libraries":[{"name":"three.js","url":"https://cdn/three.module.js","kind":"module"}]}`
	p, err := ParsePlan(raw)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if len(p.ExternalLibraries) != 1 || p.ExternalLibraries[0].ModuleName != "three" {
		t.Errorf("ParsePlan should fill module_name to \"three\", got %+v", p.ExternalLibraries)
	}
}

// A classic library that omits global_name gets its well-known global, so the
// undefined-reference gate keeps blocking; a module, a stylesheet, a name the planner
// gave and an unknown library are all left alone.
func TestFillGlobalNames(t *testing.T) {
	p := &Plan{ExternalLibraries: []LibrarySpec{
		{Name: "Chart.js", URL: "https://cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js", Kind: "classic"},
		{Name: "gsap", URL: "https://cdnjs.cloudflare.com/ajax/libs/gsap/3.12.5/gsap.min.js"},
		{Name: "three.js", URL: "https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js", Kind: "module"},
		{Name: "Open Sans", URL: "https://fonts.googleapis.com/css2?family=Open+Sans"},
		{Name: "marked", URL: "u.js", Kind: "classic", GlobalName: "markdownIt"},
		{Name: "obscure-lib", URL: "https://example.com/obscure.js", Kind: "classic"},
	}}
	fillModuleNames(p)
	fillGlobalNames(p)
	for i, want := range []string{"Chart", "gsap", "", "", "markdownIt", ""} {
		if got := p.ExternalLibraries[i].GlobalName; got != want {
			t.Errorf("%s: global_name = %q, want %q", p.ExternalLibraries[i].Name, got, want)
		}
	}
}
