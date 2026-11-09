package llm

// Role represents the role of a message in a conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message represents a single message in a conversation.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`

	// Images attaches inline image parts to a user message. When non-empty,
	// providers send the message in multimodal content-array form alongside
	// Content as the text part. Only honored on user messages; non-user
	// messages with Images set are serialized without the images (graceful
	// degradation).
	Images []MessageImage `json:"-"`

	// Cacheable marks a stable user message as a prompt-cache breakpoint.
	// Anthropic honours this via cache_control: ephemeral, as do the
	// OpenAI-compatible gateways that map to an explicit-cache backend
	// (OpenRouter -> Anthropic/Gemini), which emit the same control in the
	// content-part format. Automatic-cache endpoints (OpenAI, DeepSeek, Z.ai)
	// ignore it and rely on byte-stable prefixes plus prompt_cache_key. Use for
	// long, slow-changing user-message context blocks (e.g. the live
	// project_state snapshot) so iteration N+1 reads from cache instead of
	// re-billing the bytes.
	Cacheable bool `json:"-"`
}

// MessageImage is an inline image attachment on a user message. Data is the
// raw bytes; MIMEType is the image's Content-Type (e.g. "image/png").
type MessageImage struct {
	MIMEType string
	Data     []byte
}

// ToolCall represents a tool invocation requested by the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// ToolDef defines a tool that can be used by the model.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Type        string         `json:"type,omitempty"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema
}
