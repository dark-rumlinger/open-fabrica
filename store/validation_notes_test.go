package store

import (
	"strings"
	"testing"
)

// These guards lock the "one home per check" consolidation for the shared
// validation notes: a rejection surface touching an asset-tag or path-parameter
// mistake must END with the single canonical note, not a paraphrase - these
// tests fail first when a surface drifts.

func TestAssetRejectionsShareTheChannelNote(t *testing.T) {
	// Cross-origin surface (validateNoCrossOriginAssets, joining the stylesheet
	// and script checks).
	crossOrigin := `<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Inter">
<script src="https://cdn.jsdelivr.net/npm/foo.js"></script>`
	if err := validateNoCrossOriginAssets(crossOrigin, "layout"); err == nil {
		t.Fatal("expected a cross-origin asset rejection")
	} else if !strings.Contains(err.Error(), AssetLoadingNote) {
		t.Errorf("cross-origin rejection must end with the shared AssetLoadingNote; got: %v", err)
	}

	// Same-origin surface (validateNoSameOriginAssetTags).
	sameOrigin := `<script src="/assets/game.js" type="module"></script>`
	if err := validateNoSameOriginAssetTags(sameOrigin, "page"); err == nil {
		t.Fatal("expected a same-origin asset rejection")
	} else if !strings.Contains(err.Error(), AssetLoadingNote) {
		t.Errorf("same-origin rejection must end with the shared AssetLoadingNote; got: %v", err)
	}
}

func TestPagePathParamRejectionsShareTheNote(t *testing.T) {
	// Each dynamic-segment syntax the page router does not accept must carry the
	// shared PathParamNote so the page and endpoint grammars never teach halves.
	for _, p := range []string{"/item/{id}", "/item/[id]", "/user/<name>"} {
		err := validatePagePath(p)
		if err == nil {
			t.Errorf("%q should be rejected", p)
			continue
		}
		if !strings.Contains(err.Error(), PathParamNote) {
			t.Errorf("%q rejection must carry the shared PathParamNote; got: %v", p, err)
		}
	}
}

// The notes must stay substantive - a future edit that guts one to a stub would
// pass the "is referenced" checks above while silently removing the guidance.
func TestValidationNotesStaySubstantive(t *testing.T) {
	for _, tok := range []string{"attach", "{{assets}}", "manage_libraries", "external_libraries"} {
		if !strings.Contains(AssetLoadingNote, tok) {
			t.Errorf("AssetLoadingNote lost the %q channel", tok)
		}
	}
	for _, tok := range []string{":param", "/api/<path>/<id>"} {
		if !strings.Contains(PathParamNote, tok) {
			t.Errorf("PathParamNote lost the %q half of the rule", tok)
		}
	}
}
