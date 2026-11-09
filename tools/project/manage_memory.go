package project

import (
	"fmt"
	"time"
	"unicode/utf8"

	"openfabrica/tools"
)

const (
	memoryMaxEntries  = 100
	memoryMaxValueLen = 500
)

// manage_memory is the persistent key-value store the engine reads across
// owner-chat sessions. Bounded to memoryMaxEntries entries with values
// clipped at memoryMaxValueLen chars so a memory burst can't blow up the
// context window.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_memory",
		Category: "project",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult: 8000,
		OneLiner:  "Manage persistent key-value memory the engine reads across owner-chat sessions.",
		WhenToUse: `Store *durable* owner facts that survive across sessions - preferences,
decisions, stray context from chat (favorite colors, business hours, who their
customers are). The plan and snapshot already carry ephemeral build state; don't
duplicate it. recall reads values, list enumerates keys. Application/business data
belongs in manage_data, credentials in manage_secrets.

Categories preferences, decisions, and facts are surfaced to the planner at the
start of future builds; others are stored but not auto-surfaced.

Limits (per project): 100 entries, value clipped at 500 chars. At capacity a store
evicts the least-recently-updated entry (reported as "evicted") rather than failing.`,
		Actions: []tools.Action{
			&MemoryStore{},
			&MemoryRecall{},
			&MemoryList{},
			&MemoryForget{},
		},
	})
}

// MemoryStore saves a key-value pair, overwriting on conflict. Enforces the
// per-project entry cap and clips overly long values.
type MemoryStore struct {
	Key      string `arg:"key,required"   desc:"Memory key (unique identifier)."`
	Value    string `arg:"value,required" desc:"Memory value (max 500 chars)."`
	Category string `arg:"category"       desc:"Memory category. preferences, decisions and facts (the default) are surfaced to planning; any other category is stored but not surfaced." default:"facts"`
}

func (a *MemoryStore) Doc() string { return "Save a key-value pair. Overwrites on conflict." }
func (a *MemoryStore) Example() any {
	return &MemoryStore{Key: "brand_color", Value: "warm orange, not red", Category: "preferences"}
}
func (a *MemoryStore) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	value := a.Value
	clipped := false
	if len(value) > memoryMaxValueLen {
		// Cut on a rune boundary so a multi-byte character is never split.
		cut := memoryMaxValueLen
		for cut > 0 && !utf8.RuneStart(value[cut]) {
			cut--
		}
		value = value[:cut]
		clipped = true
	}
	var existing int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_memory WHERE key = ?", a.Key).Scan(&existing); err != nil {
		return nil, fmt.Errorf("checking memory key: %w", err)
	}
	var evicted string
	if existing == 0 {
		var total int
		if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_memory").Scan(&total); err != nil {
			return nil, fmt.Errorf("counting memory entries: %w", err)
		}
		// At capacity: evict the least-recently-updated entry to make room
		// rather than failing the store. The evicted key is reported back so
		// the drop is visible to the caller and operator, never silent.
		if total >= memoryMaxEntries {
			if err := ctx.DB.QueryRow(
				"SELECT key FROM of_memory ORDER BY updated_at ASC, id ASC LIMIT 1",
			).Scan(&evicted); err == nil && evicted != "" {
				_, _ = ctx.DB.Exec("DELETE FROM of_memory WHERE key = ?", evicted)
			}
		}
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := ctx.DB.Exec(
		"INSERT INTO of_memory (key, value, category, updated_at) VALUES (?, ?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value, category = excluded.category, updated_at = excluded.updated_at",
		a.Key, value, a.Category, now,
	); err != nil {
		return tools.Fail("failed to store memory: %v", err), nil
	}
	data := map[string]any{"key": a.Key, "category": a.Category}
	if evicted != "" {
		data["evicted"] = evicted
	}
	if clipped {
		data["truncated"] = fmt.Sprintf("value cut to %d bytes; keep memories short", memoryMaxValueLen)
	}
	return tools.Ok(data), nil
}

// MemoryRecall returns one stored value by key.
type MemoryRecall struct {
	Key string `arg:"key,required" desc:"Memory key to look up."`
}

func (a *MemoryRecall) Doc() string    { return "Look up a stored value by key." }
func (a *MemoryRecall) ReadOnly() bool { return true }
func (a *MemoryRecall) Example() any   { return &MemoryRecall{Key: "brand_color"} }
func (a *MemoryRecall) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var value, category, updatedAt string
	if err := ctx.DB.QueryRow("SELECT value, category, updated_at FROM of_memory WHERE key = ?", a.Key).Scan(&value, &category, &updatedAt); err != nil {
		return tools.Fail("memory not found: %s", a.Key), nil
	}
	return tools.Ok(map[string]any{
		"key": a.Key, "value": value, "category": category, "updated_at": updatedAt,
	}), nil
}

// MemoryList returns recent memories, newest first.
type MemoryList struct {
	Category string `arg:"category" desc:"Optional category filter."`
}

func (a *MemoryList) Doc() string {
	return "List the 50 most recently updated memories, newest first (up to 100 can be stored; recall reads any one by key). Optionally filtered by category."
}
func (a *MemoryList) ReadOnly() bool { return true }
func (a *MemoryList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var query string
	var queryArgs []any
	if a.Category != "" {
		query = "SELECT key, value, category, updated_at FROM of_memory WHERE category = ? ORDER BY updated_at DESC LIMIT 50"
		queryArgs = []any{a.Category}
	} else {
		query = "SELECT key, value, category, updated_at FROM of_memory ORDER BY updated_at DESC LIMIT 50"
	}
	rows, err := ctx.DB.Query(query, queryArgs...)
	if err != nil {
		return tools.Fail("failed to list memories: %v", err), nil
	}
	defer rows.Close()
	var memories []map[string]any
	for rows.Next() {
		var key, value, cat, updatedAt string
		if rows.Scan(&key, &value, &cat, &updatedAt) == nil {
			memories = append(memories, map[string]any{
				"key": key, "value": value, "category": cat, "updated_at": updatedAt,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return tools.Fail("failed to list memories: %v", err), nil
	}
	return tools.Ok(map[string]any{"memories": memories, "count": len(memories)}), nil
}

// MemoryForget deletes a memory by key.
type MemoryForget struct {
	Key string `arg:"key,required" desc:"Memory key to delete."`
}

func (a *MemoryForget) Doc() string  { return "Delete a memory by key." }
func (a *MemoryForget) Example() any { return &MemoryForget{Key: "brand_color"} }
func (a *MemoryForget) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	result, err := ctx.DB.Exec("DELETE FROM of_memory WHERE key = ?", a.Key)
	if err != nil {
		return tools.Fail("failed to forget: %v", err), nil
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return tools.Fail("memory not found: %s", a.Key), nil
	}
	return tools.Ok(map[string]any{"key": a.Key, "deleted": true}), nil
}
