package pack

import (
	"io/fs"
	"path"
	"strings"
	"testing"

	"openfabrica/knowledge"
)

// TestGuideTreeIsConsistent holds the structural rules of the guide tree that the
// loader silently tolerates: a stray slug, category or cross-reference would not
// fail to load, it would just stop routing.
func TestGuideTreeIsConsistent(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	caps := Capabilities(ps)
	slugs := map[string]bool{}
	for _, g := range caps {
		if slugs[g.Slug] {
			t.Errorf("slug %q is used by more than one guide", g.Slug)
		}
		slugs[g.Slug] = true
	}

	// Folder layout: guides/<category>/<slug>/GUIDE.md, matching the front-matter,
	// and a CATEGORY.md for every category.
	found := map[string]string{} // slug -> category dir
	categories := map[string]bool{}
	err = fs.WalkDir(knowledge.FS, "guides", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "GUIDE.md" {
			return err
		}
		parts := strings.Split(p, "/") // guides, <category>, <slug>, GUIDE.md
		if len(parts) != 4 {
			t.Errorf("%s: a guide lives at guides/<category>/<slug>/GUIDE.md", p)
			return nil
		}
		found[parts[2]] = parts[1]
		categories[parts[1]] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk guides: %v", err)
	}
	for _, g := range caps {
		dir, ok := found[g.Slug]
		if !ok {
			t.Errorf("guide %q has no folder named after its slug", g.Slug)
			continue
		}
		if dir != g.Category {
			t.Errorf("guide %q sits in guides/%s but declares category %q", g.Slug, dir, g.Category)
		}
	}
	for cat := range categories {
		if _, err := fs.Stat(knowledge.FS, path.Join("guides", cat, "CATEGORY.md")); err != nil {
			t.Errorf("category %q has no CATEGORY.md", cat)
		}
	}

	// Cross-references name real guides; a miss is dropped silently at selection.
	for _, g := range caps {
		for _, list := range [][]string{g.Requires, g.ExclusiveWith} {
			for _, ref := range list {
				if !slugs[ref] {
					t.Errorf("guide %q references unknown guide %q", g.Slug, ref)
				}
			}
		}
		for _, ref := range g.RequiresAnyOf {
			if spec, ok := strings.CutPrefix(strings.TrimSpace(ref), "decision:"); ok {
				topic, choice, hasChoice := strings.Cut(spec, "/")
				if !hasChoice || strings.TrimSpace(topic) == "" || strings.TrimSpace(choice) == "" {
					t.Errorf("guide %q has a malformed decision anchor %q (want decision:<topic>/<choice>)", g.Slug, ref)
				}
				continue
			}
			if !slugs[strings.TrimSpace(ref)] {
				t.Errorf("guide %q requires_any_of names unknown guide %q", g.Slug, ref)
			}
		}
	}

	// Every extended section says when to pull it; the section index falls back to
	// the bare title otherwise, which does not tell the model why to recall it.
	for _, g := range caps {
		for _, s := range g.Sections {
			if strings.TrimSpace(s.When) == "" {
				t.Errorf("guide %q section %q has no `> when:` hint", g.Slug, s.Anchor)
			}
		}
	}
}
