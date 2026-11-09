package content

// Save-time CSS contract advisory. The project_state "Design system (shared CSS)"
// section already keeps the defined class vocabulary in front of the model every
// turn; this is its enforcement complement - a NON-BLOCKING nudge when a just-
// saved body renders an element whose class no stylesheet defines (the element
// that ships unstyled and that no runtime probe or JS gate can see). It never
// fails a save: styling is not a correctness gate, dynamically-composed class
// names legitimately exist, and a false nudge must never bounce a write.

import (
	"fmt"
	"sort"
	"strings"

	"openfabrica/analysis"
	"openfabrica/internal/dbh"
	"openfabrica/store"
)

const (
	// styleContractMinDefined is the floor of project-wide defined classes below
	// which the check stays silent: a project with only a handful has no real
	// design system (utility-framework or pre-CSS), so "undefined" would be noise.
	styleContractMinDefined = 5
	// styleContractMaxReport caps how many undefined classes one save reports, so a
	// body that forgot the stylesheet yields a readable nudge, not a wall of names.
	styleContractMaxReport = 8
)

// bodyRendersMarkup reports whether a saved artifact of this kind/path renders
// markup whose classes should match the design system. Pages, layouts, and
// components are markup; a kind=file qualifies only as a JS module (its template
// strings), never a .css/.svg/.json asset.
func bodyRendersMarkup(kind, path string) bool {
	switch kind {
	case "page", "layout", "component":
		return true
	case "file":
		return isJSModulePath(path)
	}
	return false
}

// projectDefinedClasses is the union of class names any project stylesheet
// defines - every *.css file plus the inline <style> preludes of layouts/pages/
// components, with compound selectors fully expanded (ExtractStyleSurface reads
// every `.name` in a prelude, so `.act.liked` defines both). Deliberately project-
// wide: a class defined for one page counts as defined everywhere, so the advisory
// only ever fires on a name NO stylesheet anywhere defines - the high-confidence
// "renders unstyled" case, never a cross-page style that merely lives elsewhere.
func projectDefinedClasses(db dbh.Handle) map[string]bool {
	defined := map[string]bool{}
	collect := func(arts []*store.Artifact) {
		for _, a := range arts {
			if a == nil || a.Storage != store.StorageInline || a.Body == "" {
				continue
			}
			for _, c := range analysis.ExtractStyleSurface(a.Body).Classes {
				defined[c] = true
			}
		}
	}
	// Standalone stylesheets. PathContains narrows the scan; the suffix check below
	// (in bodyRendersMarkup's mirror) is unnecessary here because ExtractStyleSurface
	// on a non-CSS body simply yields no selector preludes.
	cssFiles, _ := store.ArtifactList(db, store.ArtifactListOpts{Kind: "file", PathContains: ".css", IncludeBody: true})
	collect(cssFiles)
	// Inline <style> carried by markup artifacts (a design system authored in the
	// layout rather than a file).
	inlineStyled, _ := store.ArtifactList(db, store.ArtifactListOpts{Kinds: []string{"layout", "page", "component"}, IncludeBody: true})
	collect(inlineStyled)
	return defined
}

// undefinedClassesUsed returns the class names a saved body references in markup
// that NO project stylesheet defines. Empty (no advisory) unless the project has a
// real design system (>= styleContractMinDefined defined classes) and the body
// actually references classes - both keep the check silent for utility-framework
// or logic-only files.
func undefinedClassesUsed(db dbh.Handle, kind, path, body string) []string {
	if db == nil || !bodyRendersMarkup(kind, path) {
		return nil
	}
	groups := analysis.ClassGroupsUsed(body)
	if len(groups) == 0 {
		return nil
	}
	defined := projectDefinedClasses(db)
	if len(defined) < styleContractMinDefined {
		return nil
	}
	return undefinedClassGroups(groups, defined)
}

// undefinedClassGroups is the per-body judging kernel shared by the save-path
// advisory (undefinedClassesUsed) and the project-wide check (styleStaticIssues):
// given the pre-resolved set of defined class names, it returns the referenced
// classes that render unstyled. Hoisting `defined` out of the loop lets the whole-
// project pass resolve it once instead of per file. Judge per element - an element
// is unstyled only when NONE of its classes has a rule; a group with any defined
// class is styled (its undefined siblings are behavior-only hooks, `a-like` beside
// `act`), so the whole group is skipped. Result is deduplicated and sorted.
func undefinedClassGroups(groups [][]string, defined map[string]bool) []string {
	var missing []string
	seen := map[string]bool{}
	for _, g := range groups {
		styled := false
		for _, c := range g {
			if defined[c] {
				styled = true
				break
			}
		}
		if styled {
			continue
		}
		for _, c := range g {
			if !seen[c] {
				seen[c] = true
				missing = append(missing, c)
			}
		}
	}
	sort.Strings(missing)
	return missing
}

// styleContractNote renders the advisory for a save whose markup uses classes no
// stylesheet defines, or "" when there is nothing to report.
func styleContractNote(missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	shown, suffix := missing, ""
	if len(shown) > styleContractMaxReport {
		shown = shown[:styleContractMaxReport]
		suffix = fmt.Sprintf(", +%d more", len(missing)-styleContractMaxReport)
	}
	return fmt.Sprintf(
		"style: %d class(es) used here have no matching CSS rule, so those elements render unstyled: %s%s. "+
			"Add the rule(s) to the global stylesheet, or reuse a class from the design system (shown in project state). "+
			"Advisory only - the save succeeded; ignore for a class you style dynamically or inline.",
		len(missing), strings.Join(shown, ", "), suffix)
}
