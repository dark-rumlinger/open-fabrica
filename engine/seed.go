package engine

import (
	"strings"

	"openfabrica/providers/llm"
)

// seed is a session's opening messages. Their ORDER is the prompt-cache contract,
// and this type is the only thing that produces it. Blocks run from most-shared to
// least-shared, so a longer prefix stays warm for a longer time and for more callers:
//
//	shared   byte-stable ACROSS projects (guide cores drawn from the fixed corpus,
//	         tool routing prose, install-scoped capability notes)
//	owner    byte-stable for the project's lifetime
//	stable   byte-stable for this session (the project's own intent); the cache breakpoint
//	history  carried conversation, ahead of the ask that follows it
//	task     this session's instruction
//	followUp a prior answer plus the critique of it (a corrective turn)
//	date     volatile per day, so it goes LAST: nothing byte-stable sits behind it,
//	         and a date rollover cannot invalidate the prefix above.
//
// Every field is optional except owner. history and followUp are not
// interchangeable: history continues a conversation, so it precedes the task;
// followUp is the model's own answer plus the critique, so it can only come after.
type seed struct {
	shared   string
	owner    string
	stable   string
	history  []llm.Message
	task     string
	followUp []llm.Message
}

// messages renders the seed in contract order. Empty fields are skipped, so a
// stage with no shared block, no stable prefix and no history emits owner, task, date.
func (s seed) messages() []llm.Message {
	out := make([]llm.Message, 0, len(s.history)+len(s.followUp)+4)
	if s.shared != "" {
		// No breakpoint here: the one on stable below covers this block as part of its
		// prefix, and spending a second of the provider's four slots would buy nothing.
		out = append(out, llm.Message{Role: llm.RoleUser, Content: s.shared})
	}
	// No owner context (no named owner, no stored preferences): no message, rather
	// than an empty block on every request.
	if strings.TrimSpace(s.owner) != "" {
		out = append(out, ownerContextMessage(s.owner))
	}
	if s.stable != "" {
		// The only user-message cache breakpoint the engine sets. Everything above
		// it is stable for the project or shared beyond it; this is stable for the session.
		out = append(out, llm.Message{Role: llm.RoleUser, Content: s.stable, Cacheable: true})
	}
	out = append(out, s.history...)
	if s.task != "" {
		out = append(out, llm.Message{Role: llm.RoleUser, Content: s.task})
	}
	out = append(out, s.followUp...)
	out = append(out, dateContextMessage())
	return out
}

// answered returns a copy of the seed carrying one corrective turn: what the
// model said, and what is wrong with it. Used by PLAN's retries, which re-ask the
// same task with the previous answer in front of the model.
func (s seed) answered(said, critique string) seed {
	s.followUp = []llm.Message{
		{Role: llm.RoleAssistant, Content: said},
		{Role: llm.RoleUser, Content: critique},
	}
	return s
}
