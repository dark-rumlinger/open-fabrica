package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// AgentEndpointRow holds raw columns from of_agent_endpoints.
type AgentEndpointRow struct {
	Path             string
	SystemPrompt     string
	ModelID          string
	MaxTokens        int
	Temperature      float64
	MaxSteps         int
	RateLimit        int
	DailyTokenBudget int
	DailyTokensUsed  int
	DayWindowStart   string
	RequiresAuth     bool
	CORSOrigins      string
	Functions        string
	AllowDataQuery   bool
	AllowHTTPFetch   bool
	MaxSubagents     int
	MaxDepth         int
}

// LoadAgentEndpoint loads an agent endpoint configuration by path.
func LoadAgentEndpoint(reader *sql.DB, path string) (*AgentEndpointRow, error) {
	var ep AgentEndpointRow
	err := reader.QueryRow(
		"SELECT path, system_prompt, model_id, max_tokens, temperature, max_steps, rate_limit, daily_token_budget, daily_tokens_used, day_window_start, requires_auth, cors_origins, functions, allow_data_query, max_subagents, max_depth, allow_http_fetch FROM of_agent_endpoints WHERE path = ?",
		path,
	).Scan(
		&ep.Path, &ep.SystemPrompt, &ep.ModelID, &ep.MaxTokens, &ep.Temperature,
		&ep.MaxSteps, &ep.RateLimit, &ep.DailyTokenBudget, &ep.DailyTokensUsed,
		&ep.DayWindowStart, &ep.RequiresAuth, &ep.CORSOrigins, &ep.Functions,
		&ep.AllowDataQuery, &ep.MaxSubagents, &ep.MaxDepth, &ep.AllowHTTPFetch,
	)
	if err != nil {
		return nil, err
	}
	return &ep, nil
}

// ResetAgentEndpointDailyBudget resets daily_tokens_used and updates day_window_start.
func ResetAgentEndpointDailyBudget(db *sql.DB, path, today string) error {
	_, err := db.Exec("UPDATE of_agent_endpoints SET daily_tokens_used = 0, day_window_start = ? WHERE path = ?", today, path)
	return err
}

// InsertAgentRun records a new running async agent run in of_agent_runs.
func InsertAgentRun(writer *sql.DB, runID, path, reqJSON string) error {
	_, err := writer.Exec(
		`INSERT INTO of_agent_runs (id, path, status, request) VALUES (?, ?, 'running', ?)`,
		runID, path, reqJSON,
	)
	return err
}

// MarkAgentRunFailed updates an of_agent_runs row to status='failed'.
func MarkAgentRunFailed(writer *sql.DB, runID, errMsg string) error {
	_, err := writer.Exec(
		`UPDATE of_agent_runs SET status='failed', error=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		errMsg, runID,
	)
	return err
}

// MarkAgentRunDone updates an of_agent_runs row to status='done' with its metrics.
func MarkAgentRunDone(writer *sql.DB, runID, content, stopReason string, steps, subagents, inTok, outTok int) error {
	_, err := writer.Exec(
		`UPDATE of_agent_runs SET status='done', content=?, stop_reason=?, steps=?, subagents=?, input_tokens=?, output_tokens=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		content, stopReason, steps, subagents, inTok, outTok, runID,
	)
	return err
}

// AgentRunStatusRow holds the status and metrics for an async agent run.
type AgentRunStatusRow struct {
	Status       string
	Content      string
	StopReason   string
	RunErr       string
	Steps        int
	Subagents    int
	InputTokens  int
	OutputTokens int
	AgeSecs      int
}

// LoadAgentRunStatus loads an of_agent_runs row by id and endpoint path.
func LoadAgentRunStatus(reader *sql.DB, runID, path string) (*AgentRunStatusRow, error) {
	var r AgentRunStatusRow
	err := reader.QueryRow(
		`SELECT status, content, stop_reason, steps, subagents, input_tokens, output_tokens, error,
		        CAST(strftime('%s','now') - strftime('%s', updated_at) AS INTEGER)
		   FROM of_agent_runs WHERE id = ? AND path = ?`,
		runID, path,
	).Scan(&r.Status, &r.Content, &r.StopReason, &r.Steps, &r.Subagents, &r.InputTokens, &r.OutputTokens, &r.RunErr, &r.AgeSecs)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListAppTableNames returns up to limit non-platform table names from sqlite_master.
func ListAppTableNames(reader *sql.DB, limit int) ([]string, error) {
	rows, err := reader.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'of\_%' ESCAPE '\' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}

// ListScopedAPITables returns the lower-cased names of tables behind an
// of_api_endpoints row with owner_column or tenant_column configured.
func ListScopedAPITables(reader *sql.DB) (map[string]bool, error) {
	rows, err := reader.Query(
		`SELECT table_name FROM of_api_endpoints WHERE COALESCE(owner_column,'') != '' OR COALESCE(tenant_column,'') != ''`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil && n != "" {
			out[strings.ToLower(n)] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LLMEndpointRow holds raw columns from of_llm_endpoints.
type LLMEndpointRow struct {
	Path             string
	SystemPrompt     string
	ModelID          string
	MaxTokens        int
	Temperature      float64
	MaxHistory       int
	RateLimit        int
	DailyTokenBudget int
	DailyTokensUsed  int
	DayWindowStart   string
	RequiresAuth     bool
	CORSOrigins      string
	ResponseSchema   string
	KnowledgeBase    string
}

// LoadLLMEndpoint loads an LLM proxy endpoint configuration by path.
func LoadLLMEndpoint(reader *sql.DB, path string) (*LLMEndpointRow, error) {
	var ep LLMEndpointRow
	err := reader.QueryRow(
		"SELECT path, system_prompt, model_id, max_tokens, temperature, max_history, rate_limit, daily_token_budget, daily_tokens_used, day_window_start, requires_auth, cors_origins, response_schema, knowledge_base FROM of_llm_endpoints WHERE path = ?",
		path,
	).Scan(
		&ep.Path, &ep.SystemPrompt, &ep.ModelID, &ep.MaxTokens, &ep.Temperature,
		&ep.MaxHistory, &ep.RateLimit, &ep.DailyTokenBudget, &ep.DailyTokensUsed,
		&ep.DayWindowStart, &ep.RequiresAuth, &ep.CORSOrigins, &ep.ResponseSchema,
		&ep.KnowledgeBase,
	)
	if err != nil {
		return nil, err
	}
	return &ep, nil
}

// ArchiveLLMEndpointDailyUsage archives a previous day's token total into of_llm_usage_daily.
func ArchiveLLMEndpointDailyUsage(db *sql.DB, path, day string, tokens int) error {
	_, err := db.Exec(
		"INSERT OR IGNORE INTO of_llm_usage_daily (path, day, tokens_used) VALUES (?, ?, ?)",
		path, day, tokens,
	)
	return err
}

// ResetLLMEndpointDailyBudget resets daily_tokens_used and updates day_window_start.
func ResetLLMEndpointDailyBudget(db *sql.DB, path, today string) error {
	_, err := db.Exec(
		"UPDATE of_llm_endpoints SET daily_tokens_used = 0, day_window_start = ? WHERE path = ?",
		today, path,
	)
	return err
}

// AddEndpointDailyTokens increments daily_tokens_used on of_llm_endpoints or of_agent_endpoints.
func AddEndpointDailyTokens(db *sql.DB, table, path string, tokens int) error {
	if table != "of_llm_endpoints" && table != "of_agent_endpoints" {
		return fmt.Errorf("invalid endpoint table: %s", table)
	}
	_, err := db.Exec(
		"UPDATE "+table+" SET daily_tokens_used = daily_tokens_used + ? WHERE path = ?",
		tokens, path,
	)
	return err
}
