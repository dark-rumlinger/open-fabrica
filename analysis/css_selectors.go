package analysis

import (
	"regexp"
	"strings"
)

// StyleSurface is the design-system surface of one CSS body: custom properties,
// class selectors, and @keyframes/animation names. Custom properties also carry
// their declared value, so a page built after a context rollover composes from
// the real token values instead of re-inventing them. Class and keyframe bodies
// are still never carried.
type StyleSurface struct {
	Vars      []string          // custom properties, leading -- kept: "--color-bg"
	VarValues map[string]string // declared value per custom property, keyed by the Vars entry
	Classes   []string          // class selector names, leading . dropped: "btn", "card"
	Keyframes []string          // @keyframes / animation names: "fade-in"
	Truncated bool              // a per-list cap or the framework-dump guard fired
}

const (
	styleMaxVars      = 80
	styleMaxClasses   = 120
	styleMaxKeyframes = 24
	// Longest token value carried. Enough for a colour, a length, a font stack, or
	// a short easing curve; a long gradient is clipped rather than dropped.
	styleMaxVarValueChars = 64
	// A body over this size, or with more raw class matches than the ceiling, is
	// treated as a framework/utility dump: only vars, keyframes, and the first
	// styleMaxClasses classes are kept, so utilities can never expand the contract.
	styleMaxBodyBytes       = 200 * 1024
	styleMaxRawClassMatches = 400
)

var (
	// A custom-property DECLARATION (`--name:`), not a `var(--name)` use: the
	// leading boundary excludes `(`, and the trailing `:` requires a declaration.
	cssVarDeclRe = regexp.MustCompile(`(?:^|[;{\s])(--[A-Za-z0-9_-]+)\s*:`)
	// A class-selector token: a `.` followed by a CSS identifier.
	cssClassRe = regexp.MustCompile(`\.(-?[A-Za-z_][A-Za-z0-9_-]*)`)
	// An @keyframes name; vendor prefixes tolerated.
	cssKeyframesRe = regexp.MustCompile(`@(?:-webkit-|-moz-|-o-|-ms-)?keyframes\s+(-?[A-Za-z_][A-Za-z0-9_-]*)`)
)

// cssDeclValue returns the declaration value starting at from (just past the
// `:`), ending at the first `;` or `}` in the masked body so a comment or string
// cannot terminate it early. Whitespace is collapsed and the result is clipped to
// styleMaxVarValueChars.
func cssDeclValue(body, masked string, from int) string {
	if from < 0 || from > len(masked) || len(body) != len(masked) {
		return ""
	}
	end := strings.IndexAny(masked[from:], ";}")
	if end < 0 {
		end = len(masked) - from
	}
	v := strings.Join(strings.Fields(body[from:from+end]), " ")
	if len(v) > styleMaxVarValueChars {
		v = strings.TrimSpace(v[:styleMaxVarValueChars]) + "..."
	}
	return v
}

// ExtractStyleSurface returns the names-only design-system surface of a CSS body.
// It scans a comment/string-masked copy (maskCSS) and reads class names only from
// selector preludes; names are source-ordered, deduplicated, and capped (Truncated).
func ExtractStyleSurface(body string) StyleSurface {
	var ss StyleSurface
	if strings.TrimSpace(body) == "" {
		return ss
	}
	masked := maskCSS(body)

	// Custom properties (declarations only), scanned over the whole masked body.
	// The value is sliced from the ORIGINAL body at the masked offsets, so a
	// quoted font stack survives while comments still cannot end a declaration.
	varSeen := map[string]bool{}
	for _, m := range cssVarDeclRe.FindAllStringSubmatchIndex(masked, -1) {
		name := masked[m[2]:m[3]]
		if varSeen[name] {
			continue
		}
		varSeen[name] = true
		if len(ss.Vars) >= styleMaxVars {
			ss.Truncated = true
			break
		}
		ss.Vars = append(ss.Vars, name)
		if v := cssDeclValue(body, masked, m[1]); v != "" {
			if ss.VarValues == nil {
				ss.VarValues = map[string]string{}
			}
			ss.VarValues[name] = v
		}
	}

	// @keyframes names.
	kfSeen := map[string]bool{}
	for _, m := range cssKeyframesRe.FindAllStringSubmatch(masked, -1) {
		name := m[1]
		if kfSeen[name] {
			continue
		}
		kfSeen[name] = true
		if len(ss.Keyframes) >= styleMaxKeyframes {
			ss.Truncated = true
			break
		}
		ss.Keyframes = append(ss.Keyframes, name)
	}

	// Classes are read only from selector preludes (the segment before a `{`), so
	// a declaration value like `background:url(.x)` never contributes a class.
	framework := len(body) > styleMaxBodyBytes
	classSeen := map[string]bool{}
	rawClasses := 0
	segStart := 0
	scanPrelude := func(seg string) {
		for _, m := range cssClassRe.FindAllStringSubmatch(seg, -1) {
			rawClasses++
			name := m[1]
			if classSeen[name] {
				continue
			}
			classSeen[name] = true
			if len(ss.Classes) >= styleMaxClasses {
				ss.Truncated = true
				continue
			}
			ss.Classes = append(ss.Classes, name)
		}
	}
	for i := 0; i < len(masked); i++ {
		switch masked[i] {
		case '{':
			scanPrelude(masked[segStart:i])
			segStart = i + 1
			// On a framework dump, stop once the class cap is full and the raw
			// count clearly confirms it, so a multi-MB utility build is not
			// fully walked.
			if framework && len(ss.Classes) >= styleMaxClasses && rawClasses > styleMaxRawClassMatches {
				i = len(masked)
			}
		case '}', ';':
			segStart = i + 1
		}
	}
	if framework || rawClasses > styleMaxRawClassMatches {
		ss.Truncated = true
	}
	return ss
}
