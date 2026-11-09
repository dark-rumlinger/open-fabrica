package validators

// The static link-readiness validator: catches a page-scope module the browser
// never loads because no linked entry imports it. An orphaned module is silent
// at runtime - never fetched, never run - so a build marks every task done while
// the feature never executes. Registers the store.UnlinkedPageArtifacts signal
// (advisory in project_state) as a hard gate. Static, so it runs even when the
// runtime probe self-skips.

import (
	"context"
	"database/sql"
	"strings"

	"openfabrica/store"
)

type linkReadinessValidator struct{}

func (linkReadinessValidator) Name() string { return "link_readiness" }

// Validate flags every page-scope artifact that no linked entry reaches. baseURL
// and hostHeader are unused: the check is pure static analysis over the project
// DB, mirroring the resolver the runtime server uses, so a file flagged here is
// one the browser genuinely never loads.
func (linkReadinessValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	res := store.UnlinkedPageArtifactsClassified(pdb)
	if len(res.Orphans) == 0 {
		return nil
	}

	// Split JS from CSS: the remedy differs. A JS module is wired by importing
	// it from a linked entry (the entry imports the leaves); a stylesheet can
	// only be attached directly, because an ES module cannot pull in CSS.
	var js, css []string
	for _, p := range res.Orphans {
		if lp := strings.ToLower(p); strings.HasSuffix(lp, ".css") {
			css = append(css, p)
		} else {
			js = append(js, p)
		}
	}

	issues := make([]Issue, 0, len(res.Orphans))
	for _, p := range js {
		// When an opaque dynamic import (import(variable) / import(`tmpl`)) lives
		// in the reachable closure, the static graph cannot prove this module
		// dead - it may be loaded at runtime. Emit it as a speculative finding:
		// NormalizeSpeculative downgrades it to a non-looping warning, and the
		// message drops the destructive "delete it" guidance so a false orphan
		// flag can never drive the model to tear out a working dynamic loader.
		if res.Uncertain {
			issues = append(issues, Issue{
				Severity:    "warning",
				Category:    "orphaned_module",
				File:        p,
				Speculative: true,
				Message: "Page module `" + p + "` is reached by no STATIC import, so the static graph cannot confirm it loads. The page's entry graph " +
					"contains a dynamic `import()` with a computed specifier, which may load this module at runtime - the static analysis cannot see " +
					"where that resolves. If this module is loaded dynamically, this is expected and no change is needed. If it is genuinely orphaned, " +
					"either add a static `import` for it to the linked entry, or remove it.",
				RelatedFiles: siblings(js, p),
			})
			continue
		}
		issues = append(issues, Issue{
			Severity:      "error",
			Category:      "orphaned_module",
			File:          p,
			Deterministic: true,
			Message: "Page module `" + p + "` is reached by no linked entry, so the browser never loads it and its code never runs - the feature it " +
				"implements is dead even though the file exists. Wire it into the page's import graph: add a static `import` for it to the linked entry " +
				"module (the entry imports the leaves; leaves never import back from the entry, which would make the graph cyclic and boot nothing). If " +
				"it is itself a page's entry, attach it with `manage_files action=attach`. Delete it only if it is genuinely unused.",
			RelatedFiles: siblings(js, p),
		})
	}
	for _, p := range css {
		issues = append(issues, Issue{
			Severity:      "error",
			Category:      "unlinked_stylesheet",
			File:          p,
			Deterministic: true,
			Message: "Stylesheet `" + p + "` is linked to no page, so its rules never load (an ES module cannot import CSS). Attach it directly to the " +
				"page that needs it: `manage_files action=attach page_path=/<route> file_path=" + p + "`. Delete it only if it is genuinely unused.",
			RelatedFiles: siblings(css, p),
		})
	}
	return issues
}

// siblings returns the members of group other than self, so a clustered fix-pass
// can patch the whole orphan set in one turn instead of leaking duplicates.
func siblings(group []string, self string) []string {
	if len(group) <= 1 {
		return nil
	}
	out := make([]string, 0, len(group)-1)
	for _, p := range group {
		if p != self {
			out = append(out, p)
		}
	}
	return out
}

func init() {
	RegisterValidator(linkReadinessValidator{})
}
