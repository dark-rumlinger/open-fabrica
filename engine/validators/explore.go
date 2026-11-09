package validators

// The explore probe as a validator: clicks every visible control on every page,
// checks the layout at desktop and phone sizes, and reports what did nothing,
// what threw, what cannot be tapped, and what scrolls sideways (see
// probe.RunExplore). Its clicks write data, so it runs under a snapshot that is
// restored afterwards, and it is sequential: it runs after the parallel batch so
// its snapshot never overlaps the flow validator's. Without a snapshot it does not
// click at all.

import (
	"context"
	"database/sql"

	"openfabrica/internal/flowsnap"
	"openfabrica/probe"
	"openfabrica/store"
)

type exploreValidator struct{}

func (exploreValidator) Name() string { return "explore" }

// Sequential marks this validator as one that must not overlap another.
func (exploreValidator) Sequential() {}

func (exploreValidator) Validate(ctx context.Context, pdb *sql.DB, baseURL, hostHeader string) []Issue {
	var res probe.RuntimeResult
	skip, restoreErr := flowsnap.Around(ctx, func() {
		res = probe.RunExplore(ctx, pdb, baseURL, hostHeader)
	})
	if skip != "" {
		res = probe.RuntimeResult{Skipped: skip}
	}
	var issues []Issue
	for _, i := range res.Issues {
		if i.Severity == store.SeverityError {
			issues = append(issues, i)
		}
	}
	if res.Skipped != "" {
		issues = append(issues, Issue{
			Severity:  store.SeverityWarning,
			Category:  "explore",
			Signature: "explore|skipped",
			Message:   "clicking through the pages was skipped: " + res.Skipped + " (controls, click errors and phone layout were not checked).",
		})
	}
	if restoreErr != nil {
		issues = append(issues, Issue{
			Severity:  store.SeverityWarning,
			Category:  "explore",
			Signature: "explore|rollback",
			Message:   "the data changes made while clicking through the pages could not be fully rolled back: " + restoreErr.Error(),
		})
	}
	return issues
}

func init() {
	RegisterValidator(exploreValidator{})
}
