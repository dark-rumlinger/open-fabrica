package auth

// manage_tenancy: row-level multi-tenant SaaS setup.
//
// Provisions of_tenants + of_tenant_members, switches auth into tenant-aware
// signup modes, and attaches `tenant_id` columns to user-facing tables +
// `tenant_column` to the matching API/upload/WS/import/export endpoints so
// every CRUD path applies tenant isolation automatically.
//
// Once enable + add_tenant_column have run, the dynamic_api / upload / ws
// handlers do all the work; the LLM does not have to remember to write WHERE
// clauses or stamp user IDs. The tool is the only sanctioned path for those
// schema mutations, so we cannot silently miss a table.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"openfabrica/internal/stringutil"
	"openfabrica/tools"

	"openfabrica/events"

	"openfabrica/security"
)

// tenancySlugRe matches valid tenant slugs: lowercase alphanumerics + hyphens,
// no leading/trailing hyphen, 1-63 chars (DNS-safe so subdomain routing works
// without a second sanitiser).
var tenancySlugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// tenancySlugify derives a tenant slug from a display name. The public auth
// signup path (server/public/auth_tenancy.go) mints slugs for the same tables,
// so both delegate to stringutil.Slug to stay identical.
func tenancySlugify(name string) string {
	return stringutil.Slug(name, 63)
}

func tenancyEnable(ctx *tools.ToolContext, authPath, mode, defaultSlug string) (*tools.Result, error) {
	if authPath == "" {
		return tools.Fail("auth_endpoint_path is required"), nil
	}
	if mode == "" {
		// enable passes no mode: keep one an earlier set_signup_mode chose rather than
		// resetting it, and default a fresh endpoint to invite_only.
		var current string
		_ = ctx.DB.QueryRow("SELECT COALESCE(tenant_signup_mode,'') FROM of_auth_endpoints WHERE path = ?", authPath).Scan(&current)
		mode = current
		if mode == "" {
			mode = "invite_only"
		}
	}
	if defaultSlug != "" && !tenancySlugRe.MatchString(defaultSlug) {
		return tools.FailCode(tools.CodeBadArg, "default_tenant_slug %q must be lowercase letters, digits and hyphens (1-63 chars)", defaultSlug), nil
	}
	if mode != "invite_only" && mode != "signup_creates_tenant" && mode != "signup_joins_default" {
		return tools.Fail("signup_mode must be invite_only | signup_creates_tenant | signup_joins_default"), nil
	}

	// Confirm the auth endpoint exists; tenancy attaches to a configured
	// auth surface, not a hypothetical one.
	var exists int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_auth_endpoints WHERE path = ?", authPath).Scan(&exists); err != nil || exists == 0 {
		return tools.Fail("auth endpoint %q not found, call manage_auth action=create first", authPath), nil
	}

	if _, err := ctx.DB.Exec("UPDATE of_auth_endpoints SET tenant_signup_mode = ? WHERE path = ?", mode, authPath); err != nil {
		return nil, fmt.Errorf("setting tenant_signup_mode: %w", err)
	}

	// signup_joins_default needs a default tenant. Create one if the caller
	// supplied a slug, otherwise leave it to a later create_tenant call.
	if mode == "signup_joins_default" && defaultSlug != "" {
		if _, err := tenancyEnsureTenant(ctx, defaultSlug, defaultSlug, "free", "{}"); err != nil {
			return nil, fmt.Errorf("seeding default tenant: %w", err)
		}
	}

	return tools.Ok(map[string]any{
		"auth_endpoint_path":  authPath,
		"signup_mode":         mode,
		"default_tenant_slug": defaultSlug,
		"next_steps":          "Call manage_tenancy add_tenant_column on every user-facing table so their CRUD endpoints scope to claims.tenant_id.",
	}), nil
}

func tenancySetSignupMode(ctx *tools.ToolContext, authPath, mode, defaultSlug string) (*tools.Result, error) {
	return tenancyEnable(ctx, authPath, mode, defaultSlug) // same body and validation
}

// tenancyAddTenantColumn is the workhorse that adds `tenant_id INTEGER` to
// each named table and sets tenant_column on every API/upload/WS/import/
// export endpoint pointing at it. Idempotent on both halves.
func tenancyAddTenantColumn(ctx *tools.ToolContext, tablesRaw []any, col string) (*tools.Result, error) {
	if len(tablesRaw) == 0 {
		return tools.Fail("tables array is required"), nil
	}
	if col == "" {
		col = "tenant_id"
	}
	if err := validateTenancyIdentifier(col); err != nil {
		return tools.FailErr(err), nil
	}

	var touched []string
	scoped := map[string]int{}
	var unwired []string
	for _, raw := range tablesRaw {
		table, ok := raw.(string)
		if !ok || table == "" {
			continue
		}
		if err := validateTenancyIdentifier(table); err != nil {
			return tools.Fail("invalid table name %q: %v", table, err), nil
		}
		// Confirm the table is a project-managed dynamic table; refuse to
		// touch system tables (of_*).
		if strings.HasPrefix(table, "of_") {
			return tools.Fail("refusing to add tenant column to system table %q", table), nil
		}
		var exists int
		if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?", table).Scan(&exists); err != nil || exists == 0 {
			return tools.Fail("table %q is not a dynamic table", table), nil
		}

		// Add the column if it isn't there yet (idempotent).
		hasCol, err := tenancyTableHasColumn(ctx, table, col)
		if err != nil {
			return nil, err
		}
		if !hasCol {
			alter := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s INTEGER", table, col)
			if _, err := ctx.DB.Exec(alter); err != nil {
				return nil, fmt.Errorf("adding %s to %s: %w", col, table, err)
			}
		}

		// Stamp tenant_column on every endpoint config pointing at this
		// table. Each statement is a no-op when tenant_column is already
		// set, so reruns don't churn rows unnecessarily.
		statements := []string{
			"UPDATE of_api_endpoints    SET tenant_column = ? WHERE table_name = ? AND COALESCE(tenant_column,'') = ''",
			"UPDATE of_upload_endpoints SET tenant_column = ? WHERE table_name = ? AND COALESCE(tenant_column,'') = ''",
			"UPDATE of_ws_endpoints     SET tenant_column = ? WHERE write_to_table = ? AND COALESCE(tenant_column,'') = ''",
			"UPDATE of_export_endpoints SET tenant_column = ? WHERE table_name = ? AND COALESCE(tenant_column,'') = ''",
			"UPDATE of_import_endpoints SET tenant_column = ? WHERE table_name = ? AND COALESCE(tenant_column,'') = ''",
		}
		for _, q := range statements {
			if _, err := ctx.DB.Exec(q, col, table); err != nil {
				return nil, fmt.Errorf("propagating tenant_column to endpoints for %s: %w", table, err)
			}
		}

		// Count the endpoints now scoped by this column, so a table with none reads as
		// the gap it is rather than as success.
		n := 0
		for _, q := range []string{
			"SELECT COUNT(*) FROM of_api_endpoints WHERE table_name = ? AND tenant_column = ?",
			"SELECT COUNT(*) FROM of_upload_endpoints WHERE table_name = ? AND tenant_column = ?",
			"SELECT COUNT(*) FROM of_ws_endpoints WHERE write_to_table = ? AND tenant_column = ?",
			"SELECT COUNT(*) FROM of_export_endpoints WHERE table_name = ? AND tenant_column = ?",
			"SELECT COUNT(*) FROM of_import_endpoints WHERE table_name = ? AND tenant_column = ?",
		} {
			var c int
			_ = ctx.DB.QueryRow(q, table, col).Scan(&c)
			n += c
		}
		scoped[table] = n
		if n == 0 {
			unwired = append(unwired, table)
		}

		touched = append(touched, table)
	}

	data := map[string]any{
		"tenant_column":    col,
		"tables":           touched,
		"endpoints_scoped": scoped,
	}
	if len(unwired) > 0 {
		data["warning"] = fmt.Sprintf("No endpoint over %s exists yet, so nothing is tenant-scoped there. This action only stamps endpoints that already exist: create the table's manage_api (and upload/export/import) endpoint, then run add_tenant_column again. Rows inserted before scoping have a NULL %s and are invisible to every tenant.", strings.Join(unwired, ", "), col)
	}
	return tools.Ok(data), nil
}

func tenancyCreateTenant(ctx *tools.ToolContext, slug, name, plan string, settingsMap map[string]any) (*tools.Result, error) {
	if name == "" {
		return tools.Fail("name is required"), nil
	}
	if slug == "" {
		slug = tenancySlugify(name)
	}
	if !tenancySlugRe.MatchString(slug) {
		return tools.Fail("invalid slug %q (lowercase a-z, 0-9, hyphens; 1-63 chars; no leading/trailing hyphen)", slug), nil
	}
	if plan == "" {
		plan = "free"
	}

	settings := "{}"
	if len(settingsMap) > 0 {
		b, err := json.Marshal(settingsMap)
		if err != nil {
			return tools.Fail("settings JSON: %v", err), nil
		}
		settings = string(b)
	}

	id, err := tenancyEnsureTenant(ctx, slug, name, plan, settings)
	if err != nil {
		return nil, err
	}

	if ctx.Bus != nil {
		ctx.Bus.Publish(events.NewEvent(events.EventTenantCreated, ctx.ProjectID, map[string]any{
			"tenant_id":     id,
			"slug":          slug,
			"name":          name,
			"plan":          plan,
			"owner_user_id": 0, // created by the agent, not by a signing-up user
		}))
	}

	return tools.Ok(map[string]any{
		"tenant_id": id,
		"slug":      slug,
		"name":      name,
		"plan":      plan,
	}), nil
}

func tenancyListTenants(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query("SELECT id, slug, name, plan, created_at FROM of_tenants WHERE is_deleted = 0 ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("listing tenants: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id int
		var slug, name, plan, createdAt string
		if err := rows.Scan(&id, &slug, &name, &plan, &createdAt); err == nil {
			out = append(out, map[string]any{
				"id": id, "slug": slug, "name": name, "plan": plan, "created_at": createdAt,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing tenants: %w", err)
	}
	return tools.Ok(map[string]any{"tenants": out, "count": len(out)}), nil
}

func tenancyInviteMember(ctx *tools.ToolContext, tenantID int, email, role string) (*tools.Result, error) {
	if tenantID == 0 {
		return tools.Fail("tenant_id is required"), nil
	}
	if email == "" {
		return tools.Fail("email is required"), nil
	}
	if role == "" {
		role = "member"
	}

	var slug string
	if err := ctx.DB.QueryRow("SELECT slug FROM of_tenants WHERE id = ? AND is_deleted = 0", tenantID).Scan(&slug); err != nil {
		return tools.Fail("tenant %d not found", tenantID), nil
	}

	token, err := tenancyRandomToken()
	if err != nil {
		return nil, fmt.Errorf("generating invite token: %w", err)
	}

	if _, err := ctx.DB.Exec(
		"INSERT INTO of_tenant_invites (tenant_id, email, role, token, expires_at) VALUES (?, ?, ?, ?, datetime('now', '+7 days'))",
		tenantID, email, role, token,
	); err != nil {
		return nil, fmt.Errorf("inserting invite: %w", err)
	}

	if ctx.Bus != nil {
		ctx.Bus.Publish(events.NewEvent(events.EventTenantMemberInvited, ctx.ProjectID, map[string]any{
			"tenant_id": tenantID,
			"slug":      slug,
			"email":     email,
			"role":      role,
			"token":     token,
		}))
	}

	return tools.Ok(map[string]any{
		"tenant_id":    tenantID,
		"tenant_slug":  slug,
		"email":        email,
		"role":         role,
		"invite_token": token,
		"hint":         "Send the invite token to the user. They register via POST /api/<auth_path>/register?invite=<token>. It expires in 7 days.",
	}), nil
}

func tenancySetTenantRole(ctx *tools.ToolContext, tenantID, userID int, role string) (*tools.Result, error) {
	if tenantID == 0 {
		return tools.Fail("tenant_id is required"), nil
	}
	if userID == 0 {
		return tools.Fail("user_id is required"), nil
	}
	if role == "" {
		return tools.Fail("role is required"), nil
	}
	res, err := ctx.DB.Exec(
		"UPDATE of_tenant_members SET role = ? WHERE tenant_id = ? AND user_id = ?",
		role, tenantID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("updating tenant role: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tools.Fail("membership not found"), nil
	}
	return tools.Ok(map[string]any{
		"tenant_id": tenantID, "user_id": userID, "role": role,
	}), nil
}

func tenancyListMembers(ctx *tools.ToolContext, tenantID int) (*tools.Result, error) {
	if tenantID == 0 {
		return tools.Fail("tenant_id is required"), nil
	}
	rows, err := ctx.DB.Query(
		"SELECT user_id, role, status, joined_at FROM of_tenant_members WHERE tenant_id = ? ORDER BY joined_at",
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing members: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var userID int
		var role, status, joinedAt string
		if err := rows.Scan(&userID, &role, &status, &joinedAt); err == nil {
			out = append(out, map[string]any{
				"user_id": userID, "role": role, "status": status, "joined_at": joinedAt,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing members: %w", err)
	}
	return tools.Ok(map[string]any{
		"tenant_id": tenantID, "members": out, "count": len(out),
	}), nil
}

// tenancyEnsureTenant inserts a tenant by slug if missing, returns its ID
// either way. Idempotent for setup scripts and reruns.
func tenancyEnsureTenant(ctx *tools.ToolContext, slug, name, plan, settings string) (int64, error) {
	var id int64
	if err := ctx.DB.QueryRow("SELECT id FROM of_tenants WHERE slug = ?", slug).Scan(&id); err == nil {
		return id, nil
	}
	res, err := ctx.DB.Exec(
		"INSERT INTO of_tenants (slug, name, plan, settings) VALUES (?, ?, ?, ?)",
		slug, name, plan, settings,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting tenant: %w", err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading new tenant id: %w", err)
	}
	return id, nil
}

// tenancyTableHasColumn returns true if the named column exists on the table.
func tenancyTableHasColumn(ctx *tools.ToolContext, table, col string) (bool, error) {
	rows, err := ctx.DB.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("inspecting %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			continue
		}
		if name == col {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("inspecting %s: %w", table, err)
	}
	return false, nil
}

// validateTenancyIdentifier guards against the LLM passing names that would
// break the ALTER TABLE / UPDATE statements above: the platform's identifier rule,
// the one the table and column had to pass to be created.
func validateTenancyIdentifier(s string) error {
	return security.ValidateColumnName(s)
}

func tenancyRandomToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
