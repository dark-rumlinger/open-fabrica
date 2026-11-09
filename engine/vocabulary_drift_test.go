package engine

// Vocabulary single-home guard for the shared-module concept. The concept was
// renamed to "zero-import store module" across every LLM-facing surface after a
// partial rename left FOUR names live at once ("state leaf", "zero-import
// leaf", "zero-import shared leaf", "zero-import state/util leaf") - two of
// them inside one reject message file. A model that meets two names for one
// concept must decide whether they differ; this pins the survivor. "leaf" alone
// stays legal: a leaf module (imports the store) is a different concept.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/pack"
)

// forbiddenVocabulary lists retired names for the store-module concept. Matched
// case-insensitively against pack bodies (contracts transcluded) and non-test
// Go sources; Go identifiers (camelCase, no spaces) never match.
var forbiddenVocabulary = []string{
	"state leaf",
	"state-leaf",
	"zero-import leaf",
	"zero-import shared leaf",
	"state/util leaf",
}

func TestNoRetiredVocabularyInPacks(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	for _, p := range ps {
		body := strings.ToLower(pack.TranscludeContracts(p.Body))
		for _, phrase := range forbiddenVocabulary {
			if strings.Contains(body, phrase) {
				t.Errorf("pack %q says %q - the concept is named \"zero-import store module\" everywhere "+
					"the model reads; two names for one concept read as two concepts", p.Slug, phrase)
			}
		}
	}
}

func TestNoRetiredVocabularyInSource(t *testing.T) {
	for _, root := range []string{"../engine", "../tools", "../analysis", "../store"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			src := strings.ToLower(string(b))
			for _, phrase := range forbiddenVocabulary {
				if strings.Contains(src, phrase) {
					t.Errorf("%s says %q (string literal or comment) - use \"zero-import store module\"; "+
						"reject messages are LLM-facing and must carry one vocabulary", path, phrase)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
