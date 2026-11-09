package agentrt

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// maxToolNameLen is the shortest tool-name limit among the providers we call.
const maxToolNameLen = 64

// ToolNameForFunction derives a provider-safe tool name from an endpoint path,
// mapping any character outside [A-Za-z0-9_-] to '_' and prefixing so it can't
// collide with a built-in tool (query_data). Shared by the HTTP runtime and the
// manage_agent validation, so the two cannot drift.
func ToolNameForFunction(path string) string {
	var b strings.Builder
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" {
		s = "fn"
	}
	name := "fn_" + s
	if len(name) <= maxToolNameLen {
		return name
	}
	// Too long for a provider: keep a readable prefix and make the rest a hash of
	// the whole path, so two long paths sharing a prefix stay distinct.
	sum := sha256.Sum256([]byte(path))
	suffix := "_" + hex.EncodeToString(sum[:4])
	return name[:maxToolNameLen-len(suffix)] + suffix
}
