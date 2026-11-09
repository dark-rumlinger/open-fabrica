package content

// Save-time advisory for a scroll or entrance reveal that starts invisible. A rule such
// as `.reveal{opacity:0}` that only `.reveal.in{opacity:1}` undoes leaves the content
// blank whenever the JS adding `.in` is deferred, throws, or never fires (content already
// in view when the observer attaches). It parses clean, boots green and passes the
// page-load probe, so nothing catches it before a visitor does. The post-build
// opacity_reveal validator reports it too late to fix; this surfaces it on the write that
// introduces the rule, while the stylesheet is in front of the model. Non-blocking: only
// the cross-file picture (a JS observer that adds the state class) decides a real defect,
// and a save cannot see it.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	revealCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	// A reduced-motion block is stripped first: its reset rescues one audience, not the
	// default render.
	revealReducedMotionRe = regexp.MustCompile(`(?is)@media[^{]*prefers-reduced-motion[^{]*\{(?:[^{}]*\{[^{}]*\})*[^{}]*\}`)
	// An innermost `selector{declarations}` rule (nested at-rule bodies yield their rules).
	revealRuleRe       = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	revealLoneClassRe  = regexp.MustCompile(`^\.([A-Za-z_][\w-]*)$`)
	revealCompoundRe   = regexp.MustCompile(`^\.([A-Za-z_][\w-]*)\.([A-Za-z_][\w-]*)$`)
	revealZeroRe       = regexp.MustCompile(`(?i)(?:^|[;\s])opacity\s*:\s*0(?:\.0+)?\s*(?:!important\s*)?(?:;|$)`)
	revealVisibleRe    = regexp.MustCompile(`(?i)(?:^|[;\s])opacity\s*:\s*(?:0*\.0*[1-9][0-9]*|[1-9][0-9]*(?:\.[0-9]+)?)`)
	revealClassNameRe  = regexp.MustCompile(`(?i)reveal|fade|rise|appear|enter|in-?view|scroll|animate|anim|slide|stagger`)
	revealNoteMaxNames = 4
)

// strandedRevealClasses returns the reveal-style classes css hides at their base
// (`.name{opacity:0}`), shows only through a compound state rule (`.name.state{opacity:1}`)
// and never resets to visible on their own. Names that do not read as a reveal (a toast,
// a modal) are ignored: those are toggled by events, not scroll, and are legitimately
// hidden by default.
func strandedRevealClasses(css string) []string {
	css = revealReducedMotionRe.ReplaceAllString(revealCommentRe.ReplaceAllString(css, ""), "")
	hidden := map[string]bool{}
	visible := map[string]bool{}
	revealed := map[string]bool{} // class -> some compound rule shows it
	for _, m := range revealRuleRe.FindAllStringSubmatch(css, -1) {
		decl := m[2]
		for _, sel := range strings.Split(m[1], ",") {
			sel = strings.TrimSpace(sel)
			if lone := revealLoneClassRe.FindStringSubmatch(sel); lone != nil {
				switch {
				case revealZeroRe.MatchString(decl):
					hidden[lone[1]] = true
				case revealVisibleRe.MatchString(decl):
					visible[lone[1]] = true
				}
				continue
			}
			if c := revealCompoundRe.FindStringSubmatch(sel); c != nil && revealVisibleRe.MatchString(decl) && !revealZeroRe.MatchString(decl) {
				revealed[c[1]] = true
				revealed[c[2]] = true
			}
		}
	}
	var out []string
	for class := range hidden {
		if visible[class] || !revealed[class] || !revealClassNameRe.MatchString(class) {
			continue
		}
		out = append(out, class)
	}
	sort.Strings(out)
	return out
}

// revealNote returns the advisory for a stylesheet save or patch that INTRODUCES a
// stranded reveal class, or "" when the file is not CSS or the classes were already
// there before the edit (the note was shown once; repeating it on every edit of the
// stylesheet is noise).
func revealNote(path, oldBody, newBody string) string {
	if !strings.HasSuffix(strings.ToLower(path), ".css") {
		return ""
	}
	fresh := strandedRevealClasses(newBody)
	if len(fresh) == 0 {
		return ""
	}
	if oldBody != "" {
		prior := map[string]bool{}
		for _, c := range strandedRevealClasses(oldBody) {
			prior[c] = true
		}
		kept := fresh[:0:0]
		for _, c := range fresh {
			if !prior[c] {
				kept = append(kept, c)
			}
		}
		fresh = kept
	}
	if len(fresh) == 0 {
		return ""
	}
	names := make([]string, 0, revealNoteMaxNames)
	for i, c := range fresh {
		if i == revealNoteMaxNames {
			break
		}
		names = append(names, "`."+c+"`")
	}
	class := fresh[0]
	return fmt.Sprintf(
		"reveal: %s start at opacity:0 and only a state class turns them visible, so if the JS that adds it is deferred behind an await, throws, "+
			"or never fires (content already in view when the observer attaches), that content stays blank - and no probe scrolls, so nothing else catches it. "+
			"Fix it now, in the next turn: make `.%s` visible by default and have the module add a hiding class (`.%s.prep{opacity:0}`) only to nodes below the fold, "+
			"then reveal with `.%s.prep.in{opacity:1}`. The client_view_toggle guide's entrance-reveal section has the exact pattern.",
		strings.Join(names, ", "), class, class, class)
}
