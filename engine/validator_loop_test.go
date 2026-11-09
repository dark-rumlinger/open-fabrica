package engine

import (
	"fmt"
	"testing"

	"openfabrica/store"
)

func TestFixableErrorsFiltersAndSorts(t *testing.T) {
	issues := []Issue{
		{Severity: store.SeverityWarning, Category: "runtime", Page: "/", Deterministic: true}, // warning: dropped
		{Severity: store.SeverityError, Category: "runtime", Page: "/", Deterministic: false},  // non-deterministic: dropped
		{Severity: store.SeverityError, Category: "module_imports", File: "z.js", Deterministic: true},
		{Severity: store.SeverityError, Category: "js_syntax", File: "a.js", Deterministic: true},
	}
	out := FixableErrors(issues)
	if len(out) != 2 {
		t.Fatalf("want 2 fixable errors (warnings and non-deterministic dropped), got %d: %+v", len(out), out)
	}
	// Sorted by fingerprint: "js_syntax\x00\x00a.js" sorts before "module_imports\x00\x00z.js".
	if out[0].Category != "js_syntax" || out[1].Category != "module_imports" {
		t.Fatalf("fixable errors must be sorted by fingerprint for a stable seed, got %+v", out)
	}
}

// TestFingerprintDiscriminatesBySignature: two runtime issues sharing
// category+page+file (a page's File is "") are the SAME fingerprint when their
// Signature matches and DIFFERENT when it does not - the property the layered
// fix-loop depends on. An empty Signature reproduces the prior coarse identity.
func TestFingerprintDiscriminatesBySignature(t *testing.T) {
	base := Issue{Severity: store.SeverityError, Category: "runtime", Page: "/", Deterministic: true}
	a, b, same := base, base, base
	a.Signature = "makeFloraMesh@game.terrain.js"
	b.Signature = "genFloraName@game.terrain.js"
	same.Signature = "makeFloraMesh@game.terrain.js"
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatalf("distinct failing sites must not collapse to one fingerprint:\n %q\n %q", a.Fingerprint(), b.Fingerprint())
	}
	if a.Fingerprint() != same.Fingerprint() {
		t.Fatalf("the same failing site must share a fingerprint:\n %q\n %q", a.Fingerprint(), same.Fingerprint())
	}
	// Empty signature preserves the pre-existing category+page+file identity.
	none1, none2 := base, base
	if none1.Fingerprint() != none2.Fingerprint() {
		t.Fatal("issues with no signature must keep the coarse category+page+file identity")
	}
}

// Sanity: fingerprints ignore the free-text message so the same defect with a
// run-varying message string is recognised as the same finding, not a new one.
func TestFingerprintIgnoresMessage(t *testing.T) {
	base := Issue{Severity: store.SeverityError, Category: "runtime", Page: "/play", Deterministic: true}
	a := base
	a.Message = "navigation error: context canceled; server returned HTTP 404"
	b := base
	b.Message = "navigation error: net::ERR_ABORTED; requested URL: /play/7"
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("same defect, different message must share a fingerprint:\n %q\n %q", a.Fingerprint(), b.Fingerprint())
	}
	_ = fmt.Sprint(a.Fingerprint())
}
