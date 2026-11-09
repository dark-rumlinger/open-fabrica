package validators

// authoritativeReducerValidator is the build-time counterpart to set_reducer's
// save-time parse check: it runs each authoritative endpoint's reducer through its
// @init/@join/view lifecycle in the real goja sandbox (probe.RunAuthoritativeReducerSmoke),
// turning a throw/timeout/non-JSON result into a fixable build issue. No browser, so
// it runs regardless of Chrome.

import (
	"context"
	"database/sql"

	"openfabrica/probe"
)

type authoritativeReducerValidator struct{}

func (authoritativeReducerValidator) Name() string { return "authoritative_reducer" }

func (authoritativeReducerValidator) Validate(ctx context.Context, pdb *sql.DB, _, _ string) []Issue {
	return probe.RunAuthoritativeReducerSmoke(ctx, pdb)
}

func init() { RegisterValidator(authoritativeReducerValidator{}) }
