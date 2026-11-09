// External test package (knowledge_test) so it can import pack/contracts, which
// both import knowledge - an internal test would cycle.
package knowledge_test

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/contracts"
	"openfabrica/knowledge"
	"openfabrica/pack"
)

// TestGuidesHaveRequiredFrontMatter: every capability guide carries the metadata
// the always-on index renders (slug/title/summary/category). A missing summary
// or category degrades the index, so guard it here.
func TestGuidesHaveRequiredFrontMatter(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	guides := pack.Capabilities(ps)
	if len(guides) == 0 {
		t.Fatal("no capability guides loaded from knowledge/guides")
	}
	for _, g := range guides {
		if strings.TrimSpace(g.Slug) == "" {
			t.Error("a guide has an empty slug")
		}
		if strings.TrimSpace(g.Title) == "" {
			t.Errorf("guide %q has an empty title", g.Slug)
		}
		if strings.TrimSpace(g.Summary) == "" {
			t.Errorf("guide %q has an empty summary - its index line would be blank", g.Slug)
		}
		if strings.TrimSpace(g.Category) == "" {
			t.Errorf("guide %q has an empty category - it would fall under 'general'", g.Slug)
		}
		// An evidence-gated guide is one the picker must NOT include on a hunch, so
		// it has to say what counts as evidence and what does not. Gated with only a
		// summary to go on is the worst case: the gate suppresses it while nothing
		// tells the picker how to clear the gate.
		if g.EvidenceGated {
			if strings.TrimSpace(g.UseWhen) == "" {
				t.Errorf("guide %q is evidence_gated but declares no use_when - the picker cannot tell what clears the gate", g.Slug)
			}
			if strings.TrimSpace(g.NotWhen) == "" {
				t.Errorf("guide %q is evidence_gated but declares no not_when - the picker cannot tell what to rule out", g.Slug)
			}
		}
	}
}

// TestGuideFrontMatterKeysAreKnown: every front-matter key in the knowledge tree
// must be one pack.Parse understands. Parse skips a key it has no case for, so a
// typo'd or invented key is dropped in silence and reads as working routing
// metadata that can never fire - which is exactly how a documented-but-unparsed
// key can sit in the tree unnoticed. Scans the raw files rather than parsed Packs,
// since by then the unknown keys are already gone.
func TestGuideFrontMatterKeysAreKnown(t *testing.T) {
	known := map[string]bool{}
	for _, k := range pack.KnownFrontMatterKeys() {
		known[k] = true
	}
	files, err := packFiles()
	if err != nil {
		t.Fatalf("walking the knowledge tree: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no pack files found - the walk is not reaching the knowledge tree")
	}
	for path, body := range files {
		for _, line := range frontMatterLines(body) {
			key, _, found := strings.Cut(line, ":")
			key = strings.TrimSpace(key)
			if !found || key == "" || strings.HasPrefix(key, "#") {
				continue
			}
			if !known[key] {
				t.Errorf("%s declares front-matter key %q, which pack.Parse ignores - it is silently dropped; add a case in pack.Parse (and to KnownFrontMatterKeys) or remove the key", path, key)
			}
		}
	}
}

// packFiles returns every GUIDE.md / STAGE.md in the embedded knowledge tree,
// keyed by path, as raw text - front-matter included.
func packFiles() (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(knowledge.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		base := filepath.Base(path)
		if base != "GUIDE.md" && base != "STAGE.md" {
			return nil
		}
		raw, err := fs.ReadFile(knowledge.FS, path)
		if err != nil {
			return err
		}
		out[path] = string(raw)
		return nil
	})
	return out, err
}

// frontMatterLines returns the lines between the opening and closing `---` of a
// pack file, or nil when it has no front-matter block.
func frontMatterLines(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	if !strings.HasPrefix(raw, "---\n") {
		return nil
	}
	rest := raw[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil
	}
	return strings.Split(rest[:end], "\n")
}

// TestGuideSummariesAreRoutable: a guide's summary is the ONLY line the PLAN guide
// picker (guideSelectionIndex) and the always-on index show for it, so it must be
// substantive and must not merely echo the title - a blank or title-equal summary
// gives the router nothing to route on and wastes an index line on every prompt.
func TestGuideSummariesAreRoutable(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	for _, g := range pack.Capabilities(ps) {
		s := strings.TrimSpace(g.Summary)
		if s == "" {
			continue // empty summary is already caught by TestGuidesHaveRequiredFrontMatter
		}
		if strings.EqualFold(s, strings.TrimSpace(g.Title)) {
			t.Errorf("guide %q summary just echoes its title %q - say what the capability is and when to reach for it", g.Slug, g.Title)
		}
		if len([]rune(s)) < 20 {
			t.Errorf("guide %q summary %q is too terse to route on (< 20 chars)", g.Slug, s)
		}
	}
}

// TestMentalModelContractsResolve: the mental model carries the laws as
// {{contract:id}} tokens and a {{GUIDE_INDEX}} marker; every contract must
// resolve and the marker must be present so the index has a home.
func TestMentalModelContractsResolve(t *testing.T) {
	body, err := knowledge.MentalModel()
	if err != nil {
		t.Fatalf("MentalModel: %v", err)
	}
	if !strings.Contains(body, "{{GUIDE_INDEX}}") {
		t.Error("mental-model.md is missing the {{GUIDE_INDEX}} marker")
	}
	all, err := contracts.Load()
	if err != nil {
		t.Fatalf("contracts.Load: %v", err)
	}
	for _, id := range contractTokens(body) {
		if _, ok := all[id]; !ok {
			t.Errorf("mental-model.md references unknown {{contract:%s}}", id)
		}
	}
}

// TestEveryGuideAppearsInAssembledIndex: the assembled system prompt's always-on
// index lists every capability guide by slug (nothing is silently dropped), the
// laws are transcluded (no raw token survives), and no guide is inlined as a full
// body IN THE SYSTEM PROMPT. (Matched guides ARE inlined elsewhere: the BUILD
// stage brief carries their cores via the build intent seed, the recall-only
// rule is scoped to this assembled index, not to every prompt the model sees.)
//
// Checked on PLAN, which chooses its own guides and so carries the full index.
// BUILD receives its guides prefetched and carries a pointer instead.
func TestEveryGuideAppearsInAssembledIndex(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	sys := pack.Assemble(pack.AssembleInput{Stage: "plan", Wave: "w", Selection: pack.Select(ps, "plan")}).System
	for _, g := range pack.Capabilities(ps) {
		if !strings.Contains(sys, "`"+g.Slug+"`") {
			t.Errorf("guide %q missing from the assembled guide index", g.Slug)
		}
	}
	buildSys := pack.Assemble(pack.AssembleInput{Stage: "build", Wave: "w", Selection: pack.Select(ps, "build")}).System
	if !strings.Contains(buildSys, "manage_guides action=list") {
		t.Error("BUILD dropped the guide index without leaving a way to reach the shelf")
	}
	if strings.Contains(sys, "{{contract:") || strings.Contains(sys, "{{GUIDE_INDEX}}") {
		t.Error("an unresolved {{contract:...}}/{{GUIDE_INDEX}} token survived assembly")
	}
	if strings.Contains(sys, "<guide slug=") {
		t.Error("a guide was inlined as a full body in the assembled system prompt; the index is recall-only (matched guides belong in the stage brief, not here)")
	}
}

func contractTokens(body string) []string {
	const open = "{{contract:"
	var out []string
	for {
		i := strings.Index(body, open)
		if i < 0 {
			break
		}
		rest := body[i+len(open):]
		j := strings.Index(rest, "}}")
		if j < 0 {
			break
		}
		out = append(out, strings.TrimSpace(rest[:j]))
		body = rest[j+2:]
	}
	return out
}
