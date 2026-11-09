package engine

// Canonical request hashing for the content-addressed replay cache
// (of_llm_cache): semantically equal requests hash identically, and any
// semantic difference hashes differently.
//
// Field classification (TestEveryCompletionRequestFieldIsClassified fails on any
// CompletionRequest field that is neither hashed nor listed here):
//   - Maps serialize deterministically: encoding/json sorts keys.
//   - Effort is hashed: the same prompt at two efforts is two requests.
//   - Images are hashed in, but callers must not cache them anyway.
//   - Excluded: PromptCacheKey and prefix diagnostics (routing hints, not
//     semantics); CacheSystem / Cacheable (placement does not change the response).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"openfabrica/providers/llm"
)

// hashedMessage is the canonical projection of one message.
type hashedMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ImageSums  []string       `json:"image_sums,omitempty"`
}

// hashedRequest is the canonical projection of a full completion request.
type hashedRequest struct {
	ProviderType string          `json:"provider_type"`
	Model        string          `json:"model"`
	System       string          `json:"system"`
	Messages     []hashedMessage `json:"messages"`
	Tools        []llm.ToolDef   `json:"tools,omitempty"`
	MaxTokens    int             `json:"max_tokens"`
	Temperature  *float64        `json:"temperature,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
	StopWords    []string        `json:"stop,omitempty"`
	Thinking     *bool           `json:"thinking,omitempty"`
	Effort       string          `json:"effort,omitempty"`
	SchemaName   string          `json:"schema_name,omitempty"`
	Schema       json.RawMessage `json:"schema,omitempty"`
	SchemaStrict bool            `json:"schema_strict,omitempty"`
}

// RequestHash computes the SHA-256 hex digest of the canonical request bytes.
func RequestHash(providerType string, req llm.CompletionRequest) string {
	hr := hashedRequest{
		ProviderType: providerType,
		Model:        req.Model,
		System:       req.System,
		Messages:     make([]hashedMessage, 0, len(req.Messages)),
		Tools:        req.Tools,
		MaxTokens:    req.MaxTokens,
		Temperature:  req.Temperature,
		TopP:         req.TopP,
		StopWords:    req.StopWords,
		Thinking:     req.Thinking,
		Effort:       req.Effort,
	}
	if req.ResponseSchema != nil {
		hr.SchemaName = req.ResponseSchema.Name
		hr.Schema = req.ResponseSchema.Schema
		hr.SchemaStrict = req.ResponseSchema.Strict
	}
	for _, m := range req.Messages {
		hm := hashedMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
		}
		for _, img := range m.Images {
			sum := sha256.Sum256(img.Data)
			hm.ImageSums = append(hm.ImageSums, img.MIMEType+":"+hex.EncodeToString(sum[:]))
		}
		hr.Messages = append(hr.Messages, hm)
	}
	raw, err := json.Marshal(hr)
	if err != nil {
		// Marshalling these shapes cannot realistically fail; an empty hash
		// disables caching for the call rather than panicking a build.
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// requestHasImages reports whether any message carries inline images. Such
// calls are excluded from the replay cache entirely (reads and writes).
func requestHasImages(req llm.CompletionRequest) bool {
	for _, m := range req.Messages {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}
