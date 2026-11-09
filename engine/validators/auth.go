package validators

// The static auth-gate validator: catches an API endpoint requiring a logged-in
// user in a project with NO login surface at all, so every write 401s - dead on
// arrival. The hard-gate counterpart to the save-time auth_warning nudge, which
// the build agent can finish past; a page-load probe cannot see it (the write
// fires on a form submit, not on load). Static, so it runs even when the probe
// self-skips; post-build, so an "API task before auth task" ordering is not a
// false positive - only a build that ENDS with an auth-gated API and no login.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/store"
)

type authGateValidator struct{}

func (authGateValidator) Name() string { return "auth_gate" }

func (authGateValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	findings := store.AuthGatedEndpointsWithoutAuth(pdb)
	if len(findings) == 0 {
		return nil
	}
	issues := make([]Issue, 0, len(findings))
	for _, f := range findings {
		scope := "every write"
		if !f.PublicRead {
			scope = "every read and write"
		}
		var msg string
		if strings.TrimSpace(f.OwnerColumn) != "" {
			// Planned owner-scoped: the fix is to add a login, NOT to flip it public.
			// Going public would discard the per-user ownership and expose every row to
			// every visitor - the silent leak this branch exists to prevent.
			msg = fmt.Sprintf("Endpoint `/api/%s` is owner-scoped (per-user rows via `%s`) and requires a logged-in user, but this project has NO login surface (no create_auth/create_oauth), so %s 401s from the browser - the feature is dead on arrival. PREFER adding an auth surface (a user table + `create_auth`) so each user signs in and sees only their own rows. Do NOT just set `auth_model=public`: that makes EVERY row world-readable AND world-writable (any visitor can list, overwrite, and delete all rows), discarding the per-user ownership the plan chose. If this data is personal but needs no accounts, drop the endpoint and persist it client-side in localStorage instead.",
				f.Path, strings.TrimSpace(f.OwnerColumn), scope)
		} else {
			msg = fmt.Sprintf("Endpoint `/api/%s` requires a logged-in user but this project has NO login surface (no create_auth/create_oauth), so %s 401s from the browser - the app's writes are dead on arrival. "+
				"Either add an auth endpoint so users can sign in, or set `auth_model=public` via `manage_api action=update` (add `public_read=true` if reads should stay open) when the app has no accounts.",
				f.Path, scope)
		}
		issues = append(issues, Issue{
			Severity: "error",
			Category: "auth_gate",
			// Signature, not File: the finding names an API resource, not a file.
			Signature:     f.Path,
			Deterministic: true,
			Message:       msg,
		})
	}
	return issues
}

func init() {
	RegisterValidator(authGateValidator{})
}
