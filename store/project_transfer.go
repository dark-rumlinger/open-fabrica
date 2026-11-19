package store

import (
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/db"
	"openfabrica/security"
)

// SanitizeExportDB strips per-instance state and secret values from a snapshot
// DB before it is bundled into a shareable .of file. Foreign keys are disabled
// so the deletes are order-independent.
func SanitizeExportDB(path string) error {
	dsn := fmt.Sprintf("%s?_pragma=foreign_keys(0)&_pragma=busy_timeout(30000)", path)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("opening snapshot: %w", err)
	}
	defer d.Close()
	d.SetMaxOpenConns(1)

	stmts := make([]string, 0, len(db.EphemeralResetStmts)+18)
	stmts = append(stmts, db.EphemeralResetStmts...)
	stmts = append(stmts,
		// History and its body snapshots are stripped per the export policy
		// (smaller, cleaner file). of_snapshot_blobs holds version snapshots and is
		// distinct from of_blobs (the general blob store, which is preserved).
		"DELETE FROM of_file_history",
		"DELETE FROM of_snapshot_blobs",
		// Secret values are encrypted with this instance's key and useless /
		// unsafe elsewhere. Keep the names so the importer knows what to set.
		"UPDATE of_secrets SET value_encrypted = ''",
		// A shared project is a clean template: it carries the app and its
		// authored content but none of its secrets or end-user data. Blank the
		// plaintext webhook signing secret (keeping the webhook config so the
		// importer just regenerates a secret; an incoming webhook is rejected
		// until one is set), and drop per-user 2FA secrets, live login/room/invite
		// tokens, and tenant records - all instance-scoped and meaningless or
		// unsafe elsewhere. End-user account rows are cleared separately below.
		"UPDATE of_webhooks SET secret_encrypted = ''",
		"DELETE FROM of_user_totp",
		"DELETE FROM of_magic_tokens",
		"DELETE FROM of_ws_room_tokens",
		"DELETE FROM of_tenant_invites",
		"DELETE FROM of_tenant_members",
		"DELETE FROM of_tenants",
		// End-user records that are not account tables: orders and subscriptions
		// (payer identity, amounts), push endpoints (browser keys), webhook
		// delivery logs and replay ids (request bodies), agent run transcripts,
		// live room state and per-day LLM spend. Config rows (of_payment_config,
		// of_subscription_plans, of_webhooks) are authored content and stay.
		"DELETE FROM of_payments",
		"DELETE FROM of_subscriptions",
		"DELETE FROM of_push_subscriptions",
		"DELETE FROM of_webhook_logs",
		"DELETE FROM of_processed_webhooks",
		"DELETE FROM of_agent_runs",
		"DELETE FROM of_ws_room_state",
		"DELETE FROM of_llm_usage_daily",
	)
	for _, stmt := range stmts {
		if _, err := d.Exec(stmt); err != nil {
			return fmt.Errorf("sanitising (%s): %w", stmt, err)
		}
	}

	// Empty every dynamic account table so the template ships with no users.
	if err := clearAuthAccountTables(d); err != nil {
		return err
	}

	// Reclaim space freed by the deletes.
	if _, err := d.Exec("VACUUM"); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}

// clearAuthAccountTables empties every dynamic table an auth endpoint
// authenticates against, so an exported template carries no end-user accounts
// (emails, password hashes). The table names come from of_auth_endpoints.table_name
// - the authoritative pointer to each account table, alongside its username/password
// columns - and are quoted as identifiers before interpolation. Only auth-referenced
// tables are cleared; other business tables are preserved as app content.
func clearAuthAccountTables(d *sql.DB) error {
	rows, err := d.Query("SELECT DISTINCT table_name FROM of_auth_endpoints WHERE table_name <> ''")
	if err != nil {
		return fmt.Errorf("listing auth account tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("scanning auth account table: %w", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing auth account tables: %w", err)
	}

	for _, name := range tables {
		stmt := fmt.Sprintf("DELETE FROM %s", security.QuoteIdent(name))
		if _, err := d.Exec(stmt); err != nil {
			// An auth endpoint may point at a table that no longer exists.
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			return fmt.Errorf("clearing account table %q: %w", name, err)
		}
	}
	return nil
}
