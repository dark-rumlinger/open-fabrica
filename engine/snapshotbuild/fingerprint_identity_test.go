package snapshotbuild

import (
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"testing"
)

// fullSnapshot populates every entity kind with values that exercise each branch
// of the fingerprint encoding: empty and non-empty lists, both sides of every
// conditional flag, and the seeded / empty / unknown table counts.
func fullSnapshot() *ProjectSnapshot {
	sheet := func(p string, trunc bool) StyleContract {
		return StyleContract{
			Path:      p,
			Vars:      []string{"--bg", "--fg"},
			Classes:   []string{"btn", "card"},
			Keyframes: []string{"spin"},
			Truncated: trunc,
		}
	}
	return &ProjectSnapshot{
		Pages: []PageRef{
			{Path: "/", Title: "Home", Assets: []string{"a.css", "b.js"}},
			{Path: "/bare", Title: "", Assets: nil},
		},
		Layouts:    []LayoutRef{{Name: "main"}},
		Components: []ComponentRef{{Name: "nav"}},
		Tables: []TableRef{
			{Name: "seeded", Columns: []string{"id INTEGER", "name TEXT"}, RowCount: 3},
			{Name: "empty", Columns: []string{"id INTEGER"}, RowCount: 0},
			{Name: "unknown", Columns: nil, RowCount: -1},
		},
		Views: []ViewRef{{Name: "v_rev", Columns: []string{"date", "revenue REAL"}}},
		Endpoints: []EndpointRef{
			{Path: "todos", Methods: "GET,POST", Auth: true, AuthKind: authKindAPI},
			{Path: "public", Methods: "GET", Auth: false, AuthKind: ""},
			{Path: "users", Methods: "POST", Auth: false, AuthKind: authKindAuth},
			// One WS row per mode, plus both sides of the reducer bit. "inert" is an
			// authoritative endpoint whose reducer was never attached: it is the state
			// these bytes exist to make visible, and it must not encode the same as
			// "arena", which has one.
			{Path: "chat", Methods: "WS", AuthKind: authKindWS, WSMode: wsModeRelay},
			{Path: "world", Methods: "WS", AuthKind: authKindWS, WSMode: wsModeTick, TickHz: 20},
			{Path: "match", Methods: "WS", AuthKind: authKindWS, WSMode: wsModeMatchmaking},
			{Path: "arena", Methods: "WS", Auth: true, AuthKind: authKindWS,
				WSMode: wsModeAuthoritative, TickHz: 30, HasReducer: true},
			{Path: "inert", Methods: "WS", AuthKind: authKindWS,
				WSMode: wsModeAuthoritative, TickHz: 30, HasReducer: false},
		},
		Libraries: []LibraryRef{
			{Name: "three", URL: "https://cdn/three.js"},
			{Name: "bare", URL: ""},
		},
		Files: []FileRef{
			{Path: "js/main.js", Kind: "js", Scope: "page"},
			{Path: "css/app.css", Kind: "css", Scope: "global"},
		},
		UnlinkedPageFiles: []string{"js/orphan.js"},
		Modules: []ModuleContract{
			{
				Path:    "js/main.js",
				Exports: []string{"init", "reset"},
				Imports: []ModuleImport{
					{Spec: "./input.js", Names: []string{"read", "write"}},
					{Spec: "./state.js", Names: nil},
				},
			},
			{Path: "js/leaf.js", Exports: nil, Imports: nil},
		},
		Styles: []StyleContract{sheet("css/app.css", false), sheet("css/big.css", true)},
		PageDOM: []DOMContract{
			{Path: "/", IDs: []string{"root", "board"}, Truncated: false},
			{Path: "/big", IDs: []string{"x"}, Truncated: true},
		},
		PageStyles: []PageStyleContract{
			{Page: "/", Sheets: []StyleContract{sheet("css/home.css", false), sheet("css/home2.css", true)}},
			{Page: "/none", Sheets: nil},
		},
	}
}

// dumpEntityBytes renders fullSnapshot()'s fingerprint encoding as sorted
// key/hex lines: the exact byte sequences, in a form a diff can compare.
func dumpEntityBytes() string {
	lines := []string{}
	for _, d := range fullSnapshot().entityList() {
		lines = append(lines, fmt.Sprintf("%-28s %s", d.key, hex.EncodeToString([]byte(d.desc))))
	}
	sort.Strings(lines)
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

// TestEntityBytesGolden pins the fingerprint encoding byte-for-byte against
// testdata/entity_bytes.golden. These bytes decide when the model is re-shown
// project state and when a cached prompt prefix stays valid; nothing downstream
// reveals a mistake in them (the only symptom is a prompt cache that churns or
// goes stale), so the encoding is compared directly. Regenerate with -update
// ONLY when the encoding change is intended; every stored baseline re-renders
// once against a new format.
func TestEntityBytesGolden(t *testing.T) {
	const golden = "testdata/entity_bytes.golden"
	got := dumpEntityBytes()

	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden updated")
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading %s (regenerate with UPDATE_GOLDEN=1): %v", golden, err)
	}
	if got != string(want) {
		t.Errorf("fingerprint encoding drifted from the golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// moduleBytes returns the fingerprint bytes of the one module in a snapshot.
func moduleBytes(t *testing.T, m ModuleContract) string {
	t.Helper()
	for _, e := range (&ProjectSnapshot{Modules: []ModuleContract{m}}).entityList() {
		if e.key == "module:"+m.Path {
			return e.desc
		}
	}
	t.Fatalf("no module entity for %s", m.Path)
	return ""
}

// Everything moduleRecord renders must move the fingerprint, or a changed arity or
// field set emits no delta and the model wires against a stale line.
func TestModuleFingerprintCoversRenderedContract(t *testing.T) {
	base := ModuleContract{
		Path: "js/a.js", Bare: "a", Exports: []string{"f", "S"},
		Sigs:   map[string][]string{"f": {"x"}},
		Shapes: map[string][]string{"S": {"p", "q"}},
	}
	ref := moduleBytes(t, base)

	arity := base
	arity.Sigs = map[string][]string{"f": {"x", "y"}}
	shape := base
	shape.Shapes = map[string][]string{"S": {"p", "q", "r"}}
	bare := base
	bare.Bare = ""
	bare.Collides = true
	for name, m := range map[string]ModuleContract{"arity": arity, "shape": shape, "collision": bare} {
		if moduleBytes(t, m) == ref {
			t.Errorf("%s change left the module fingerprint unchanged", name)
		}
	}
}
