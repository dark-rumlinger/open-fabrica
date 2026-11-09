package content

import (
	"fmt"
	"testing"
)

// The content-addressed cache must be transparent: a repeat parse of the same body
// returns the same verdict, and the two-generation swap at the cap never changes a
// result or loses correctness for a still-live body.
func TestCodeSyntaxCacheTransparent(t *testing.T) {
	const clean = "export function f(){ return 1 }\n"
	const broken = "export function f(){ return 1 \n" // missing brace

	g1, c1, _ := codeSyntax("file", "app.js", clean)
	g2, c2, _ := codeSyntax("file", "app.js", clean) // served from cache
	if g1 != g2 || c1 != c2 || !g1 || !c1 {
		t.Fatalf("clean body: first=(%v,%v) cached=(%v,%v), want gated+clean both times", g1, c1, g2, c2)
	}

	bg, bc, bd := codeSyntax("file", "app.js", broken)
	if !bg || bc || bd == "" {
		t.Fatalf("broken body must parse gated+not-clean with a detail, got (%v,%v,%q)", bg, bc, bd)
	}
	// Same body re-parsed returns the identical detail from cache.
	if _, _, bd2 := codeSyntax("file", "app.js", broken); bd2 != bd {
		t.Fatalf("cached broken detail = %q, want %q", bd2, bd)
	}

	// Overflow the current generation so it becomes prev and a fresh current starts,
	// then confirm the original clean body still parses correctly (promoted on hit).
	for i := 0; i < syntaxCacheMax+10; i++ {
		body := fmt.Sprintf("export const v%d = %d\n", i, i)
		codeSyntax("file", "app.js", body)
	}
	if g, c, _ := codeSyntax("file", "app.js", clean); !g || !c {
		t.Fatalf("after a generation swap the clean body = (%v,%v), want gated+clean", g, c)
	}
}
