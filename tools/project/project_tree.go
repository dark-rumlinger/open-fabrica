package project

// project_tree, holistic structural view of every file-like artifact in the
// project. Aggregates pages, layouts, components, and assets into a single
// markdown tree where each file shows its outline (HTML structural tags, CSS
// selectors, JS function signatures).
//
// Designed as a single tool call so the LLM can answer "does X already exist?"
// without N round trips. Output is bounded, when the rendered markdown would
// exceed projectTreeBudget, we degrade to compact mode (paths + line counts)
// and note the truncation at the top.

import (
	"fmt"
	"sort"
	"strings"

	"openfabrica/analysis"
	"openfabrica/internal/dbh"
	"openfabrica/store"
)

// projectTreeBudget caps the rendered markdown size. Tuned so a typical
// project lands well under the cap with full outlines, while a sprawling one
// degrades cleanly. ~3 KB ~= 750 tokens for outline-mode; compact-mode prints
// roughly one short line per file so the cap is generous.
const projectTreeBudget = 8000

// ProjectTreeOpts controls what BuildProjectTree returns.
type ProjectTreeOpts struct {
	// Kinds filters the rendered set ("page", "layout", "component",
	// "file"). Empty means all kinds.
	Kinds []string
	// Path narrows the output to a single artifact across all kinds (mostly
	// useful when paired with Kinds to disambiguate).
	Path string
	// Compact skips per-file outlines and emits paths + line counts only.
	Compact bool
}

// BuildProjectTree assembles the tree. Cheap: one indexed read per kind plus
// the page-asset link table. Outlines reuse analysis.GenerateOutline so binary blobs
// are flagged but never decoded.
func BuildProjectTree(db dbh.Handle, opts ProjectTreeOpts) (string, error) {
	if db == nil {
		return "", fmt.Errorf("project_tree: nil db")
	}

	kinds := opts.Kinds
	if len(kinds) == 0 {
		kinds = []string{"page", "layout", "component", "file"}
	}

	items, err := store.ArtifactList(db, store.ArtifactListOpts{
		Kinds:       kinds,
		IncludeBody: true,
		Limit:       1000,
	})
	if err != nil {
		return "", err
	}
	if opts.Path != "" {
		filtered := items[:0]
		for _, a := range items {
			if a.Path == opts.Path {
				filtered = append(filtered, a)
			}
		}
		items = filtered
	}

	// Page-asset links: page_path -> []file_path, and reverse for
	// "linked-from" annotations on page-scope artifacts.
	pageAssets := make(map[string][]string)
	assetLinkedFrom := make(map[string][]string)
	if rows, qErr := db.Query(`SELECT page_path, file_path FROM of_page_assets ORDER BY page_path, position, file_path`); qErr == nil {
		for rows.Next() {
			var pp, ap string
			if rows.Scan(&pp, &ap) == nil {
				pageAssets[pp] = append(pageAssets[pp], ap)
				assetLinkedFrom[ap] = append(assetLinkedFrom[ap], pp)
			}
		}
		_ = rows.Err() // best-effort annotations; a partial link map is fine
		rows.Close()
	}

	byKind := make(map[string][]*store.Artifact)
	for _, a := range items {
		byKind[a.Kind] = append(byKind[a.Kind], a)
	}
	for _, list := range byKind {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	}

	// First render attempt: full outlines. If oversized, retry compact.
	rendered := renderProjectTree(byKind, pageAssets, assetLinkedFrom, opts.Compact)
	if !opts.Compact && len(rendered) > projectTreeBudget {
		rendered = "_(project too large for full outline view, falling back to compact mode; call `manage_files action=outline kind=… file_path=…` to inspect a single file)_\n\n" +
			renderProjectTree(byKind, pageAssets, assetLinkedFrom, true)
	}
	return rendered, nil
}

func renderProjectTree(byKind map[string][]*store.Artifact, pageAssets, assetLinkedFrom map[string][]string, compact bool) string {
	var b strings.Builder
	b.WriteString("# Project Tree\n")

	renderSection := func(label, kind string) {
		list := byKind[kind]
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s (%d)\n", label, len(list))
		for _, a := range list {
			renderArtifact(&b, a, kind, pageAssets, assetLinkedFrom, compact)
		}
	}

	renderSection("Pages", "page")
	renderSection("Layouts", "layout")
	renderSection("Components", "component")

	// Artifacts: split into page-scope and global for readability.
	if assets := byKind["file"]; len(assets) > 0 {
		var pageScope, global []*store.Artifact
		for _, a := range assets {
			if a.Scope == "page" {
				pageScope = append(pageScope, a)
			} else {
				global = append(global, a)
			}
		}
		if len(pageScope) > 0 {
			fmt.Fprintf(&b, "\n## Page-scope assets (%d)\n", len(pageScope))
			for _, a := range pageScope {
				renderArtifact(&b, a, "file", pageAssets, assetLinkedFrom, compact)
			}
		}
		if len(global) > 0 {
			fmt.Fprintf(&b, "\n## Global assets (%d)\n", len(global))
			for _, a := range global {
				renderArtifact(&b, a, "file", pageAssets, assetLinkedFrom, compact)
			}
		}
	}

	if b.Len() == len("# Project Tree\n") {
		b.WriteString("\n_(empty project, no files yet)_\n")
	}
	return b.String()
}

func renderArtifact(b *strings.Builder, a *store.Artifact, kind string, pageAssets, assetLinkedFrom map[string][]string, compact bool) {
	header := "### " + a.Path
	if title := titleFromMetadata(a); title != "" {
		header += ", " + title
	}
	b.WriteString(header)
	b.WriteByte('\n')

	// Annotations: scope, layout (for pages), linked assets, linked-from (for assets).
	if kind == "file" && a.Scope == "page" {
		if linked := assetLinkedFrom[a.Path]; len(linked) > 0 {
			fmt.Fprintf(b, "  linked: %s\n", strings.Join(linked, ", "))
		} else {
			b.WriteString("  linked: (none, UNLINKED)\n")
		}
	}
	if kind == "page" {
		if layout := layoutFromMetadata(a); layout != "" {
			fmt.Fprintf(b, "  layout: %s\n", layout)
		}
		if assets := pageAssets[a.Path]; len(assets) > 0 {
			fmt.Fprintf(b, "  assets: %s\n", strings.Join(assets, ", "))
		}
	}

	// Body rendering, outline (full mode) or single line (compact).
	body := a.Body
	if a.Storage == store.StorageDisk || body == "" {
		fmt.Fprintf(b, "  (binary or disk-stored, %d bytes)\n", a.Size)
		return
	}

	if compact {
		lineCount := strings.Count(body, "\n") + 1
		fmt.Fprintf(b, "  (%s, %d lines)\n", outlineFileType(a), lineCount)
		return
	}

	outline := analysis.GenerateOutline(body, outlineFileType(a))
	// Indent the outline block by two spaces so it nests under the artifact
	// header in markdown readers without disturbing line numbers.
	for _, line := range strings.Split(strings.TrimRight(outline, "\n"), "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

func titleFromMetadata(a *store.Artifact) string {
	if a.Metadata == nil {
		return ""
	}
	if t, _ := a.Metadata["title"].(string); t != "" {
		return t
	}
	return ""
}

func layoutFromMetadata(a *store.Artifact) string {
	if a.Metadata == nil {
		return ""
	}
	if l, _ := a.Metadata["layout"].(string); l != "" {
		return l
	}
	return ""
}

// outlineFileType picks the dialect for analysis.GenerateOutline, mirroring the
// logic in analysis.FileTypeForOutline (artifacts.go) but operating on an store.Artifact
// directly. Pages, layouts, and components are always HTML.
func outlineFileType(a *store.Artifact) string {
	switch a.Kind {
	case "page", "layout", "component":
		return "html"
	}
	return analysis.FileTypeForOutline(a.Path, a.ContentType)
}
