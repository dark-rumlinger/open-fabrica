package public

import (
	"strings"
	"testing"
)

// Inline scripts (no src) are stamped with the nonce; external scripts are left
// alone (a nonce-based script-src admits them via 'self'/host-source).
func TestStampInlineScriptNonce(t *testing.T) {
	doc := `<html><head>` +
		`<script type="importmap">{"imports":{}}</script>` +
		`<script src="/files/app.js"></script>` +
		`</head><body><script>console.log(1)</script></body></html>`

	out := stampInlineScriptNonce(doc, "ABC123")

	if n := strings.Count(out, `nonce="ABC123"`); n != 2 {
		t.Fatalf("expected 2 inline scripts stamped, got %d:\n%s", n, out)
	}
	if strings.Contains(out, `src="/files/app.js" nonce=`) || strings.Contains(out, `<script nonce="ABC123" src="/files/app.js"`) {
		t.Fatalf("external src script must not be stamped:\n%s", out)
	}
	// The body's inline script must carry the nonce.
	if !strings.Contains(out, `<script nonce="ABC123">console.log(1)</script>`) {
		t.Fatalf("inline body script was not stamped:\n%s", out)
	}
}

func TestStampInlineScriptNonce_EmptyNonceNoop(t *testing.T) {
	doc := `<script>x()</script>`
	if got := stampInlineScriptNonce(doc, ""); got != doc {
		t.Fatalf("empty nonce must be a no-op, got %q", got)
	}
}

// An already-nonced inline script is not double-stamped.
func TestStampInlineScriptNonce_Idempotentish(t *testing.T) {
	doc := `<script nonce="OLD">y()</script>`
	out := stampInlineScriptNonce(doc, "NEW")
	if strings.Contains(out, `nonce="NEW"`) {
		t.Fatalf("a script that already has a nonce must be left alone: %s", out)
	}
}
