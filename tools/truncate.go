package tools

import (
	"fmt"

	"openfabrica/internal/stringutil"
)

// defaultMaxResult caps a tool result whose ToolDef declares no MaxResult.
const defaultMaxResult = 3000

// TruncateResultAt caps a body at maxSize bytes, appending a note naming the
// original size so the model knows content was elided.
func TruncateResultAt(result string, maxSize int) string {
	origLen := len(result)
	out := stringutil.Truncate(result, maxSize)
	if out != result {
		return out + fmt.Sprintf("\n[truncated: showed first ~%d of %d bytes, narrow the arguments (a filter, a smaller limit, a line range) to see the rest]", maxSize, origLen)
	}
	return out
}

// TruncateResult caps a tool result body for a transcript, honoring the tool's
// declared MaxResult. Shared by every LLM surface that appends results, so the
// caps cannot drift between the engine and chat.
func TruncateResult(reg *Registry, toolName, result string) string {
	maxSize := defaultMaxResult
	if reg != nil {
		if def, err := reg.Get(toolName); err == nil && def.MaxResult > 0 {
			maxSize = def.MaxResult
		}
	}
	return TruncateResultAt(result, maxSize)
}
