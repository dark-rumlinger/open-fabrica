package engine

// Selection of the validator findings that ship as known issues. Identity is
// store.Issue.Fingerprint(), never the free-text message, which varies run to run and
// would make the same defect look new.

import (
	"sort"

	"openfabrica/store"
)

// FixableErrors selects the issues that count as defects - error severity AND
// deterministic - sorted by fingerprint so the report is stable. A non-deterministic or
// warning-severity finding is surfaced elsewhere and never counts.
func FixableErrors(issues []Issue) []Issue {
	out := make([]Issue, 0, len(issues))
	for _, is := range issues {
		if is.Severity == store.SeverityError && is.Deterministic {
			out = append(out, is)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Fingerprint() < out[j].Fingerprint() })
	return out
}
