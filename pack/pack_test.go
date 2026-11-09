package pack

import (
	"strings"
	"testing"
	"testing/fstest"
)

// fixtureFS is a minimal packs/ tree exercising the loader and assembler:
// a stage frame with a sentinel + a transcluded contract, and capability guides
// (one with an extended section, one with a transcluded contract). Every key here
// is one Parse understands - an unrecognised key is dropped in silence, so the
// knowledge tree bans them (TestGuideFrontMatterKeysAreKnown) and the fixture
// holds to the same rule rather than modelling front-matter that does nothing.
func fixtureFS() fstest.MapFS {
	return fstest.MapFS{
		"packs/_contracts/note.md": {Data: []byte("ignored - contracts are their own package")},

		"packs/stage/build/STAGE.md": {Data: []byte(`---
slug: build
role: stage
stage: build
title: Build the project.
---
Build everything. Columns: {{ALLOWED}}.

{{contract:routing}}
`)},

		"packs/capability/api-response-contract/GUIDE.md": {Data: []byte(`---
slug: api-response-contract
role: capability
position: 25
---
# API response shapes

{{contract:api-response}}
`)},

		"packs/capability/e2ee/GUIDE.md": {Data: []byte(`---
slug: e2ee
role: capability
requires: [realtime-signaling]
position: 23
---
# E2EE core

Use OFE2EE; never hand-roll crypto.

<!-- extended -->

## Group rooms
> when: 3+ participants

Use mode:'group'.
`)},

		"packs/capability/realtime-signaling/GUIDE.md": {Data: []byte(`---
slug: realtime-signaling
role: capability
position: 21
---
# Realtime relay

System frames: welcome/join/leave.
`)},

		"packs/capability/auth-contract/GUIDE.md": {Data: []byte(`---
slug: auth-contract
role: capability
summary: Auth endpoint + bootstrap.
position: 20
---
# Auth

Token key is auth_token.
`)},
	}
}

func loadFixture(t *testing.T) []Pack {
	t.Helper()
	ps, err := ParseFS(fixtureFS(), "packs")
	if err != nil {
		t.Fatalf("ParseFS: %v", err)
	}
	return ps
}

// TestKnownFrontMatterKeysAllParse closes the loop on the exported allowlist: a key
// listed there but missing a `case` in Parse would let a knowledge-tree guard bless
// front-matter that is still silently dropped. Parsing a doc that sets every known
// key and asserting each landed catches that, so the list cannot over-promise.
func TestKnownFrontMatterKeysAllParse(t *testing.T) {
	raw := `---
slug: probe
title: Probe
summary: A probe pack.
category: probes
role: capability
stage: build
stages: [build, live]
position: 7
tools: [manage_probe]
libraries: [probelib]
requires: [base_probe]
requires_any_of: [anchor_probe]
decisions: [probe topic/probe choice]
structural: [probe_signal]
exclusive_with: [rival_probe]
exclusive_default: true
use_when: A probe is needed.
not_when: No probe is needed.
evidence_gated: true
---
Body.
`
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	set := map[string]bool{}
	for _, k := range KnownFrontMatterKeys() {
		set[k] = true
	}
	landed := map[string]bool{
		"slug": p.Slug != "", "title": p.Title != "", "summary": p.Summary != "",
		"category": p.Category == "probes", "role": p.Role == RoleCapability,
		"stage": p.Stage != "", "stages": len(p.Stages) == 2,
		"position": p.Position == 7, "tools": len(p.Tools) == 1, "libraries": len(p.Libraries) == 1,
		"requires": len(p.Requires) == 1, "requires_any_of": len(p.RequiresAnyOf) == 1,
		"decisions":  len(p.Decisions) == 1,
		"structural": len(p.Structural) == 1, "exclusive_with": len(p.ExclusiveWith) == 1,
		"exclusive_default": p.ExclusiveDefault, "use_when": p.UseWhen != "",
		"not_when": p.NotWhen != "", "evidence_gated": p.EvidenceGated,
	}
	for k := range set {
		ok, checked := landed[k]
		if !checked {
			t.Errorf("known key %q has no assertion here - add one so the allowlist stays honest", k)
			continue
		}
		if !ok {
			t.Errorf("known key %q is in KnownFrontMatterKeys but Parse dropped it", k)
		}
	}
	for k := range landed {
		if !set[k] {
			t.Errorf("key %q parses but is missing from KnownFrontMatterKeys - a guard would reject valid front-matter", k)
		}
	}
}

func TestParseFS(t *testing.T) {
	ps := loadFixture(t)
	// 5 packs (1 stage, 4 capability); _contracts ignored.
	if len(ps) != 5 {
		t.Fatalf("loaded %d packs, want 5: %v", len(ps), slugs(ps))
	}
	// Sorted by (position, slug): auth(20), realtime(21), e2ee(23), api(25),
	// then the position-0 pack (build).
	e2 := BySlug(ps, "e2ee")
	if e2 == nil {
		t.Fatal("e2ee not parsed")
	}
	if len(e2.Sections) != 1 || e2.Sections[0].Anchor != "group-rooms" {
		t.Fatalf("e2ee extended section not parsed: %+v", e2.Sections)
	}
	if e2.Sections[0].When != "3+ participants" {
		t.Errorf("section when = %q", e2.Sections[0].When)
	}
}

func TestSelectStageAndGuides(t *testing.T) {
	ps := loadFixture(t)
	sel := Select(ps, "build")

	if sel.Stage == nil || sel.Stage.Slug != "build" {
		t.Fatalf("stage frame = %v", sel.Stage)
	}
	// Every capability guide is always in the guide set - no trigger/requires
	// gating, no inlining.
	got := map[string]bool{}
	for _, g := range sel.Guides {
		got[g.Slug] = true
	}
	for _, want := range []string{"e2ee", "realtime-signaling", "api-response-contract", "auth-contract"} {
		if !got[want] {
			t.Errorf("guide %q missing from the always-on set; got %v", want, slugs(sel.Guides))
		}
	}
}

func TestAssembleTranscludesAndIsDeterministic(t *testing.T) {
	ps := loadFixture(t)
	in := AssembleInput{
		Stage: "build", Wave: "test-wave", Selection: Select(ps, "build"),
		Sentinels: map[string]string{"ALLOWED": "text|integer"},
	}
	a := Assemble(in)

	// System: wave tag, stage body with sentinel replaced + routing contract
	// transcluded, and no surviving tokens.
	if !strings.Contains(a.System, "<!-- test-wave -->") {
		t.Error("system missing wave tag")
	}
	if !strings.Contains(a.System, "Columns: text|integer.") {
		t.Error("sentinel not substituted in stage body")
	}
	if strings.Contains(a.System, "{{contract:routing}}") || strings.Contains(a.System, guideIndexToken) {
		t.Error("a {{contract}}/{{GUIDE_INDEX}} token survived assembly")
	}
	if !strings.Contains(a.System, "Routing contract") {
		t.Error("routing contract body not present in system")
	}
	// BUILD's guides arrive prefetched in its seed, so it carries a pointer at
	// the shelf rather than the per-guide list.
	if !strings.Contains(a.System, "## Guide index") {
		t.Error("system missing the guide index section")
	}
	if !strings.Contains(a.System, "manage_guides action=list") {
		t.Error("BUILD's guide index pointer is missing")
	}
	// Guides appear in the always-on index (slug + summary), NOT inlined as bodies.
	planIn := in
	planIn.Stage = "plan"
	planSys := Assemble(planIn).System
	for _, want := range []string{"## Guide index", "`auth-contract`", "Auth endpoint + bootstrap."} {
		if !strings.Contains(planSys, want) {
			t.Errorf("plan system missing %q", want)
		}
	}
	if strings.Contains(a.System, "<guide slug=") || strings.Contains(planSys, "<guide slug=") {
		t.Error("guides must not be inlined as full bodies")
	}
	// A slug named as capability-dead is dropped from the index entirely.
	hiddenIn := planIn
	hiddenIn.HiddenGuides = map[string]bool{"auth-contract": true}
	if strings.Contains(Assemble(hiddenIn).System, "`auth-contract`") {
		t.Error("a hidden guide still appears in the index")
	}

	// Determinism: same inputs -> byte-identical output.
	if b := Assemble(in); a.System != b.System {
		t.Error("Assemble not deterministic")
	}
}

func TestSubstituteContractsUnknownLeftVerbatim(t *testing.T) {
	got := substituteContracts("a {{contract:api-response}} b {{contract:nope}} c")
	if strings.Contains(got, "{{contract:api-response}}") {
		t.Error("known contract not substituted")
	}
	if !strings.Contains(got, "{{contract:nope}}") {
		t.Error("unknown contract should be left verbatim")
	}
}

func slugs(ps []Pack) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Slug
	}
	return out
}
