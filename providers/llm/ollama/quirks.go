// Rejection signatures for the native /api/chat path, registered against the
// shared quirk machinery in providers/llm. The compat path inherits the
// openai-chat rules from the embedded client instead.
package ollama

import (
	"net/http"
	"strings"

	"openfabrica/providers/llm"
)

func init() {
	llm.RegisterQuirkRule("ollama", llm.QuirkRule{
		Cap: llm.CapNativeThinkField,
		// The field is sent on every native call, so there is nothing to guard on
		// beyond the match itself.
		Sent:  func(llm.CompletionRequest) bool { return true },
		Match: matchesThinkRejected,
	})
}

// matchesThinkRejected reports whether err is a daemon or model refusing the
// "think" field. Requires unknown-field wording alongside the name: a bare
// "think" match also caught 400s that merely mentioned thinking for other
// reasons, and silently gave up the reasoning toggle for the whole model.
func matchesThinkRejected(e *llm.Error) bool {
	if e.Status != http.StatusBadRequest && e.Code != llm.CodeBadRequest {
		return false
	}
	low := strings.ToLower(e.Message)
	if !strings.Contains(low, "think") {
		return false
	}
	return strings.Contains(low, "unknown") ||
		strings.Contains(low, "unsupported") ||
		strings.Contains(low, "not supported") ||
		strings.Contains(low, "unrecognized") ||
		strings.Contains(low, "unrecognised") ||
		strings.Contains(low, "invalid") ||
		strings.Contains(low, "does not support")
}
