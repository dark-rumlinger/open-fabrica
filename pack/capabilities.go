package pack

// Wave is the cache-stability tag stamped into every assembled system prompt. Bump
// it on any meaningful prompt-content change so provider prompt caches treat the new
// prompts as fresh entries instead of serving a stale cached prefix.
const Wave = "engine-wave-339"

// Capabilities returns just the capability packs from a set (for the planner
// catalogue and the manage_guides surface, which are about guides=capabilities).
func Capabilities(ps []Pack) []Pack {
	var out []Pack
	for _, p := range ps {
		if p.Role == RoleCapability {
			out = append(out, p)
		}
	}
	return out
}

// TranscludeContracts substitutes `{{contract:<id>}}` tokens in a body with the
// canonical contract block. Exposed so the recall tool serves rendered bodies
// (the embedded platform packs carry tokens that Assemble expands at build time).
func TranscludeContracts(body string) string {
	return substituteContracts(body)
}
