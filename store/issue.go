package store

// Issue is the one validation-finding shape the whole platform speaks: content
// write gates, the manage_files check action, and every post-build validator
// raise and consume it. It lives in store (a leaf package) so both tools and
// engine can depend on it without an import cycle.

// Severity levels. Only error-severity findings block a build or drive a
// fix-pass; a warning is surfaced but never loops.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Issue is a single validation finding.
//
// Deterministic marks a finding that reproduces identically on a re-check; only
// these drive the validate -> fix loop, so a transient signal cannot trap the
// builder in an unresolvable cycle.
//
// Speculative marks a structural inference with no runtime witness. Such a
// finding must never carry rewrite-grade remediation as a hard error, so the
// build downgrades it to a non-looping warning (see NormalizeSpeculative).
type Issue struct {
	Severity      string   `json:"severity"`                // "error" | "warning"
	Category      string   `json:"category"`                // check/class discriminator, e.g. "js_syntax", "module_imports", "runtime"
	Page          string   `json:"page"`                    // affected page path (if applicable)
	File          string   `json:"file"`                    // affected file (if applicable)
	Message       string   `json:"message"`                 // human-readable description
	RelatedFiles  []string `json:"related_files,omitempty"` // siblings sharing the same root cause
	Deterministic bool     `json:"-"`                       // reproduces on a re-check; only these drive a fix-pass
	Speculative   bool     `json:"-"`                       // structural inference with no runtime witness; never a looping error
	Signature     string   `json:"-"`                       // optional stable discriminator folded into Fingerprint() when page+file is too coarse; carries normalized failing frames, never the volatile message
}

// NormalizeSpeculative enforces the speculative invariant in one place: any
// finding marked Speculative is forced to a non-looping warning, so a structural
// guess never drives a destructive fix-pass. Non-speculative findings are
// returned unchanged.
func NormalizeSpeculative(issues []Issue) []Issue {
	for i := range issues {
		if issues[i].Speculative {
			issues[i].Severity = SeverityWarning
			issues[i].Deterministic = false
		}
	}
	return issues
}

// Fingerprint is the stable identity the fix-loop keys on to tell a persisted
// finding from a new one: category + page + file plus the optional Signature,
// excluding the free-text Message, which varies run to run. Signature
// disambiguates runtime findings that otherwise collapse to one key, letting a
// newly-revealed site earn its own fix-pass instead of demoting as a survivor.
func (i Issue) Fingerprint() string {
	fp := i.Category + "\x00" + i.Page + "\x00" + i.File
	if i.Signature != "" {
		fp += "\x00" + i.Signature
	}
	return fp
}
