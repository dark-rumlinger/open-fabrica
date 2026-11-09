package engine

// Dangling guide-reference guard.
//
// A gate message or guide that tells the model to `manage_guides action=recall
// slug=<x>` ships a DEAD instruction if <x> is not a real guide: the model recalls,
// gets nothing, and is left to rediscover the rule by trial. This test scans the
// repo for every `slug=<x>` reference in Go source and knowledge markdown and
// asserts each resolves to an embedded capability guide. Add a guide or fix the
// reference to make it pass.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"openfabrica/pack"
)

// slugRefRe matches a `slug=<name>` recall reference (the machine-actionable form
// the model literally passes to manage_guides). Front-matter `slug:` uses a colon,
// so it never matches here.
var slugRefRe = regexp.MustCompile(`slug=([a-zA-Z0-9_]+)`)

// proseRecallRe matches the prose forms of a recall pointer that dodge slugRefRe:
// the recall verb, an optional "topic", then a backticked name or a bare
// snake-case name (one carrying an underscore - that requirement keeps ordinary
// prose like "recall deeper sections" from matching). Prefer writing new
// pointers in the slug= form; this pattern is the safety net for prose that
// doesn't.
var proseRecallRe = regexp.MustCompile("(?i)recall (?:topic )?(?:`([a-zA-Z0-9_]+)`|([a-zA-Z0-9]+_[a-zA-Z0-9_]+))")

// wikiLinkRe matches a double-bracketed slug cross-reference in guide prose
// (written without brackets here so this comment can't self-match). The links
// are not rendered, so a dangling one ships to the model unvalidated without this.
var wikiLinkRe = regexp.MustCompile(`\[\[([a-zA-Z0-9_]+)\]\]`)

func repoRootDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed - cannot locate the repo root")
	}
	// file is <repo>/engine/guide_reference_test.go -> up two levels is <repo>.
	return filepath.Dir(filepath.Dir(file))
}

func TestNoDanglingGuideRecallReferences(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	valid := map[string]bool{}
	for _, g := range pack.Capabilities(ps) {
		valid[g.Slug] = true
	}
	if len(valid) < 5 {
		t.Fatalf("only %d capability guides loaded - did the knowledge tree move?", len(valid))
	}

	root := repoRootDir(t)
	dangling := map[string][]string{} // slug -> referencing locations
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, don't fail the walk
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "assets":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".md":
		default:
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		record := func(slug string) {
			if slug == "" || valid[slug] {
				return
			}
			rel, _ := filepath.Rel(root, path)
			dangling[slug] = append(dangling[slug], filepath.ToSlash(rel))
		}
		for _, m := range slugRefRe.FindAllSubmatch(b, -1) {
			record(string(m[1]))
		}
		for _, m := range proseRecallRe.FindAllSubmatch(b, -1) {
			slug := string(m[1])
			if slug == "" {
				slug = string(m[2])
			}
			record(slug)
		}
		for _, m := range wikiLinkRe.FindAllSubmatch(b, -1) {
			record(string(m[1]))
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
	if len(dangling) > 0 {
		slugs := make([]string, 0, len(dangling))
		for s := range dangling {
			slugs = append(slugs, s)
		}
		sort.Strings(slugs)
		var b strings.Builder
		for _, s := range slugs {
			fmt.Fprintf(&b, "\n  slug=%s referenced in %s but no such guide exists", s, strings.Join(dangling[s], ", "))
		}
		t.Errorf("dangling guide recall reference(s) - a recall instruction points at a guide that does not exist; write the guide or fix the slug:%s", b.String())
	}
}

// sectionRefRe matches a `slug=<x> section=<anchor>` recall pointer.
var sectionRefRe = regexp.MustCompile(`slug=([a-zA-Z0-9_]+)\s+section=([a-zA-Z0-9_-]+)`)

// deadRecallTopicRe matches the retired "recall topic" wording: manage_guides has
// no topic argument, so a pointer phrased that way names nothing callable.
var deadRecallTopicRe = regexp.MustCompile(`(?i)recall topic`)

// TestRecallSectionReferencesResolve holds the section half of the recall
// pointers: a `slug=<x> section=<anchor>` reference must name a real section of
// that guide (or "all"), and the retired "recall topic" phrasing must not return.
func TestRecallSectionReferencesResolve(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	bySlug := map[string]pack.Pack{}
	for _, g := range pack.Capabilities(ps) {
		bySlug[g.Slug] = g
	}
	root := repoRootDir(t)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "assets":
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".go" && ext != ".md" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if deadRecallTopicRe.Match(b) {
			t.Errorf("%s uses the retired \"recall topic\" wording; point at manage_guides action=recall slug=<slug>", rel)
		}
		for _, m := range sectionRefRe.FindAllSubmatch(b, -1) {
			slug, anchor := string(m[1]), string(m[2])
			g, ok := bySlug[slug]
			if !ok || strings.EqualFold(anchor, "all") {
				continue // unknown slugs are the sibling test's concern
			}
			if g.Section(anchor) == nil {
				t.Errorf("%s: slug=%s section=%s names no section of that guide (available: %s)", rel, slug, anchor, g.AnchorList())
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
}
