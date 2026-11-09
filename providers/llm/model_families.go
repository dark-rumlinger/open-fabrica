package llm

import "strings"

// MatchesAnyFamily reports whether the lowercased model id contains any of the
// family substrings. It is the shared discovery-time heuristic loop; each
// transport keeps its own family list, since hosted vendors and local runtimes
// name models differently. Entries must already be lowercase.
func MatchesAnyFamily(model string, families []string) bool {
	low := strings.ToLower(model)
	for _, f := range families {
		if strings.Contains(low, f) {
			return true
		}
	}
	return false
}

// LocalVisionFamilies flags vision-capable models by name for the local-model
// runtimes (Ollama, llama.cpp). One list because both serve the same GGUF model
// ecosystem, so a family one runtime recognizes must not be invisible to the
// other. "-vl" covers the whole Qwen-VL line, so no per-version entries.
// Coarse on purpose: discovery cannot see whether a projector is actually
// loaded, and admins can flip the per-model flag when the heuristic is wrong.
// Hosted vendors (OpenAI, Gemini) keep their own lists; theirs is a different
// namespace.
var LocalVisionFamilies = []string{"llava", "-vl", "vision", "gemma-3", "gemma3", "minicpm-v", "pixtral", "mmproj", "moondream"}
