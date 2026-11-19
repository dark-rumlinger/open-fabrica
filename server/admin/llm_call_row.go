package admin

// llmCallColumns is the SELECT list for reading an of_llm_calls row.
const llmCallColumns = `id, source, COALESCE(run_id,''), session_id, stage, iteration, model, provider_type,
 request_hash, cache_hit,
 COALESCE(request_messages,''), COALESCE(request_system,''),
 COALESCE(request_tools,''), COALESCE(request_max_tokens, 0),
 COALESCE(response_content,''), COALESCE(response_tool_calls,''),
 COALESCE(response_stop_reason,''), input_tokens, output_tokens,
 cache_read_input_tokens, cache_creation_input_tokens,
 stable_prefix_bytes, stable_prefix_msgs, total_request_bytes,
 duration_ms, COALESCE(error_message, ''), created_at`
