package engine

import (
	"strings"
	"testing"

	"openfabrica/internal/stringutil"
	"openfabrica/providers/llm"
)

// roles renders a seed as a compact shape string: role initial plus a marker for
// the cache breakpoint, so an ordering change reads as a diff rather than a
// wall of message structs.
func shape(ms []llm.Message) string {
	var b strings.Builder
	for i, m := range ms {
		if i > 0 {
			b.WriteByte(' ')
		}
		switch m.Role {
		case llm.RoleUser:
			b.WriteByte('u')
		case llm.RoleAssistant:
			b.WriteByte('a')
		default:
			b.WriteByte('?')
		}
		if m.Cacheable {
			b.WriteByte('*')
		}
	}
	return b.String()
}

// TestSeedOrderIsTheContract pins the message order every stage depends on. The
// order is load-bearing for the prompt cache and nothing downstream reports it
// being wrong, so it is asserted here directly.
func TestSeedOrderIsTheContract(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  seed
		want  string
		first string // content prefix of the message after owner
	}{
		{
			name: "expand/plan/scheduled: owner, task, date",
			seed: seed{owner: "o", task: "t"},
			want: "u u u",
		},
		{
			name: "build: shared corpus, owner, stable (the breakpoint), task, date",
			seed: seed{shared: "guides", owner: "o", stable: "intent", task: "instruction"},
			want: "u u u* u u",
		},
		{
			name: "build: owner, stable (the breakpoint), task, date",
			seed: seed{owner: "o", stable: "intent", task: "instruction"},
			want: "u u* u u",
		},
		{
			name: "live: history precedes the task it is being asked to continue",
			seed: seed{owner: "o", history: []llm.Message{{Role: llm.RoleUser, Content: "h1"}, {Role: llm.RoleAssistant, Content: "h2"}}, task: "t"},
			want: "u u a u u",
		},
		{
			name: "plan retry: the answer and its critique follow the task",
			seed: seed{owner: "o", task: "t"}.answered("said", "wrong because"),
			want: "u u a u u",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := shape(tc.seed.messages())
			if got != tc.want {
				t.Errorf("seed shape = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSeedDateIsAlwaysLast: the date is volatile per day, so anything byte-stable
// sitting behind it would be re-billed on a date rollover. Every shape must end
// with it. The head of the seed is the shared corpus when there is one, owner context
// otherwise: blocks run most-shared to least-shared so the warm prefix reaches as far as
// it can.
func TestSeedDateIsAlwaysLast(t *testing.T) {
	for name, s := range map[string]seed{
		"minimal":     {owner: "o"},
		"full":        seed{owner: "o", stable: "s", history: []llm.Message{{Role: llm.RoleUser, Content: "h"}}, task: "t"}.answered("a", "c"),
		"build":       {shared: "guides", owner: "o", stable: "s", task: "t"},
		"noTask":      {owner: "o", stable: "s"},
		"historyOnly": {owner: "o", history: []llm.Message{{Role: llm.RoleUser, Content: "h"}}},
	} {
		ms := s.messages()
		last := ms[len(ms)-1]
		if !strings.Contains(last.Content, dateContextSentinel) {
			t.Errorf("%s: last message is not the date context: %q", name, stringutil.Truncate(last.Content, 60))
		}
		wantHead := s.shared
		if wantHead == "" {
			wantHead = ownerContextSentinel
		}
		if !strings.Contains(ms[0].Content, wantHead) {
			t.Errorf("%s: first message is not the shared/owner head: %q", name, stringutil.Truncate(ms[0].Content, 60))
		}
		// Owner context always precedes the project's own intent, whatever leads.
		ownerAt, stableAt := -1, -1
		for i, m := range ms {
			if strings.Contains(m.Content, ownerContextSentinel) {
				ownerAt = i
			}
			if s.stable != "" && m.Content == s.stable {
				stableAt = i
			}
		}
		if ownerAt < 0 {
			t.Errorf("%s: owner context missing", name)
		}
		if stableAt >= 0 && ownerAt > stableAt {
			t.Errorf("%s: owner context (%d) must precede the session-stable block (%d)", name, ownerAt, stableAt)
		}
	}
}

// TestSeedCacheBreakpointOnlyOnStable: Cacheable is the provider-side breakpoint
// and the engine sets exactly one, on the session-stable prefix. A second one
// would split the cache and re-buy the tail on every turn.
func TestSeedCacheBreakpointOnlyOnStable(t *testing.T) {
	s := seed{owner: "o", stable: "s", history: []llm.Message{{Role: llm.RoleUser, Content: "h"}}, task: "t"}.answered("a", "c")
	n := 0
	for _, m := range s.messages() {
		if m.Cacheable {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("cache breakpoints = %d, want exactly 1 (on stable)", n)
	}
	if !s.messages()[1].Cacheable {
		t.Fatal("the breakpoint must be the stable prefix, immediately after owner context")
	}
}
