package engine

// Retention for the append-only tables a running project fills as a side effect of
// being used. Without a window each grows for the life of the project: one row per
// page view, one per audit event, one per delivered webhook. The LLM tables have
// their own windows (see llmCallRetention); these are the rest.

import (
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/internal/sqliteretry"
)

// retentionRule deletes rows of table older than window, judged by col. where
// narrows it to rows that are safe to drop (finished jobs, read notifications).
type retentionRule struct {
	table  string
	col    string
	where  string
	window time.Duration
}

const day = 24 * time.Hour

var retentionRules = []retentionRule{
	{table: "of_analytics", col: "created_at", window: 90 * day},
	{table: "of_activity_log", col: "created_at", window: 90 * day},
	{table: "of_webhook_logs", col: "created_at", window: 30 * day},
	// Idempotency keys only need to outlive a provider's redelivery window (days).
	{table: "of_processed_webhooks", col: "created_at", window: 30 * day},
	{table: "of_magic_tokens", col: "created_at", window: 2 * day},
	// Finished jobs are history; pending and running ones are never touched.
	{table: "of_jobs", col: "COALESCE(completed_at, created_at)", where: "status IN ('completed', 'failed', 'cancelled')", window: 30 * day},
	// A read notification the user has not deleted in 90 days is clutter.
	{table: "of_notifications", col: "created_at", where: "is_read = 1", window: 90 * day},
}

// pruneRetained applies every rule to one project database and returns how many
// rows each table lost. A table that does not exist (an older schema) is skipped.
func pruneRetained(pdb *db.ProjectDB, now time.Time) (map[string]int64, error) {
	deleted := map[string]int64{}
	for _, r := range retentionRules {
		cutoff := now.Add(-r.window).UTC().Format("2006-01-02 15:04:05")
		q := "DELETE FROM " + r.table + " WHERE " + r.col + " < ?"
		if r.where != "" {
			q += " AND " + r.where
		}
		res, err := sqliteretry.Exec(pdb.Writer(), q, cutoff)
		if err != nil {
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			return deleted, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			deleted[r.table] = n
		}
	}
	return deleted, nil
}
