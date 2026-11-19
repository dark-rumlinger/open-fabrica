package store

import (
	"database/sql"
	"errors"
	"time"

	"openfabrica/db"
)

var (
	ErrMemoryNotFound  = errors.New("memory not found")
	ErrSecretNotFound  = errors.New("secret not found")
	ErrTriggerNotFound = errors.New("trigger not found")
	ErrJobNotFound     = errors.New("job not found or already completed/cancelled")
)

// MemoryEntry represents a row in of_memory.
type MemoryEntry struct {
	ID        int       `json:"id"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Category  string    `json:"category"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListMemories returns all of_memory rows ordered by updated_at DESC.
func ListMemories(pdb *db.ProjectDB) []MemoryEntry {
	rows, err := pdb.Query("SELECT id, key, value, category, created_at, updated_at FROM of_memory ORDER BY updated_at DESC")
	if err != nil {
		return []MemoryEntry{}
	}
	defer rows.Close()

	out := []MemoryEntry{}
	for rows.Next() {
		var e MemoryEntry
		if rows.Scan(&e.ID, &e.Key, &e.Value, &e.Category, &e.CreatedAt, &e.UpdatedAt) == nil {
			out = append(out, e)
		}
	}
	if rows.Err() != nil {
		return []MemoryEntry{}
	}
	return out
}

// UpsertMemory stores or updates a memory entry by key.
func UpsertMemory(pdb *db.ProjectDB, key, value, category string) error {
	_, err := pdb.ExecWrite(
		`INSERT INTO of_memory (key, value, category, updated_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, category = excluded.category, updated_at = CURRENT_TIMESTAMP`,
		key, value, category,
	)
	return err
}

// UpdateMemory updates a memory entry by ID.
func UpdateMemory(pdb *db.ProjectDB, id int, value, category string) error {
	res, err := pdb.ExecWrite(
		"UPDATE of_memory SET value = ?, category = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		value, category, id,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

// DeleteMemory deletes a memory entry by ID.
func DeleteMemory(pdb *db.ProjectDB, id int) error {
	res, err := pdb.ExecWrite("DELETE FROM of_memory WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

// SecretEntry represents a metadata row in of_secrets (never exposing the value).
type SecretEntry struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	HasValue  bool      `json:"has_value"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListSecrets returns all of_secrets metadata rows ordered by name.
func ListSecrets(pdb *db.ProjectDB) []SecretEntry {
	rows, err := pdb.Query("SELECT id, name, value_encrypted <> '', created_at, updated_at FROM of_secrets ORDER BY name")
	if err != nil {
		return []SecretEntry{}
	}
	defer rows.Close()

	out := []SecretEntry{}
	for rows.Next() {
		var s SecretEntry
		if rows.Scan(&s.ID, &s.Name, &s.HasValue, &s.CreatedAt, &s.UpdatedAt) == nil {
			out = append(out, s)
		}
	}
	if rows.Err() != nil {
		return []SecretEntry{}
	}
	return out
}

// DeleteSecret removes a secret by ID.
func DeleteSecret(pdb *db.ProjectDB, secretID int) error {
	res, err := pdb.ExecWrite("DELETE FROM of_secrets WHERE id = ?", secretID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSecretNotFound
	}
	return nil
}

// UpdateSecretValue replaces the encrypted value of an existing secret by ID.
func UpdateSecretValue(pdb *db.ProjectDB, secretID int, encrypted string) error {
	res, err := pdb.ExecWrite(
		"UPDATE of_secrets SET value_encrypted = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		encrypted, secretID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSecretNotFound
	}
	return nil
}

// ListTriggers returns all of_triggers rows ordered newest-first.
func ListTriggers(pdb *db.ProjectDB) []map[string]any {
	rows, err := pdb.Query(
		`SELECT id, name, event_type, event_filter, trigger_type, trigger_config, timing, is_enabled, created_at
		 FROM of_triggers ORDER BY created_at DESC`,
	)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()

	triggers := []map[string]any{}
	for rows.Next() {
		var id int
		var name, eventType, triggerType, triggerConfig string
		var eventFilter, timing sql.NullString
		var isEnabled bool
		var createdAt time.Time
		if rows.Scan(&id, &name, &eventType, &eventFilter, &triggerType, &triggerConfig, &timing, &isEnabled, &createdAt) != nil {
			continue
		}
		trig := map[string]any{
			"id": id, "name": name, "event_type": eventType,
			"trigger_type": triggerType, "trigger_config": triggerConfig,
			"is_enabled": isEnabled, "created_at": createdAt,
		}
		if eventFilter.Valid && eventFilter.String != "" {
			trig["event_filter"] = eventFilter.String
		}
		if timing.Valid {
			trig["timing"] = timing.String
		}
		triggers = append(triggers, trig)
	}
	if rows.Err() != nil {
		return []map[string]any{}
	}
	return triggers
}

// ToggleTrigger flips is_enabled atomically and returns the new state.
func ToggleTrigger(pdb *db.ProjectDB, triggerID int) (bool, error) {
	if _, err := pdb.ExecWrite("UPDATE of_triggers SET is_enabled = NOT is_enabled WHERE id = ?", triggerID); err != nil {
		return false, err
	}
	var isEnabled bool
	if err := pdb.QueryRow("SELECT is_enabled FROM of_triggers WHERE id = ?", triggerID).Scan(&isEnabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrTriggerNotFound
		}
		return false, err
	}
	return isEnabled, nil
}

// DeleteTrigger deletes a trigger by ID.
func DeleteTrigger(pdb *db.ProjectDB, triggerID int) error {
	res, err := pdb.ExecWrite("DELETE FROM of_triggers WHERE id = ?", triggerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTriggerNotFound
	}
	return nil
}

// ListJobs returns recent of_jobs rows (up to 100), newest-first.
func ListJobs(pdb *db.ProjectDB) []map[string]any {
	rows, err := pdb.Query(
		`SELECT id, type, status, attempts, max_attempts, error, scheduled_at, completed_at, created_at
		 FROM of_jobs ORDER BY id DESC LIMIT 100`,
	)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()

	jobs := []map[string]any{}
	for rows.Next() {
		var id, attempts, maxAttempts int
		var jobType, status string
		var jobError sql.NullString
		var scheduledAt, completedAt sql.NullTime
		var createdAt time.Time
		if rows.Scan(&id, &jobType, &status, &attempts, &maxAttempts, &jobError, &scheduledAt, &completedAt, &createdAt) != nil {
			continue
		}
		job := map[string]any{
			"id": id, "type": jobType, "status": status,
			"attempts": attempts, "max_attempts": maxAttempts, "created_at": createdAt,
		}
		if jobError.Valid && jobError.String != "" {
			job["error"] = jobError.String
		}
		if scheduledAt.Valid {
			job["scheduled_at"] = scheduledAt.Time
		}
		if completedAt.Valid {
			job["completed_at"] = completedAt.Time
		}
		jobs = append(jobs, job)
	}
	_ = rows.Err()
	return jobs
}

// CancelJob marks a pending or running job as cancelled.
func CancelJob(pdb *db.ProjectDB, jobID int) error {
	res, err := pdb.ExecWrite(
		"UPDATE of_jobs SET status = 'cancelled' WHERE id = ? AND status IN ('pending', 'running')",
		jobID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrJobNotFound
	}
	return nil
}

// ProjectSummaryMetrics holds per-project DB counters for the admin summary card.
type ProjectSummaryMetrics struct {
	TotalTokens       int64
	BrainActions      int
	EngineTokensToday int64
	PageViews         int
	UniqueVisitors    int
	PagesCount        int
}

// LoadProjectSummaryMetrics queries token spend, 24h traffic, and page count.
func LoadProjectSummaryMetrics(pdb *db.ProjectDB) ProjectSummaryMetrics {
	var m ProjectSummaryMetrics
	_ = pdb.QueryRow(
		`SELECT COALESCE(SUM(input_tokens + output_tokens), 0),
		        COALESCE(SUM(CASE WHEN source = 'engine' THEN 1 ELSE 0 END), 0)
		 FROM of_llm_calls`,
	).Scan(&m.TotalTokens, &m.BrainActions)

	_ = pdb.QueryRow(
		`SELECT COALESCE(SUM(input_tokens + output_tokens + cache_read_input_tokens + cache_creation_input_tokens), 0)
		 FROM of_llm_calls WHERE source = 'engine' AND created_at >= date('now')`,
	).Scan(&m.EngineTokensToday)

	_ = pdb.QueryRow(
		`SELECT COUNT(*), COUNT(DISTINCT visitor_hash)
		 FROM of_analytics WHERE created_at > datetime('now', '-1 day')`,
	).Scan(&m.PageViews, &m.UniqueVisitors)

	_ = pdb.QueryRow(
		`SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND is_deleted = 0`,
	).Scan(&m.PagesCount)

	return m
}

// LoadBuildQueue returns a snapshot of of_tasks counts and up to 60 ordered tasks.
func LoadBuildQueue(pdb *db.ProjectDB) map[string]any {
	empty := map[string]any{
		"total": 0, "done": 0, "active": 0, "pending": 0, "blocked": 0,
		"tasks": []map[string]any{},
	}
	rows, err := pdb.Query(`SELECT id, title, status, COALESCE(origin,'') FROM of_tasks ORDER BY position, id`)
	if err != nil {
		return empty
	}
	defer rows.Close()

	const maxTasks = 60
	var total, done, active, pending, blocked int
	list := make([]map[string]any, 0)
	for rows.Next() {
		var id, title, status, origin string
		if rows.Scan(&id, &title, &status, &origin) != nil {
			continue
		}
		total++
		switch status {
		case "done":
			done++
		case "active":
			active++
		case "blocked":
			blocked++
		default:
			pending++
		}
		if len(list) < maxTasks {
			list = append(list, map[string]any{
				"id": id, "title": title, "status": status, "origin": origin,
			})
		}
	}
	if rows.Err() != nil {
		return empty
	}
	return map[string]any{
		"total": total, "done": done, "active": active,
		"pending": pending, "blocked": blocked, "tasks": list,
	}
}
