package content

import (
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// A single full-body save over the size ceiling is refused with the oversized_body
// code (it would risk truncating mid-write and shipping a corrupt artifact); a body
// at or under the ceiling saves normally.
func TestArtSave_OversizedBodyRefused(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db}

	// Comment lines stay syntactically clean so only the size guard can trip.
	huge := "// big\n" + strings.Repeat("//x\n", (store.MaxSingleSaveBody/4)+16)
	if len(huge) <= store.MaxSingleSaveBody {
		t.Fatalf("test body must exceed the ceiling, got %d <= %d", len(huge), store.MaxSingleSaveBody)
	}
	res, err := (&ArtSave{Kind: "file", FilePath: "js/huge.js", Body: huge}).Run(ctx)
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("an oversized save must be refused, got %+v", res)
	}
	if res.Code != "oversized_body" {
		t.Errorf("want code=oversized_body, got %q", res.Code)
	}

	// A body under the ceiling saves fine.
	ok := "// small\n" + strings.Repeat("//y\n", 100)
	res, err = (&ArtSave{Kind: "file", FilePath: "js/ok.js", Body: ok}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("an under-ceiling save must succeed: err=%v res=%+v", err, res)
	}
}

// A corrective full re-save of a file already grown past the single-save ceiling
// must NOT be refused: the oversized_body gate fires only on CROSSING the ceiling
// (prevBody at/under it), mirroring the client-module gate, so the fix-pass
// "re-save the whole file" path is never bounced back into a patch loop. In
// production a file reaches this size via warn-only patch growth (the
// save gate never re-checks a partial edit's resulting size); here we seed it out
// of band, using CSS so only the oversized_body gate is in play.
func TestArtSave_DoesNotTrapAlreadyOversizedFile(t *testing.T) {
	db := setupBulkDB(t)
	ctx := &tools.ToolContext{DB: db, ReadTracker: tools.NewReadTracker()}

	big := "/* big */\n" + strings.Repeat(".cell{color:red}\n", 3400)
	if len(big) <= store.MaxSingleSaveBody {
		t.Fatalf("fixture must exceed the ceiling, got %d <= %d", len(big), store.MaxSingleSaveBody)
	}
	if _, _, err := store.ArtifactSave(db, &store.Artifact{
		Kind: "file", Path: "big.css", Storage: store.StorageInline,
		Body: big, ContentType: "text/css", Scope: "page", Status: "published",
	}, "test"); err != nil {
		t.Fatalf("seed big.css: %v", err)
	}

	// A modified body, still over the ceiling: prevBody is already over, so the
	// crossing guard skips the reject and the whole-file re-save lands.
	res, err := (&ArtSave{Kind: "file", FilePath: "big.css", Body: big + "/* touched */\n"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a re-save of an already-oversized file must not be trapped, got %+v", res)
	}
}
