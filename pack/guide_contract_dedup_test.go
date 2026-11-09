package pack

import (
	"strings"
	"testing"

	"openfabrica/contracts"
)

// TestRenderGuidesCoreDedupesSharedContracts: two co-selected guides that both
// transclude endpoint-routes ship ONE full copy and a pointer line, not two
// verbatim copies in the same seed.
func TestRenderGuidesCoreDedupesSharedContracts(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	var sel []Pack
	for _, g := range Capabilities(ps) {
		if g.Slug == "file_uploads" || g.Slug == "server_push" {
			sel = append(sel, g)
		}
	}
	if len(sel) != 2 {
		t.Fatalf("expected file_uploads and server_push embedded; got %d guides", len(sel))
	}

	out := RenderGuidesCore(sel)
	full := strings.TrimSpace(contracts.Render("endpoint-routes"))
	if full == "" {
		t.Fatal("contract endpoint-routes did not render")
	}
	if n := strings.Count(out, full); n != 1 {
		t.Errorf("endpoint-routes should render in full exactly once; got %d", n)
	}
	if !strings.Contains(out, "(contract endpoint-routes is rendered in full in an earlier guide above)") {
		t.Errorf("later occurrence should collapse to a pointer line")
	}

	// Byte-determinism for a fixed selection: the block rides the cached seed.
	if out != RenderGuidesCore(sel) {
		t.Errorf("RenderGuidesCore must render identical bytes for the same selection")
	}

	// A single-guide selection is unaffected: its contract renders in full.
	single := RenderGuidesCore(sel[:1])
	if n := strings.Count(single, full); n != 1 {
		t.Errorf("single-guide selection should carry the full contract once; got %d", n)
	}
}
