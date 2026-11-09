package api

import (
	"fmt"
	"strings"

	"openfabrica/security"
	"openfabrica/store"
)

// Shared validation/translation helpers for the endpoint handlers. These factor
// out logic that the create and update paths both perform, so the two cannot
// diverge in how they validate identifiers or interpret auth_model.

// validateColumnRef validates a user-supplied column identifier that will be
// interpolated into SQL (owner_column, lookup_column). The label tags the
// returned error so the caller surfaces which argument was rejected.
func validateColumnRef(label, name string) error {
	if err := security.ValidateColumnName(name); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// authModelRequiresAuth reports whether a manage_api auth_model implies a
// logged-in user. Every non-public model (authenticated/role/owner/authored) does.
func authModelRequiresAuth(authModel string) bool {
	switch authModel {
	case "authenticated", "role", "owner", "authored":
		return true
	default:
		return false
	}
}

// validatePublicWrite rejects the two contradictory public_write combinations,
// returning a human-readable message (empty = valid). public_write opens the
// write methods on an otherwise auth-gated endpoint (the contact-form shape), so
// it is meaningless with auth_model=public (already fully open) and incompatible
// with auth_model=owner (an anonymous writer has no identity to own the row).
func validatePublicWrite(writeIsPublic bool, authModel, ownerColumn string) string {
	if !writeIsPublic {
		return ""
	}
	if authModel == "" || authModel == "public" {
		return "public_write only applies to an auth-gated endpoint; it is redundant with auth_model=public (everything is already open). Use auth_model=authenticated with public_write=true for a public-submission endpoint whose reads stay gated."
	}
	if authModel == "owner" || authModel == "authored" || ownerColumn != "" {
		return "public_write cannot be combined with auth_model=owner/authored/owner_column: an anonymous writer has no identity to scope the row to. Drop owner scoping, or require auth on writes."
	}
	return ""
}

// validateMethods rejects an HTTP method a CRUD endpoint cannot answer; the router
// would store it and then never dispatch it.
func validateMethods(methods []string) string {
	for _, m := range methods {
		switch strings.ToUpper(strings.TrimSpace(m)) {
		case "GET", "POST", "PUT", "DELETE":
		default:
			return fmt.Sprintf("methods accepts GET, POST, PUT, DELETE (got %q)", m)
		}
	}
	return ""
}

// validateAuthModelArgs rejects auth settings the chosen auth_model would silently
// drop (translateAuthModel keeps required_role only for role and owner_column only for
// owner/authored), so the call cannot report success for a gate that does not exist:
// role with no role name acted as plain authenticated, and an owner_column on
// public/authenticated/role was discarded while the create hint announced RLS.
func validateAuthModelArgs(authModel, requiredRole, ownerColumn string) string {
	if authModel == "role" && strings.TrimSpace(requiredRole) == "" {
		return "auth_model=role needs required_role (the role name a caller must have, e.g. \"admin\"; admin passes every role gate)"
	}
	if strings.TrimSpace(requiredRole) != "" && authModel != "role" {
		return fmt.Sprintf("required_role only applies with auth_model=role; with auth_model=%s it would be ignored and the endpoint would NOT be role-gated. Set auth_model=role, or drop required_role", modelOrPublic(authModel))
	}
	if ownerColumn != "" && authModel != "owner" && authModel != "authored" {
		return fmt.Sprintf("owner_column only applies with auth_model=owner (private per user) or authored (shared reads, own writes); with auth_model=%s it would be ignored", modelOrPublic(authModel))
	}
	return ""
}

func modelOrPublic(m string) string {
	if m == "" {
		return "public"
	}
	return m
}

// translateAuthModel maps an auth_model enum into the persisted
// requires_auth / required_role / owner_column shape used at create time. role
// is only carried for auth_model=role; owner is carried for owner and authored
// (both scope by owner_column, differing only in whether reads are shared, which
// ownerScopeForModel captures).
func translateAuthModel(authModel, requiredRole, ownerColumn string) (requiresAuth bool, role, owner string) {
	switch authModel {
	case "authenticated":
		return true, "", ""
	case "role":
		return true, requiredRole, ""
	case "owner", "authored":
		return true, "", ownerColumn
	default:
		return false, "", ""
	}
}

// ownerScopeForModel returns the owner_scope stored for an auth_model. Only
// authored narrows owner_column to writes (shared reads); every other model
// leaves it empty, so owner_column (when present) scopes reads and writes alike.
func ownerScopeForModel(authModel string) string {
	if authModel == "authored" {
		return store.OwnerScopeAuthored
	}
	return ""
}
