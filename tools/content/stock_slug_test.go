package content

import (
	"strings"
	"testing"
)

// Two long prompts that share their first words must not share a filename: generation
// and fetch both upsert by path, so a shared prefix silently replaced the earlier image.
func TestSlugifyPrefix_LongPromptsStayDistinct(t *testing.T) {
	a := slugifyPrefix("professional photograph of a golden retriever puppy playing in autumn leaves")
	b := slugifyPrefix("professional photograph of a golden retriever puppy sleeping on a sofa")
	if a == b {
		t.Fatalf("distinct long prompts collapsed to %q", a)
	}
	if again := slugifyPrefix("professional photograph of a golden retriever puppy playing in autumn leaves"); again != a {
		t.Fatalf("the same prompt must be stable (a re-run replaces its own image): %q vs %q", a, again)
	}
	for _, s := range []string{a, b} {
		if len(s) > 24 || strings.HasPrefix(s, "-") || strings.Contains(s, "--") {
			t.Errorf("slug %q is not a short filename-safe prefix", s)
		}
	}
}

func TestSlugifyPrefix_ShortAndEmptyKeepTheirShape(t *testing.T) {
	if got := slugifyPrefix("Sunset Beach"); got != "sunset-beach" {
		t.Errorf("short query slug = %q", got)
	}
	if got := slugifyPrefix("!!!"); got != "photo" {
		t.Errorf("empty slug = %q, want the photo fallback", got)
	}
}
