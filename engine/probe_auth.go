package engine

// mintProbeAuthToken issues a short-lived session JWT so the headless runtime
// probe can load requireRole-gated pages as a real signed-in user. Without it
// the probe is anonymous, every gated page bounces to /login, and the page's
// authenticated data path (the code most worth checking) never runs.

import (
	"database/sql"
	"fmt"
	"regexp"
	"time"

	"openfabrica/security"
)

// safeIdent guards the identifiers we interpolate into mint queries. The values
// come from of_auth_endpoints (platform-controlled), but column/table names
// cannot be bound as SQL parameters, so we still validate them defensively.
var safeIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// privilegedRoles is the lowercased set we promote when picking a probe user, so
// a requireRole('admin') page renders instead of bouncing a member to home.
const privilegedRolesSQL = `('admin','owner','superadmin','super_admin','root')`

// mintProbeAuthToken returns a JWT for the highest-privilege seeded user of the
// project's first auth endpoint, or "" when the project has no auth endpoint, no
// users, or no JWT manager (in which case the probe stays anonymous, exactly as
// before). The token is scoped to projectID so the project's /me accepts it.
func mintProbeAuthToken(pdb *sql.DB, jwt *security.JWTManager, projectID int) string {
	if pdb == nil || jwt == nil {
		return ""
	}
	var table, userCol, roleCol string
	if err := pdb.QueryRow(
		`SELECT table_name, username_column, COALESCE(role_column,'') FROM of_auth_endpoints ORDER BY id LIMIT 1`,
	).Scan(&table, &userCol, &roleCol); err != nil {
		return ""
	}
	if !safeIdent.MatchString(table) || !safeIdent.MatchString(userCol) {
		return ""
	}

	var (
		id       int
		username string
		role     string
	)
	var err error
	if roleCol != "" && safeIdent.MatchString(roleCol) {
		// Prefer a privileged user so role-gated pages render under the probe.
		q := fmt.Sprintf(
			`SELECT id, %s, COALESCE(%s,'') FROM %s
			 ORDER BY CASE WHEN lower(%s) IN %s THEN 0 ELSE 1 END, id
			 LIMIT 1`, security.QuoteIdent(userCol), security.QuoteIdent(roleCol), security.QuoteIdent(table), security.QuoteIdent(roleCol), privilegedRolesSQL)
		err = pdb.QueryRow(q).Scan(&id, &username, &role)
	} else {
		q := fmt.Sprintf(`SELECT id, %s, '' FROM %s ORDER BY id LIMIT 1`, security.QuoteIdent(userCol), security.QuoteIdent(table))
		err = pdb.QueryRow(q).Scan(&id, &username, &role)
	}
	if err != nil || id == 0 {
		return ""
	}

	tok, err := jwt.Generate(security.TokenRequest{ProjectID: projectID, UserID: id, Username: username, Role: role, Expiry: time.Hour})
	if err != nil {
		return ""
	}
	return tok
}
