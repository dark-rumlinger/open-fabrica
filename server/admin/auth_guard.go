package admin

import (
	"net/http"

	"openfabrica/db/models"
	"openfabrica/security"
)

// adminAuthGuard runs after the JWT Authenticator on every protected admin
// route, and is the only authorization the admin API has: the panel has one
// kind of account, the admin. It enforces three things the signature check
// alone does not:
//
//  1. The token must be an admin token (Claims.ForAdmin). Admin and project
//     users are signed with the same key, and a project's own user can carry
//     role "admin" within that project.
//  2. The token's embedded version must still match the user's current
//     token_version. A password change or explicit sign-out-everywhere bumps the
//     stored version, instantly invalidating tokens minted before it.
func adminAuthGuard(deps *Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := r.Context().Value(security.ClaimsKey).(*security.Claims)
			if !ok || claims == nil {
				writeError(w, http.StatusUnauthorized, "missing authorization")
				return
			}
			// Reject project-scoped tokens outright: the admin API is never
			// reachable with a project user's credentials.
			if !claims.ForAdmin() {
				writeError(w, http.StatusForbidden, "project-scoped token cannot access the admin API")
				return
			}
			current, err := models.GetUserTokenVersion(deps.DB.DB, claims.UserID)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "invalid session")
				return
			}
			if claims.TokenVersion != current {
				security.ClearAuthCookie(w)
				writeError(w, http.StatusUnauthorized, "session expired, please sign in again")
				return
			}
			if claims.Role != "admin" {
				writeError(w, http.StatusForbidden, "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
