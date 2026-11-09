package admin

// The ONE home for reading an of_llm_calls row into its JSON field names. The
// call-detail endpoint (builds.go) and the build export (builds_export.go)
// used to carry a 20+ column SELECT and Scan each, edited in lockstep by hand;
// a column added to the log then had to be surfaced twice or drifted silently.

// llmCallColumns is the SELECT list scanLLMCall expects, in scan order.
const llmCallColumns = `id, source, COALESCE(run_id,''), session_id, stage, iteration, model, provider_type,
 request_hash, cache_hit,
 COALESCE(request_messages,''), COALESCE(request_system,''),
 COALESCE(request_tools,''), COALESCE(request_max_tokens, 0),
 COALESCE(response_content,''), COALESCE(response_tool_calls,''),
 COALESCE(response_stop_reason,''), input_tokens, output_tokens,
 cache_read_input_tokens, cache_creation_input_tokens,
 stable_prefix_bytes, stable_prefix_msgs, total_request_bytes,
 duration_ms, COALESCE(error_message, ''), created_at`

type llmCallRow struct {
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

// rowScanner covers both *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

func scanLLMCall(s rowScanner) (*llmCallRow, error) {
	var c llmCallRow
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

// fields renders the JSON keys both consumers share; each adds its own
// request-message representation on top (the detail endpoint replays the full
// bodies, the export interns them into pools).
func (c *llmCallRow) fields() map[string]any {
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
