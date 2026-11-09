package engine

import "testing"

// TestEngineLibReWholeWord pins the tightened engine-library matcher: real
// specifiers (bare name, dotted, concatenated, scoped, CDN URL) still imply a
// client-JS split, but a token that merely CONTAINS an engine name followed by a
// digit ("babylon5") or embedded mid-word ("underthree") does not.
func TestEngineLibReWholeWord(t *testing.T) {
	cases := []struct {
		spec string
		want bool
	}{
		{"three", true},
		{"three.js", true},
		{"threejs", true},
		{"three@0.170.0", true},
		{"@babylonjs/core", true},
		{"https://cdn/three.module.js", true},
		{"phaser", true},
		{"a pixi.min.js b", true},
		{"babylon5", false},
		{"underthree", false},
		{"react", false},
		{"d3", false},
		{"", false},
	}
	for _, c := range cases {
		if got := engineLibRe.MatchString(c.spec); got != c.want {
			t.Errorf("engineLibRe.MatchString(%q) = %v, want %v", c.spec, got, c.want)
		}
	}
}
