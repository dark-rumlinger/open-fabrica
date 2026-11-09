// Package pack is the unified knowledge-pack model and selection/rendering
// engine. ONE artifact type - a pack - covers stage prompts, guides, and
// recipes; a `role` field (stage | capability | fragment) decides how a pack is
// selected and where it renders. Select resolves every pack for a stage in one
// pass; Assemble renders them, plus the build intent seed, through one
// deterministic assembler that also transcludes canonical facts via the
// `{{contract:<id>}}` sentinel. Loading is from any fs.FS so the engine is
// testable without a database or the embedded tree.
package pack

import (
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"openfabrica/internal/frontmatter"
)

// Roles. A pack is a stage frame, a capability guide, or a fragment appended to
// a stage frame. (Golden-path recipes were folded away when the guide layer
// collapsed to an always-on index.)
const (
	RoleStage      = "stage"
	RoleCapability = "capability"
	RoleFragment   = "fragment"
)

// Pack is one parsed knowledge pack: a capability guide, a stage frame, or a
// fragment. All packs are read-only repo content (knowledge/); there is no DB
// overlay, so there are no id/enabled/source/ownership fields.
type Pack struct {
	Slug      string   `json:"slug"`
	Title     string   `json:"title"`
	Summary   string   `json:"summary"`
	Body      string   `json:"body"`
	Category  string   `json:"category"`
	Role      string   `json:"role"`
	Stage     string   `json:"stage,omitempty"`     // role:stage - which stage frame this fills
	Stages    []string `json:"stages,omitempty"`    // role:fragment - which stages this attaches to
	Tools     []string `json:"tools,omitempty"`     // role:capability - tools whose use implies this guide (drives build-time auto-prefetch)
	Libraries []string `json:"libraries,omitempty"` // role:capability - library specifiers (substrings) that imply this guide
	Requires  []string `json:"requires,omitempty"`  // role:capability - base guides this one builds on (selecting this guide pulls them in)
	Position  int      `json:"position"`

	// Decisions are the `<topic>/<choice>` pairs whose being LOCKED in a plan
	// implies this guide. The home of the decision -> guide link, for guides that
	// have no tool, library or structural signal to be inferred from.
	Decisions []string `json:"decisions,omitempty"`

	// ExclusiveWith names the guides that describe a RIVAL architecture for the
	// same thing (a blocky voxel world vs a smooth heightfield one). At most one
	// member of a group is ever selected: an extra additive guide only costs
	// tokens, but a second architecture derails the build. Declaring it on either
	// member is enough - the group is symmetrized when the map is derived.
	// ExclusiveDefault marks the member that wins when the rivals arrive with
	// equally strong evidence.
	ExclusiveWith    []string `json:"exclusive_with,omitempty"`
	ExclusiveDefault bool     `json:"exclusive_default,omitempty"`

	// Structural names the plan-SHAPE signals that imply this guide, for a
	// capability that no tool or library can express (a page split into sibling JS
	// modules). The engine decides whether a plan has the shape; the guides decide
	// which of them that shape needs, so more than one may claim the same signal.
	Structural []string `json:"structural,omitempty"`

	// RequiresAnyOf marks an ADD-ON guide that only earns its place alongside one of
	// the listed guides: game_feel layers juice on a rendering guide, so it must not
	// ride into a build that selected no game/canvas surface. Empty means the guide
	// stands alone. Unlike Requires (which pulls a base IN), this DROPS the guide
	// when none of its anchors is selected. An anchor of the form
	// `decision:<topic>/<choice words>` is met by a locked plan decision instead of a
	// guide (a vanilla-canvas game has no engine guide to anchor on).
	RequiresAnyOf []string `json:"requires_any_of,omitempty"`

	// PLAN-time routing metadata. The picker reads these off the guide index, so
	// a guide's "reach for me when / not when" rule lives with the guide instead
	// of in the picker's Go prose. UseWhen and NotWhen are one-line prose aimed at
	// the model; EvidenceGated marks a guide whose body is large enough that
	// include-when-unsure would tax every plan, so it is picked only on explicit
	// brief evidence.
	UseWhen       string `json:"use_when,omitempty"`
	NotWhen       string `json:"not_when,omitempty"`
	EvidenceGated bool   `json:"evidence_gated,omitempty"`

	// Core is the always-inlined slice of Body (before `<!-- extended -->`);
	// Sections are the recall-on-demand remainder. Derived from Body at parse
	// time - an unsplit pack has Core == Body and no Sections.
	Core     string    `json:"core,omitempty"`
	Sections []Section `json:"sections,omitempty"`
}

// CoreBody returns the always-inlined text: the CORE slice, or the whole Body
// for an unsplit pack.
func (p Pack) CoreBody() string {
	if strings.TrimSpace(p.Core) != "" {
		return p.Core
	}
	return p.Body
}

// Section returns the EXTENDED section with the given anchor (case-insensitive),
// or nil.
func (p Pack) Section(anchor string) *Section {
	anchor = strings.TrimSpace(anchor)
	for i := range p.Sections {
		if strings.EqualFold(p.Sections[i].Anchor, anchor) {
			return &p.Sections[i]
		}
	}
	return nil
}

// SectionIndex returns a compact [{anchor, when}] list of the pack's EXTENDED
// sections, for surfacing in a tool result. Nil when the pack is unsliced.
func (p Pack) SectionIndex() []map[string]string {
	if len(p.Sections) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(p.Sections))
	for _, s := range p.Sections {
		out = append(out, map[string]string{"anchor": s.Anchor, "when": sectionWhen(s)})
	}
	return out
}

// AnchorList returns the pack's section anchors comma-joined, for an error hint.
func (p Pack) AnchorList() string {
	as := make([]string, 0, len(p.Sections))
	for _, s := range p.Sections {
		as = append(as, s.Anchor)
	}
	return strings.Join(as, ", ")
}

// KnownFrontMatterKeys lists every front-matter key Parse understands. Parse skips
// a key it has no case for, so an unrecognised one is dropped in silence and reads
// as working routing metadata that never fires. Exported so a knowledge-tree guard
// can reject a key nothing consumes; TestKnownFrontMatterKeysAllParse keeps this
// list from claiming a key Parse does not actually handle.
//
// Must list exactly the `case` arms in Parse's switch below.
func KnownFrontMatterKeys() []string {
	return []string{
		"slug", "title", "summary", "category", "role",
		"stage", "stages", "position",
		"tools", "libraries", "requires", "requires_any_of",
		"decisions", "structural", "exclusive_with", "exclusive_default",
		"use_when", "not_when", "evidence_gated",
	}
}

// Parse builds one Pack from a pack-file byte body (front-matter + markdown). The
// body's Core/Sections are derived via splitCoreExtended.
func Parse(raw string) (Pack, error) {
	p := Pack{Category: "general", Role: RoleCapability}
	header, body, err := frontmatter.Split(raw)
	if err != nil {
		return p, err
	}

	for _, line := range strings.Split(header, "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "slug":
			p.Slug = val
		case "title":
			p.Title = val
		case "summary":
			p.Summary = val
		case "category":
			p.Category = val
		case "role":
			p.Role = val
		case "stage":
			p.Stage = val
		case "stages":
			p.Stages = frontmatter.SplitList(val)
		case "position":
			if n, err := strconv.Atoi(val); err == nil {
				p.Position = n
			}
		case "tools":
			p.Tools = frontmatter.SplitList(val)
		case "libraries":
			p.Libraries = frontmatter.SplitList(val)
		case "requires":
			p.Requires = frontmatter.SplitList(val)
		case "requires_any_of":
			p.RequiresAnyOf = frontmatter.SplitList(val)
		case "decisions":
			p.Decisions = frontmatter.SplitList(val)
		case "structural":
			p.Structural = frontmatter.SplitList(val)
		case "exclusive_with":
			p.ExclusiveWith = frontmatter.SplitList(val)
		case "exclusive_default":
			p.ExclusiveDefault = val == "true"
		case "use_when":
			p.UseWhen = val
		case "not_when":
			p.NotWhen = val
		case "evidence_gated":
			p.EvidenceGated = val == "true"
		}
	}

	p.Body = body
	p.Core, p.Sections = splitCoreExtended(body)

	if p.Slug == "" || p.Body == "" {
		return p, fmt.Errorf("pack needs slug and body")
	}
	if p.Title == "" {
		p.Title = p.Slug
	}
	return p, nil
}

// packBasenames are the recognised pack-file names: GUIDE.md for a capability
// guide and STAGE.md for a stage frame / fragment. A directory may hold exactly
// one.
var packBasenames = []string{"GUIDE.md", "STAGE.md"}

// isPackFile reports whether path's basename is a recognised pack file.
func isPackFile(path string) bool {
	for _, name := range packBasenames {
		if strings.HasSuffix(path, "/"+name) || strings.HasSuffix(path, "\\"+name) {
			return true
		}
	}
	return false
}

// ParseFS loads every pack under root in fsys. The layout is
// <root>/.../<slug>/{GUIDE,STAGE}.md. Deterministic: packs are returned sorted
// by (position, slug).
func ParseFS(fsys fs.FS, root string) ([]Pack, error) {
	var packMDs []string
	err := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if isPackFile(path) {
			packMDs = append(packMDs, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var out []Pack
	for _, mdPath := range packMDs {
		raw, err := fs.ReadFile(fsys, mdPath)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", mdPath, err)
		}
		p, err := Parse(string(raw))
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", mdPath, err)
		}
		out = append(out, p)
	}
	sortPacks(out)
	return out, nil
}

// sortPacks orders packs deterministically by (position, slug). Shared by
// ParseFS and Embedded (which merges two ParseFS passes).
func sortPacks(out []Pack) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].Slug < out[j].Slug
	})
}

// BySlug returns the pack with the given slug, or nil.
func BySlug(ps []Pack, slug string) *Pack {
	for i := range ps {
		if ps[i].Slug == slug {
			return &ps[i]
		}
	}
	return nil
}
