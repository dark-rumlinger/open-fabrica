package content

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"openfabrica/pack"
	"openfabrica/store"
)

// The body-size ceilings the model reads in tool docstrings ("up to 48 KB ...")
// are hand-typed prose that a Go struct tag cannot compute, so they can silently
// drift from the real constants (MaxSingleSaveBody / MaxClientModuleBody) when a
// cap is retuned. This test is that drift guard: it derives the expected figures
// from the constants via store.HumanSize and fails if any tool prose quotes a KB
// size that is neither ceiling - the constant becomes the single home, the prose
// merely references it. (The write-path oversized errors already render the
// ceiling through HumanSize, so only these two static strings need guarding.)

// sizeProseHome is the ONE tool string that states both ceilings: the save body
// param description, read at the moment the model decides how much to write.
// Demanding them here and nowhere else is deliberate - requiring every size-adjacent
// string to repeat the figures is the guard mandating the duplication it exists to
// police, and a body that overflows is refused at write with the ceiling rendered
// through HumanSize anyway.
func sizeProseHome(t *testing.T) map[string]string {
	t.Helper()
	f, ok := reflect.TypeOf(ArtSave{}).FieldByName("Body")
	if !ok {
		t.Fatal("ArtSave has no Body field")
	}
	return map[string]string{"ArtSave.Body desc": f.Tag.Get("desc")}
}

// sizeProseAll is every size-adjacent string, home or pointer. None may quote a KB
// figure that no constant backs; a string with no figure at all is a pointer and
// passes, exactly as TestKnowledgeSizeProseMatchesConstants treats the pack tree.
func sizeProseAll(t *testing.T) map[string]string {
	t.Helper()
	all := sizeProseHome(t)
	all["ArtPatch.Doc()"] = (&ArtPatch{}).Doc()
	return all
}

var kbFigureRe = regexp.MustCompile(`\d+(?:\.\d+)?\s*KB`)

func TestBodySizeProseMatchesConstants(t *testing.T) {
	saveKB := store.HumanSize(store.MaxSingleSaveBody)     // e.g. "48 KB"
	moduleKB := store.HumanSize(store.MaxClientModuleBody) // e.g. "36 KB"

	// Both ceilings must be present at the home, spelled as the constant renders them.
	for where, prose := range sizeProseHome(t) {
		if !strings.Contains(prose, saveKB) {
			t.Errorf("%s: missing save ceiling %q (derived from MaxSingleSaveBody); prose=%q", where, saveKB, prose)
		}
		if !strings.Contains(prose, moduleKB) {
			t.Errorf("%s: missing client-module ceiling %q (derived from MaxClientModuleBody); prose=%q", where, moduleKB, prose)
		}
	}
	for where, prose := range sizeProseAll(t) {
		// No stray KB figure may appear that is not one of the two real ceilings -
		// catches a hand-typed "50 KB" that no constant backs.
		for _, fig := range kbFigureRe.FindAllString(prose, -1) {
			norm := strings.ReplaceAll(fig, " ", "")
			if norm != strings.ReplaceAll(saveKB, " ", "") && norm != strings.ReplaceAll(moduleKB, " ", "") {
				t.Errorf("%s: quotes KB size %q that matches neither ceiling (%q / %q)", where, fig, saveKB, moduleKB)
			}
		}
	}
}

// TestKnowledgeSizeProseMatchesConstants extends the same rule to the knowledge
// tree, which cannot compute a figure from the constants. A pack line that names
// one of the two body-size gates and quotes a KB number must quote that gate's
// own ceiling, so retuning a constant breaks the guide rather than leaving it to
// teach a stale cap. A line with no KB figure is a pointer and passes untouched;
// unrelated KB figures elsewhere in the tree (message ceilings, state limits) are
// never scanned.
func TestKnowledgeSizeProseMatchesConstants(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	gates := map[string]string{
		"module_too_large_client": store.HumanSize(store.MaxClientModuleBody),
		"oversized_body":          store.HumanSize(store.MaxSingleSaveBody),
	}
	for _, p := range ps {
		for _, line := range strings.Split(p.Body, "\n") {
			for gate, want := range gates {
				if !strings.Contains(line, gate) {
					continue
				}
				wantNorm := strings.ReplaceAll(want, " ", "")
				for _, fig := range kbFigureRe.FindAllString(line, -1) {
					if strings.ReplaceAll(fig, " ", "") != wantNorm {
						t.Errorf("pack %q quotes %q alongside the %s gate; that gate's ceiling is %q",
							p.Slug, fig, gate, want)
					}
				}
			}
		}
	}
}
