package store

import "database/sql"

// LLMCallColumns is the SELECT list ScanLLMCall expects, in scan order.
const LLMCallColumns = `id, source, COALESCE(run_id,''), session_id, stage, iteration, model, provider_type,
 request_hash, cache_hit,
 COALESCE(request_messages,''), COALESCE(request_system,''),
 COALESCE(request_tools,''), COALESCE(request_max_tokens, 0),
 COALESCE(response_content,''), COALESCE(response_tool_calls,''),
 COALESCE(response_stop_reason,''), input_tokens, output_tokens,
 cache_read_input_tokens, cache_creation_input_tokens,
 stable_prefix_bytes, stable_prefix_msgs, total_request_bytes,
 duration_ms, COALESCE(error_message, ''), created_at`

// LLMCallRow represents a single row from of_llm_calls.
type LLMCallRow struct {
	ID                               int64
	Source, RunID, SessionID, Stage  string
	Iteration                        int
	Model, ProviderType, RequestHash string
	CacheHit                         int
	ReqMessages, ReqSystem, ReqTools string
	ReqMaxTokens                     int
	RespContent, RespToolCalls       string
	RespStop                         string
	InputTokens, OutputTokens        int
	CacheRead, CacheCreation         int
	StablePrefixBytes                int
	StablePrefixMsgs                 int
	TotalRequestBytes                int
	DurationMS                       int
	ErrorMessage, CreatedAt          string
}

type rowScanner interface{ Scan(dest ...any) error }

// ScanLLMCall scans a single row selected with LLMCallColumns.
func ScanLLMCall(s rowScanner) (*LLMCallRow, error) {
	var c LLMCallRow
	if err := s.Scan(&c.ID, &c.Source, &c.RunID, &c.SessionID, &c.Stage, &c.Iteration, &c.Model, &c.ProviderType,
		&c.RequestHash, &c.CacheHit,
		&c.ReqMessages, &c.ReqSystem, &c.ReqTools, &c.ReqMaxTokens,
		&c.RespContent, &c.RespToolCalls, &c.RespStop, &c.InputTokens, &c.OutputTokens,
		&c.CacheRead, &c.CacheCreation,
		&c.StablePrefixBytes, &c.StablePrefixMsgs, &c.TotalRequestBytes,
		&c.DurationMS, &c.ErrorMessage, &c.CreatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// Fields renders the JSON keys shared by call-detail and build-export responses.
func (c *LLMCallRow) Fields() map[string]any {
	return map[string]any{
		"id":                          c.ID,
		"source":                      c.Source,
		"session_id":                  c.SessionID,
		"stage":                       c.Stage,
		"iteration":                   c.Iteration,
		"model":                       c.Model,
		"provider_type":               c.ProviderType,
		"request_hash":                c.RequestHash,
		"replay_cache_hit":            c.CacheHit == 1,
		"request_tools":               c.ReqTools,
		"request_max_tokens":          c.ReqMaxTokens,
		"response_content":            c.RespContent,
		"response_tool_calls":         c.RespToolCalls,
		"response_stop_reason":        c.RespStop,
		"input_tokens":                c.InputTokens,
		"output_tokens":               c.OutputTokens,
		"cache_read_input_tokens":     c.CacheRead,
		"cache_creation_input_tokens": c.CacheCreation,
		"stable_prefix_bytes":         c.StablePrefixBytes,
		"stable_prefix_msgs":          c.StablePrefixMsgs,
		"total_request_bytes":         c.TotalRequestBytes,
		"duration_ms":                 c.DurationMS,
		"error_message":               c.ErrorMessage,
		"created_at":                  c.CreatedAt,
	}
}

// LoadLLMCallByID loads a single of_llm_calls row by primary key.
func LoadLLMCallByID(reader *sql.DB, logID int64) (*LLMCallRow, error) {
	return ScanLLMCall(reader.QueryRow(`SELECT `+LLMCallColumns+` FROM of_llm_calls WHERE id = ?`, logID))
}

// LoadRunEngineLLMCalls loads all engine LLM calls for runID in ascending id order.
func LoadRunEngineLLMCalls(reader *sql.DB, runID string) ([]*LLMCallRow, error) {
	rows, err := reader.Query(
		`SELECT `+LLMCallColumns+` FROM of_llm_calls WHERE run_id = ? AND source = 'engine' ORDER BY id ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*LLMCallRow
	for rows.Next() {
		c, err := ScanLLMCall(rows)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
