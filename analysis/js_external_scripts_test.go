package analysis

import (
	"strings"
	"testing"
)

func TestCrossOriginScriptInjection(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string // substrings each expected URL must contain; empty = expect none
	}{
		{"createElement + .src", `var s=document.createElement('script'); s.src="https://cdn.jsdelivr.net/npm/qrcode@1.5.3/build/qrcode.min.js";`,
			[]string{"cdn.jsdelivr.net"}},
		{"setAttribute src", `const s=document.createElement("script"); s.setAttribute('src','https://unpkg.com/x@1/x.js');`,
			[]string{"unpkg.com"}},
		{"static import url", `import {a} from 'https://cdn.example.com/lib.mjs';`, []string{"cdn.example.com"}},
		{"side-effect import", `import 'https://cdn.example.com/side.js';`, []string{"cdn.example.com"}},
		{"dynamic import", `const m = await import("https://cdn.skypack.dev/confetti");`, []string{"cdn.skypack.dev"}},

		// Conservative: none of these should be reported.
		{"bare specifier import", `import {camera} from 'three';`, nil},
		{"relative import", `import {s} from './app.state.js';`, nil},
		{"local script src", `var s=document.createElement('script'); s.src='/files/util.js';`, nil},
		{"cross-origin image, no script", `img.src='https://cdn.example.com/pic.png';`, nil},
		{"cross-origin image src beside local script", `var s=document.createElement('script'); s.src='/x.js'; img.src='https://cdn.example.com/a.webp';`, nil},
		{"remote fetch only", `await fetch('https://api.example.com/data');`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CrossOriginScriptInjection(c.body)
			if len(c.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("expected no injection, got %v", got)
				}
				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("expected %d url(s), got %v", len(c.want), got)
			}
			for i, sub := range c.want {
				if !strings.Contains(got[i], sub) {
					t.Errorf("url %d = %q, want substring %q", i, got[i], sub)
				}
			}
		})
	}
}
