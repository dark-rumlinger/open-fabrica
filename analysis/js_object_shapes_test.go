package analysis

import (
	"reflect"
	"testing"
)

func TestExtractExportedObjectShapes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want map[string][]string
	}{
		{
			name: "exported const object literal",
			src:  `export const state = { players: {}, input: {}, myId: null, net };`,
			want: map[string][]string{"state": {"players", "input", "myId", "net"}},
		},
		{
			name: "exported let object literal with methods and accessors",
			src: `export let ui = {
				score: 0,
				reset() { this.score = 0; },
				get ready() { return true; },
			};`,
			want: map[string][]string{"ui": {"score", "reset", "ready"}},
		},
		{
			name: "factory function returning an object literal",
			src: `export function packState() {
				const x = 1;
				return { x, y: 2, z: 3, yaw: 0 };
			}`,
			want: map[string][]string{"packState": {"x", "y", "z", "yaw"}},
		},
		{
			// A nested object REPORTS its keys; that is not a leak - the mismatch this
			// extractor catches lives inside the nested bag, and depth 1 cannot tell
			// `pos.horizontal` from `pos.x`. Values are never reported; an array value stays bare.
			name: "nested object values report their keys, one level down",
			src:  `export const cfg = { pos: { x: 1, y: 2 }, tags: [1, 2], name: "a" };`,
			want: map[string][]string{"cfg": {"pos{ x, y }", "tags", "name"}},
		},
		{
			// The depth cap holds: `b`'s own keys are not chased. Deeper than two is a
			// config tree, not a cross-module contract.
			name: "nesting stops at the depth cap",
			src:  `export const deep = { a: { b: { c: 1 } } };`,
			want: map[string][]string{"deep": {"a{ b }"}},
		},
		{
			// The `export const dom = {}` + `cacheDom()` pattern: the literal is empty,
			// so assignment-added keys make the shape honest. `===` must not create a
			// key, and a nested target reports only the field the sibling reads.
			name: "keys added by assignment are reported",
			src: `export const dom = {};
export function cacheDom() {
  dom.jumpBtn = document.getElementById('jump-btn');
  dom.chatLog = document.getElementById('chat');
  if (dom.jumpBtn === null) return;
  dom.chatLog.textContent = '';
}`,
			want: map[string][]string{"dom": {"jumpBtn", "chatLog"}},
		},
		{
			// Nothing to say beats saying `x{  }`.
			name: "an object that stays empty yields no entry",
			src:  `export const empty = {};`,
			want: nil,
		},
		{
			name: "spread and computed keys are skipped",
			src:  `export const merged = { ...base, [dynamic]: 1, keep: 2 };`,
			want: map[string][]string{"merged": {"keep"}},
		},
		{
			name: "commas and colons inside strings do not split keys",
			src:  `export const t = { label: "a, b: c", val: 1 };`,
			want: map[string][]string{"t": {"label", "val"}},
		},
		{
			name: "non-object exports yield no entry",
			src:  `export const N = 5; export function f(a) { return a; } export let s = "x";`,
			want: nil,
		},
		{
			name: "commented-out object is ignored",
			src:  "// export const ghost = { a: 1 };\nexport const real = { b: 2 };",
			want: map[string][]string{"real": {"b"}},
		},
		{
			name: "no exports",
			src:  `const local = { a: 1 };`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractExportedObjectShapes(tc.src)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExtractExportedObjectShapes()\n got  %#v\n want %#v", got, tc.want)
			}
		})
	}
}

func TestExtractExportedObjectShapes_KeyCap(t *testing.T) {
	var sb []byte
	sb = append(sb, []byte("export const big = {")...)
	for i := 0; i < maxObjectShapeKeys+10; i++ {
		sb = append(sb, []byte(" k")...)
		sb = append(sb, []byte{byte('0' + i%10)}...)
		sb = append(sb, []byte("_")...)
		sb = append(sb, []byte{byte('a' + i%26)}...)
		sb = append(sb, ':', ' ', '1', ',')
	}
	sb = append(sb, '}', ';')
	got := ExtractExportedObjectShapes(string(sb))["big"]
	if len(got) != maxObjectShapeKeys+1 {
		t.Fatalf("capped list len = %d, want %d", len(got), maxObjectShapeKeys+1)
	}
	if got[len(got)-1] != "…" {
		t.Fatalf("last element = %q, want ellipsis", got[len(got)-1])
	}
}
