package validators

// The static RLS-scope validator: catches an API endpoint over a row-owned
// table that applies no row-level scope, so every caller reads (and writes)
// every other user's rows (IDOR). Runtime enforcement is correct once scoped;
// WHETHER it gets scoped rests on the build agent, whose only other net is the
// save-time rls_warning advisory it can finish past. Promotes the same
// heuristic (store.UnscopedOwnerEndpoints) to a hard gate. Static, so it runs
// even when the probe self-skips.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/store"
)

type rlsScopeValidator struct{}

func (rlsScopeValidator) Name() string { return "rls_scope" }

// Validate flags every writable endpoint whose backing table is row-owned but
// left unscoped. baseURL and hostHeader are unused: the check is pure static
// analysis over the project DB - the same source of truth the save-time
// rls_warning uses - so a finding means the same thing on save and on build.
func (rlsScopeValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	findings := store.UnscopedOwnerEndpoints(pdb)
	if len(findings) == 0 {
		return nil
	}
	// An endpoint whose access model the plan LOCKED (a data-ownership decision with
	// an endpoint auth_model check) is judged against that decision by the conformance
	// validator: a deliberately public or role-gated table is not a leak, and a locked
	// owner table that was built unscoped is already reported there with the exact fix.
	locked := lockedEndpointModels(pdb)
	issues := make([]Issue, 0, len(findings))
	for _, f := range findings {
		if _, ok := locked[strings.Trim(f.Path, "/")]; ok {
			continue
		}
		if f.Container != "" {
			// Collaborative-content shape: the table is row-owned AND carries a
			// container reference, so its rows are usually one shared list many
			// members read. Per-user owner scoping - the fix a hard error would
			// force - would hide every other member's rows and break the shared
			// view. The right scope depends on intent the static check cannot
			// see, so this is a speculative, non-looping warning that leads with
			// authored rather than a destructive fix-pass driver.
			issues = append(issues, Issue{
				Severity: "warning",
				Category: "rls_scope",
				// Signature, not File: the finding names an API resource, not a file.
				Signature:   f.Path,
				Speculative: true,
				Message: fmt.Sprintf("Endpoint `/api/%s` serves row-owned table `%s` (author column `%s`, grouped by `%s`) but sets no owner_column/tenant_column, so every authenticated user can read AND write every other user's rows. These rows look SHARED within `%s` (a collaborative list members co-read, each authoring their own). To let members read all rows but edit only their own, use "+
					"`manage_api action=update endpoint_path=%s auth_model=authored owner_column=%s` (send auth_model in the SAME call). If the rows are instead PRIVATE per user, use auth_model=owner, which scopes reads too. For hard isolation between different `%s` values, add tenant scoping with `manage_tenancy action=add_tenant_column`.",
					f.Path, f.Table, f.Column, f.Container, f.Container, f.Path, f.Column, f.Container),
			})
			continue
		}
		if f.PublicRead {
			// A public-read endpoint exposing a row-owned table MIGHT be intentional
			// (a public leaderboard whose rows carry created_by). The structural
			// signal alone cannot prove intent, so this is a speculative,
			// non-looping warning: surfaced for the model to weigh, never a
			// destructive fix-pass driver. The remedy is offered, not forced.
			issues = append(issues, Issue{
				Severity:    "warning",
				Category:    "rls_scope",
				Signature:   f.Path,
				Speculative: true,
				Message: fmt.Sprintf("Endpoint `/api/%s` serves table `%s` (owner-like column `%s`) with public read and no owner_column/tenant_column, "+
					"so anyone can read every row without authenticating. If these rows are per-user or private, scope the endpoint with "+
					"`manage_api action=update endpoint_path=%s auth_model=owner owner_column=%s` (send auth_model in the SAME call - owner_column "+
					"alone is ignored). For multi-tenant isolation instead, use `manage_tenancy action=add_tenant_column`, which is the only action "+
					"that sets tenant_column; manage_api has no such arg. "+
					"If the data is genuinely public (e.g. a shared leaderboard), no change is needed.",
					f.Path, f.Table, f.Column, f.Path, f.Column),
			})
			continue
		}
		// An AUTHENTICATED endpoint over a row-owned table with no scope is a
		// near-certain isolation bug: every logged-in user reads and writes every
		// other user's rows. Deterministic (reproduces on re-check), so it drives
		// the fix-pass until scoped or shipped as a known issue.
		issues = append(issues, Issue{
			Severity:      "error",
			Category:      "rls_scope",
			Signature:     f.Path,
			Deterministic: true,
			Message: fmt.Sprintf("Endpoint `/api/%s` serves row-owned table `%s` (owner-like column `%s`) but sets no owner_column/tenant_column, so every "+
				"authenticated user can read AND write every other user's rows - a cross-user data leak. Scope it to the caller with "+
				"`manage_api action=update endpoint_path=%s auth_model=owner owner_column=%s` - send auth_model in the SAME call, because "+
				"owner_column on its own is ignored. For multi-tenant isolation instead, use `manage_tenancy action=add_tenant_column`, which is "+
				"the only action that sets tenant_column; manage_api has no such arg. "+
				"Only leave it unscoped if the table is deliberately shared across all users.",
				f.Path, f.Table, f.Column, f.Path, f.Column),
		})
	}
	return issues
}

func init() {
	RegisterValidator(rlsScopeValidator{})
}

// lockedEndpointModels maps an endpoint path to the auth_model the latest plan locked
// for it through a data-ownership decision check.
func lockedEndpointModels(pdb *sql.DB) map[string]string {
	plan := latestPlan(pdb)
	if plan == nil {
		return nil
	}
	out := map[string]string{}
	for _, d := range plan.Decisions {
		c := d.Check
		if c == nil {
			continue
		}
		a := strings.ToLower(strings.TrimSpace(c.Artifact))
		if (a != "endpoint" && a != "api") || !strings.EqualFold(strings.TrimSpace(c.Field), "auth_model") {
			continue
		}
		path := strings.Trim(strings.TrimPrefix(strings.TrimSpace(c.ID), "/api/"), "/")
		if path != "" {
			out[path] = strings.ToLower(fmt.Sprint(c.Value))
		}
	}
	return out
}
