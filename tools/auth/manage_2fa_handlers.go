package auth

import (
	"fmt"
	"time"

	"openfabrica/internal/endpoints"
	"openfabrica/tools"
	"openfabrica/tools/api"
)

// TOTP and magic-link handlers for manage_2fa. Schema lives in manage_2fa.go.

// TOTP 2FA

func authEnableTOTP(ctx *tools.ToolContext, authPath, issuer string, enforced bool) (*tools.Result, error) {
	authPath = tools.NormalizeEndpointPath(authPath)
	if authPath == "" {
		return tools.Fail("endpoint_path (the auth endpoint) is required"), nil
	}
	var authExists int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_auth_endpoints WHERE path = ?", authPath).Scan(&authExists)
	if authExists == 0 {
		return tools.Fail("auth endpoint %q not found", authPath), nil
	}

	if err := api.ClaimEndpoint(ctx, endpoints.KindTOTP, authPath, nil); err != nil {
		return tools.FailErr(err), nil
	}

	_, err := ctx.DB.Exec(`
		INSERT INTO of_totp_endpoints (path, auth_endpoint_path, issuer, enforced)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET issuer = excluded.issuer, enforced = excluded.enforced`,
		authPath, authPath, issuer, enforced,
	)
	if err != nil {
		return nil, fmt.Errorf("enable_totp: %w", err)
	}
	out := map[string]any{
		"auth_endpoint_path": authPath,
		"setup_url":          fmt.Sprintf("/api/%s/totp/setup", authPath),
		"verify_url":         fmt.Sprintf("/api/%s/totp/verify", authPath),
		"enforced":           enforced,
	}
	if enforced {
		out["note"] = "enforced: /login withholds the session from an account that has not enrolled and answers {needs_totp_setup:true, enroll_token}; the page POSTs /totp/setup then /totp/verify {code} with Authorization: Bearer <enroll_token>, and the verify response carries the session token. Build that enrollment screen."
	} else {
		out["note"] = "not enforced: /login challenges only users who enrolled (POST setup then verify while signed in). An account that never enrolled still logs in with its password alone; pass enforced=true to require enrollment."
	}
	return tools.Ok(out), nil
}

func authDisableTOTP(ctx *tools.ToolContext, authPath string) (*tools.Result, error) {
	authPath = tools.NormalizeEndpointPath(authPath)
	if authPath == "" {
		return tools.FailCode(tools.CodeMissingArg, "auth_path is required (the auth endpoint path, e.g. \"auth\")"), nil
	}
	if _, err := ctx.DB.Exec("DELETE FROM of_totp_endpoints WHERE path = ?", authPath); err != nil {
		return nil, fmt.Errorf("disabling totp for %q: %w", authPath, err)
	}
	tools.LogDestructiveAction(ctx.DB, "manage_2fa", "disable_totp", authPath)
	return tools.Ok(map[string]any{"disabled": authPath}), nil
}

func authListTOTP(ctx *tools.ToolContext, authPath string) (*tools.Result, error) {
	authPath = tools.NormalizeEndpointPath(authPath)
	query := `SELECT id, path, issuer, enforced, created_at FROM of_totp_endpoints`
	queryArgs := []any{}
	if authPath != "" {
		query += ` WHERE path = ?`
		queryArgs = append(queryArgs, authPath)
	}
	query += ` ORDER BY path`
	rows, err := ctx.DB.Query(query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id int
		var path, issuer string
		var enforced bool
		var created time.Time
		if err := rows.Scan(&id, &path, &issuer, &enforced, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "endpoint_path": path, "issuer": issuer,
			"enforced": enforced, "created_at": created,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tools.Ok(out), nil
}

// Magic-link passwordless login

func authEnableMagicLink(ctx *tools.ToolContext, authPath, emailTemplate, redirectURL string, ttlMinutes float64) (*tools.Result, error) {
	authPath = tools.NormalizeEndpointPath(authPath)
	if authPath == "" {
		return tools.FailCode(tools.CodeMissingArg, "auth_path is required (the auth endpoint path, e.g. \"auth\")"), nil
	}
	var exists int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_auth_endpoints WHERE path = ?", authPath).Scan(&exists)
	if exists == 0 {
		return tools.Fail("auth endpoint %q not found", authPath), nil
	}
	if redirectURL == "" {
		redirectURL = "/"
	}
	ttl := 15
	if ttlMinutes > 0 {
		ttl = int(ttlMinutes + 0.5)
		if ttl < 1 {
			ttl = 1
		}
	}
	if emailTemplate != "" {
		var n int
		_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_email_templates WHERE name = ?", emailTemplate).Scan(&n)
		if n == 0 {
			return tools.FailCode(tools.CodeNotFound, "email template %q not found - save it first with manage_email action=save_template (put the link at {{magic_url}}), or omit email_template for the default email", emailTemplate), nil
		}
	}

	if err := api.ClaimEndpoint(ctx, endpoints.KindMagic, authPath, nil); err != nil {
		return tools.FailErr(err), nil
	}

	_, err := ctx.DB.Exec(`
		INSERT INTO of_magic_link_endpoints (path, auth_endpoint_path, email_template, redirect_url, ttl_minutes)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  email_template = excluded.email_template,
		  redirect_url = excluded.redirect_url,
		  ttl_minutes = excluded.ttl_minutes`,
		authPath, authPath, emailTemplate, redirectURL, ttl,
	)
	if err != nil {
		return nil, err
	}
	return tools.Ok(map[string]any{
		"auth_endpoint_path": authPath,
		"request_url":        fmt.Sprintf("/api/%s/magic/request", authPath),
		"verify_url":         fmt.Sprintf("/api/%s/magic/verify", authPath),
		"ttl_minutes":        ttl,
	}), nil
}

func authDisableMagicLink(ctx *tools.ToolContext, authPath string) (*tools.Result, error) {
	authPath = tools.NormalizeEndpointPath(authPath)
	if authPath == "" {
		return tools.FailCode(tools.CodeMissingArg, "auth_path is required (the auth endpoint path, e.g. \"auth\")"), nil
	}
	if _, err := ctx.DB.Exec("DELETE FROM of_magic_link_endpoints WHERE path = ?", authPath); err != nil {
		return nil, fmt.Errorf("disabling magic-link for %q: %w", authPath, err)
	}
	tools.LogDestructiveAction(ctx.DB, "manage_2fa", "disable_magic_link", authPath)
	return tools.Ok(map[string]any{"disabled": authPath}), nil
}

func authListMagicLink(ctx *tools.ToolContext, authPath string) (*tools.Result, error) {
	authPath = tools.NormalizeEndpointPath(authPath)
	query := `SELECT id, path, redirect_url, ttl_minutes, created_at FROM of_magic_link_endpoints`
	queryArgs := []any{}
	if authPath != "" {
		query += ` WHERE path = ?`
		queryArgs = append(queryArgs, authPath)
	}
	query += ` ORDER BY path`
	rows, err := ctx.DB.Query(query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, ttl int
		var path, redirect string
		var created time.Time
		if err := rows.Scan(&id, &path, &redirect, &ttl, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "endpoint_path": path, "redirect_url": redirect,
			"ttl_minutes": ttl, "created_at": created,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tools.Ok(out), nil
}
