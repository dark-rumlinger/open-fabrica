package public

// Tenancy plumbing for the public auth surface: tenant-aware token minting, the
// tenant switcher, and tenant listing, split off so auth.go's register/login bodies
// stay focused on credentials.
//
// The two entry points other parts of the handler call:
//   - generateUserTokenWithTenant: pick the user's active tenant (or none) and mint
//     a JWT carrying tenant_id + tenant_role.
//   - handleTenantSubroute: dispatch /tenant/switch and /tenants; returns true when
//     the path was handled.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/internal/stringutil"
	"openfabrica/security"
)

// tenantMembership is the read-side shape used to mint tokens and render the
// switcher. Pulled together from of_tenant_members + of_tenants.
type tenantMembership struct {
	TenantID int    `json:"tenant_id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Status   string `json:"status"`
}

// loadActiveMemberships returns the user's active tenants, ordered by joined_at
// so the first row is a sensible auto-pick when the user has exactly one.
func (h *Handler) loadActiveMemberships(projectDB *sql.DB, userID int) ([]tenantMembership, error) {
	rows, err := projectDB.Query(
		`SELECT t.id, t.slug, t.name, m.role, m.status
		 FROM of_tenant_members m
		 JOIN of_tenants t ON t.id = m.tenant_id AND t.is_deleted = 0
		 WHERE m.user_id = ? AND m.status = 'active'
		 ORDER BY m.joined_at ASC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tenantMembership
	for rows.Next() {
		var m tenantMembership
		if err := rows.Scan(&m.TenantID, &m.Slug, &m.Name, &m.Role, &m.Status); err == nil {
			out = append(out, m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// generateUserTokenWithTenant mints a JWT for the user, folding in the active
// tenant when the project has tenant memberships. Returns the token plus the
// memberships and the selected one so callers can auto-redirect or render a
// switcher. Selection rules:
//   - 1 active membership: auto-pick.
//   - 0 active memberships: tokenless response when tenant_signup_mode != "none";
//     otherwise a non-tenanted token (projects without tenancy).
//   - >1 and preferredSlug matches one: pick that; otherwise return memberships
//     with no token so the client calls /switch.
func (h *Handler) generateUserTokenWithTenant(
	projectDB *sql.DB,
	projectID, userID int,
	username, role, preferredSlug string,
	ae *authEndpointConfig,
) (token string, memberships []tenantMembership, selected *tenantMembership, err error) {
	if h.deps.JWTManager == nil {
		return "", nil, nil, fmt.Errorf("JWT manager not configured")
	}

	// Projects without tenancy: keep the existing token shape.
	if ae == nil || ae.TenantSignupMode == "" || ae.TenantSignupMode == "none" {
		t, err := h.deps.JWTManager.Generate(security.TokenRequest{ProjectID: projectID, UserID: userID, Username: username, Role: role, Expiry: authTokenExpiry(ae)})
		return t, nil, nil, err
	}

	memberships, err = h.loadActiveMemberships(projectDB, userID)
	if err != nil {
		return "", nil, nil, fmt.Errorf("loading memberships: %w", err)
	}

	switch len(memberships) {
	case 0:
		// Tenanted project, user with no membership. Return a tokenless result;
		// the caller decides whether to send signup hints to the client. We do
		// NOT mint a tenant_id=0 token here, that token would be rejected on
		// every tenanted endpoint.
		return "", nil, nil, nil
	case 1:
		m := memberships[0]
		t, err := h.deps.JWTManager.Generate(security.TokenRequest{ProjectID: projectID, UserID: userID, TenantID: m.TenantID, Username: username, Role: role, TenantRole: m.Role, Expiry: authTokenExpiry(ae)})
		return t, memberships, &m, err
	default:
		if preferredSlug != "" {
			for i := range memberships {
				if memberships[i].Slug == preferredSlug {
					m := memberships[i]
					t, err := h.deps.JWTManager.Generate(security.TokenRequest{ProjectID: projectID, UserID: userID, TenantID: m.TenantID, Username: username, Role: role, TenantRole: m.Role, Expiry: authTokenExpiry(ae)})
					return t, memberships, &m, err
				}
			}
		}
		// Multiple memberships, no preference: return the list, no token. The
		// client renders a chooser and re-posts login with ?tenant=<slug>.
		// NOT /tenant/switch - that re-mints an existing token and needs a valid
		// one to authenticate, which this caller does not have yet.
		return "", memberships, nil, nil
	}
}

// handleTenantSubroute dispatches tenant-management sub-actions on the auth
// endpoint. Returns true when the path matched. Used by handleAuthRequest.
func (h *Handler) handleTenantSubroute(
	w http.ResponseWriter,
	r *http.Request,
	projectID int,
	projectDB *sql.DB,
	ae *authEndpointConfig,
	action string,
) bool {
	switch action {
	case "tenants":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET required")
			return true
		}
		h.authListTenants(w, r, projectID, projectDB, ae)
		return true
	case "tenant/switch":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return true
		}
		h.authSwitchTenant(w, r, projectID, projectDB, ae)
		return true
	}
	return false
}

// authListTenants returns the tenants the calling user belongs to.
func (h *Handler) authListTenants(w http.ResponseWriter, r *http.Request, projectID int, projectDB *sql.DB, ae *authEndpointConfig) {
	claims, err := h.extractUserClaims(r)
	if err != nil || claims.ProjectID != projectID {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	memberships, err := h.loadActiveMemberships(projectDB, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "loading memberships")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active_tenant_id": claims.TenantID,
		"tenants":          memberships,
		"count":            len(memberships),
	})
}

// authSwitchTenant re-mints the caller's JWT with a new active tenant. The
// caller must already be authenticated and a member of the target tenant.
func (h *Handler) authSwitchTenant(w http.ResponseWriter, r *http.Request, projectID int, projectDB *sql.DB, ae *authEndpointConfig) {
	claims, err := h.extractUserClaims(r)
	if err != nil || claims.ProjectID != projectID {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body struct {
		TenantSlug string `json:"tenant_slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.TenantSlug) == "" {
		writeError(w, http.StatusBadRequest, "tenant_slug is required")
		return
	}

	memberships, err := h.loadActiveMemberships(projectDB, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "loading memberships")
		return
	}
	var picked *tenantMembership
	for i := range memberships {
		if memberships[i].Slug == body.TenantSlug {
			picked = &memberships[i]
			break
		}
	}
	if picked == nil {
		writeError(w, http.StatusForbidden, "not a member of that tenant")
		return
	}

	token, err := h.deps.JWTManager.Generate(security.TokenRequest{ProjectID: projectID, UserID: claims.UserID, TenantID: picked.TenantID, Username: claims.Username, Role: claims.Role, TenantRole: picked.Role, Expiry: authTokenExpiry(ae)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to mint token")
		return
	}

	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventTenantSwitched, projectID, map[string]any{
			"user_id":   claims.UserID,
			"tenant_id": picked.TenantID,
			"slug":      picked.Slug,
		}))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"token":     token,
		"tenant_id": picked.TenantID,
		"slug":      picked.Slug,
		"role":      picked.Role,
	})
}

// inviteUsable reports whether an invite token is present, unexpired and
// addressed to this email, without consuming it. Lets registration refuse before
// it creates the account.
func (h *Handler) inviteUsable(readDB *sql.DB, token, email string) bool {
	if token == "" {
		return false
	}
	var n int
	err := readDB.QueryRow(
		`SELECT COUNT(*) FROM of_tenant_invites
		 WHERE token = ? AND (email = ? OR email = '')
		   AND (expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP)`,
		token, email,
	).Scan(&n)
	return err == nil && n > 0
}

// consumeInviteToken atomically validates an invite token, returns the
// associated tenant_id + role, and deletes the row. Returns ok=false when the
// token is missing or already consumed; callers translate that into a 4xx
// without leaking which case it was.
func (h *Handler) consumeInviteToken(writeDB *sql.DB, token, email string) (tenantID int, role string, ok bool) {
	err := db.RunInTx(writeDB, func(tx *sql.Tx) error {
		// Enforce expiry alongside the token/email match: an invite past its
		// expires_at must be treated as already gone, not silently honoured.
		// expires_at IS NULL means "never expires".
		if err := tx.QueryRow(
			`SELECT tenant_id, role FROM of_tenant_invites
			 WHERE token = ? AND (email = ? OR email = '')
			   AND (expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP)`,
			token, email,
		).Scan(&tenantID, &role); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM of_tenant_invites WHERE token = ?", token)
		return err
	})
	if err != nil {
		return 0, "", false
	}
	if role == "" {
		role = "member"
	}
	return tenantID, role, true
}

// applyRegistrationTenancy resolves the new user's tenant according to the
// auth endpoint's signup mode and inserts the membership row. Returns the
// chosen tenant_id + role (so the post-register token mint can fold them in),
// or an HTTP error message when the request can't proceed.
//
// Side effects (membership insert, tenant creation for signup_creates_tenant,
// invite consumption) all happen on writeDB.
func (h *Handler) applyRegistrationTenancy(
	r *http.Request,
	writeDB *sql.DB,
	projectID, userID int,
	username string,
	ae *authEndpointConfig,
) (tenantID int, tenantRole, errMsg string, status int) {
	if ae.TenantSignupMode == "" || ae.TenantSignupMode == "none" {
		return 0, "", "", 0
	}

	switch ae.TenantSignupMode {
	case "invite_only":
		token := r.URL.Query().Get("invite")
		if token == "" {
			return 0, "", "this endpoint requires an invite token (?invite=...)", http.StatusForbidden
		}
		tid, role, ok := h.consumeInviteToken(writeDB, token, username)
		if !ok {
			return 0, "", "invalid or expired invite", http.StatusForbidden
		}
		if _, err := writeDB.Exec(
			"INSERT INTO of_tenant_members (tenant_id, user_id, role, status) VALUES (?, ?, ?, 'active')",
			tid, userID, role,
		); err != nil {
			return 0, "", "creating membership: " + err.Error(), http.StatusInternalServerError
		}
		if h.deps.Bus != nil {
			h.deps.Bus.Publish(events.NewEvent(events.EventTenantMemberJoined, projectID, map[string]any{
				"tenant_id": tid, "user_id": userID, "role": role,
			}))
		}
		return tid, role, "", 0

	case "signup_creates_tenant":
		// New user owns a new tenant. Slug derived from the username so we
		// don't have to ask for one in the body; conflicts get a numeric
		// suffix.
		base := slugifyForTenant(username)
		if base == "" {
			base = fmt.Sprintf("tenant-%d", userID)
		}
		slug := base
		for i := 2; i < 100; i++ {
			var exists int
			_ = writeDB.QueryRow("SELECT COUNT(*) FROM of_tenants WHERE slug = ?", slug).Scan(&exists)
			if exists == 0 {
				break
			}
			slug = fmt.Sprintf("%s-%d", base, i)
		}
		res, err := writeDB.Exec(
			"INSERT INTO of_tenants (slug, name, plan, settings) VALUES (?, ?, 'free', '{}')",
			slug, slug,
		)
		if err != nil {
			return 0, "", "creating tenant: " + err.Error(), http.StatusInternalServerError
		}
		newID, err := res.LastInsertId()
		if err != nil {
			return 0, "", "creating tenant: could not read new tenant id", http.StatusInternalServerError
		}
		if _, err := writeDB.Exec(
			"INSERT INTO of_tenant_members (tenant_id, user_id, role, status) VALUES (?, ?, 'owner', 'active')",
			newID, userID,
		); err != nil {
			return 0, "", "creating membership: " + err.Error(), http.StatusInternalServerError
		}
		if h.deps.Bus != nil {
			h.deps.Bus.Publish(events.NewEvent(events.EventTenantCreated, projectID, map[string]any{
				"tenant_id": newID, "slug": slug, "name": slug, "plan": "free", "owner_user_id": userID,
			}))
		}
		return int(newID), "owner", "", 0

	case "signup_joins_default":
		var defID int
		// The "default" tenant is the lowest-id active tenant; manage_tenancy
		// enable seeds one when default_tenant_slug is provided.
		if err := writeDB.QueryRow("SELECT id FROM of_tenants WHERE is_deleted = 0 ORDER BY id ASC LIMIT 1").Scan(&defID); err != nil {
			return 0, "", "no default tenant configured", http.StatusInternalServerError
		}
		if _, err := writeDB.Exec(
			"INSERT INTO of_tenant_members (tenant_id, user_id, role, status) VALUES (?, ?, 'member', 'active')",
			defID, userID,
		); err != nil {
			return 0, "", "creating membership: " + err.Error(), http.StatusInternalServerError
		}
		if h.deps.Bus != nil {
			h.deps.Bus.Publish(events.NewEvent(events.EventTenantMemberJoined, projectID, map[string]any{
				"tenant_id": defID, "user_id": userID, "role": "member",
			}))
		}
		return defID, "member", "", 0
	}

	return 0, "", "unknown tenant_signup_mode: " + ae.TenantSignupMode, http.StatusInternalServerError
}

// slugifyForTenant derives a tenant slug from a username. The manage_tenancy
// tool mints slugs for the same tables, so both delegate to stringutil.Slug
// to stay identical.
func slugifyForTenant(s string) string {
	return stringutil.Slug(s, 63)
}
