package llm

// StreamChunk represents a single chunk in a streaming response.
type StreamChunk struct {
	Delta      string      `json:"delta,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	Done       bool        `json:"done"`
	StopReason string      `json:"stop_reason,omitempty"`
	Usage      *Usage      `json:"usage,omitempty"`
	Error      error       `json:"-"`

	// Reasoning carries an incremental chain-of-thought token delta from
	// providers that stream a separate reasoning channel (Z.ai/DeepSeek
	// `reasoning_content`, xAI/OpenAI-style `reasoning`). Empty for
	// non-reasoning providers and for non-reasoning chunks. Consumers that
	// don't care can ignore it; consumers that surface it (logged_provider,
	// frontend "thinking" indicators) accumulate it like Delta.
	Reasoning string `json:"reasoning,omitempty"`
}

// ToolResult represents the result of a tool execution, streamed back to the
// frontend so tool cards can update in real-time.
type ToolResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error,omitempty"`
}

// Usage represents token usage information.
//
// CacheReadInputTokens and CacheCreationInputTokens are populated by providers
// that support prompt caching (Anthropic). They are advisory, providers
// without prompt caching leave them at zero. The total input billed is
// approximately InputTokens + CacheCreationInputTokens + CacheReadInputTokens,
// but cached reads cost ~10% of normal input tokens.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// StreamCallback is a function called for each chunk during streaming.
type StreamCallback func(StreamChunk)
