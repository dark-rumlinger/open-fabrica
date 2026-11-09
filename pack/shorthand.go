package pack

import (
	"fmt"
	"strings"
)

// Shorthand is one bare capability name the prompts use in prose ("a `create_api`
// endpoint", "any `create_auth` project") and the registered tool actions that realize
// it. Plans and checklists speak in these names, so the model must be told what each
// one calls; a shorthand is never itself an action verb.
type Shorthand struct {
	Name  string
	Tools [][2]string // {tool, action} pairs; several when more than one tool realizes it
}

// Shorthands is the single table of capability shorthands. The mental model renders
// it as a glossary (shorthandGlossaryToken), and TestDocsShorthandVerbsResolve checks
// every pair against the tool registry and every backticked shorthand in the docs
// against this table, so the prose, the glossary and the registry cannot drift apart.
// Ordered as the glossary prints it.
var Shorthands = []Shorthand{
	{"create_api", [][2]string{{"manage_api", "create"}}},
	{"enable_query", [][2]string{{"manage_api", "enable_query"}}},
	{"create_auth", [][2]string{{"manage_auth", "create"}}},
	{"create_oauth", [][2]string{{"manage_oauth", "create"}}},
	{"create_stream", [][2]string{{"manage_sse", "create"}}},
	{"create_websocket", [][2]string{
		{"manage_relay", "create"}, {"manage_tick", "create"},
		{"manage_authoritative", "create"}, {"manage_matchmaking", "create"},
		{"manage_signaling", "create"},
	}},
	{"set_reducer", [][2]string{{"manage_authoritative", "set_reducer"}}},
	{"create_table", [][2]string{{"manage_schema", "create_table"}}},
	{"alter_table", [][2]string{{"manage_schema", "alter_table"}}},
	{"add_index", [][2]string{{"manage_schema", "add_index"}}},
	{"enable_pwa", [][2]string{{"manage_project", "enable_pwa"}}},
	{"set_meta", [][2]string{{"manage_seo", "set_meta"}}},
	{"set_robots", [][2]string{{"manage_seo", "set_robots"}}},
	{"test_endpoint", [][2]string{{"manage_testing", "test_endpoint"}}},
	{"test_url", [][2]string{{"manage_testing", "test_url"}}},
	{"test_page", [][2]string{{"manage_testing", "test_page"}}},
}

// shorthandGlossaryToken is replaced in the mental model with ShorthandGlossary.
const shorthandGlossaryToken = "{{SHORTHAND_GLOSSARY}}"

// ShorthandGlossary renders the capability shorthands the plans, checklists and guides
// use, each with the call it stands for. Only shorthands that are NOT already a tool's
// own action verb are listed: `create_table` is `manage_schema action=create_table`
// verbatim and needs no gloss. Deterministic (table order).
func ShorthandGlossary() string {
	var parts []string
	for _, s := range Shorthands {
		if len(s.Tools) == 1 && s.Tools[0][1] == s.Name {
			continue
		}
		if len(s.Tools) == 1 {
			parts = append(parts, fmt.Sprintf("`%s` = %s %s", s.Name, s.Tools[0][0], s.Tools[0][1]))
			continue
		}
		parts = append(parts, fmt.Sprintf("`%s` = create on the locked realtime tool", s.Name))
	}
	return "Shorthand names (capabilities, never `action` verbs): " + strings.Join(parts, "; ") + "."
}
