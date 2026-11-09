package tools

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"openfabrica/providers/llm"
)

// RoutingProse renders the when-to-use guidance for an explicit set of tools, one block
// per tool, in the order given (unknown / gated names are dropped).
//
// It exists because a PRELOADED tool's WhenToUse otherwise never reaches the model:
// LookupFull hands the provider a wire schema described only by the <=130-char
// OneLiner, and LookupDocs (the only path that emits WhenToUse) runs exclusively
// inside the find_tool handler, which the ground rules forbid for the preloaded core
// set.
//
// It carries SchemaNotes for the same reason, and because they correct what the wire
// schema claims: the per-action required args and the enum/bound narrowing a merged
// union schema cannot express are emitted only by LookupDocs, so without them the
// model reads manage_data.query's max of 500 as manage_data.search's cap and only
// learns otherwise from a silently truncated result. ~2.8KB for the preloaded set, on
// the cached prefix, paid once per build.
//
// Left out on purpose: examplesDoc (twice the size, and a call example helps least for
// the six tools the model already calls all build long) and the one-liner and action
// list (they ride the tool's wire schema in the same request, so repeating them would
// bill the same bytes twice).
//
// Deterministic: same names in, same bytes out, so a caller can place it on a
// cache-stable prompt prefix.
func (r *Registry) RoutingProse(names []string, caps llm.Capabilities, stage string) string {
	defs := r.resolveAvailable(names, caps, stage)
	var b strings.Builder
	for _, t := range defs {
		when := strings.TrimSpace(t.WhenToUse)
		notes := t.SchemaNotes()
		if when == "" && notes == "" {
			continue
		}
		fmt.Fprintf(&b, "### `%s`\n", t.Name)
		for _, part := range []string{when, notes} {
			if part != "" {
				b.WriteString(part + "\n")
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// examplesDoc renders only the per-action call examples for one tool, one line per example.
// The schema already carries the action enum and summaries, and ToolDoc.WhenToUse the
// routing prose, so re-listing them would duplicate; keeping only the examples gives the
// model the one thing the schema lacks (concrete arg shapes) at the lowest token cost.
func (d *ToolDef) examplesDoc() string {
	var b strings.Builder
	for _, name := range d.actionNames() {
		meta := d.ActionMeta[name]
		for _, ex := range meta.Examples {
			body, _ := json.Marshal(ex.Args)
			if ex.Title != "" {
				fmt.Fprintf(&b, "- `%s.%s` example - %s: `%s`\n", d.Name, name, ex.Title, string(body))
			} else {
				fmt.Fprintf(&b, "- `%s.%s` example: `%s`\n", d.Name, name, string(body))
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// enumNarrowingDoc lists, per action, the args whose accepted enum is narrower than the
// tool-level union schema. Parameters() merges enums across actions (a deliberate widening),
// so without this note the model can pick an enum value the wire schema advertises but the
// chosen action rejects, burning a turn. Empty when no action narrows anything.
func (d *ToolDef) enumNarrowingDoc() string {
	unionEnums := map[string]int{}
	for _, p := range d.Params {
		if len(p.Enum) > 0 {
			unionEnums[p.Name] = len(p.Enum)
		}
	}
	var b strings.Builder
	for _, name := range d.actionNames() {
		var parts []string
		for _, p := range d.ActionMeta[name].Params {
			if len(p.Enum) > 0 && len(p.Enum) < unionEnums[p.Name] {
				parts = append(parts, fmt.Sprintf("%s=[%s]", p.Name, strings.Join(p.Enum, ", ")))
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "- `%s.%s` accepts only: %s\n", d.Name, name, strings.Join(parts, "; "))
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n" + b.String()
}

// requiredByActionDoc lists, per action, the args that action REQUIRES.
//
// Parameters() cannot express this: one tool exposes many actions through a single union
// schema, so Define deliberately marks every merged param optional and leaves `action` as
// the only required key (see define.go). Requiredness is still enforced, per action, in the
// handler - but the model does not read the handler. Without this note the only way it can
// learn that `manage_files.restore` needs `kind`, or that `manage_data.delete` needs `id`
// while `update` does not, is to spend a turn getting a missing_arg back.
//
// Empty when no action requires anything beyond `action`.
func (d *ToolDef) requiredByActionDoc() string {
	var parts []string
	for _, name := range d.actionNames() {
		var req []string
		for _, p := range d.ActionMeta[name].Params {
			if p.Required {
				req = append(req, p.Name)
			}
		}
		if len(req) > 0 {
			parts = append(parts, fmt.Sprintf("%s(%s)", name, strings.Join(req, ", ")))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "\nRequired per action (the schema marks only `action`): " + strings.Join(parts, "; ") + "\n"
}

// boundNarrowingDoc lists, per action, the numeric args whose real cap or default differs
// from what the union schema advertises.
//
// mergeParam keeps the FIRST action's bound and default when actions disagree (see
// define.go), which is the safe choice for a merged schema but leaves the model reading one
// action's numbers for another's arg: manage_data.search clamps `limit` at 200 while the
// advertised maximum is query's 500, so a search for 400 rows silently returns 200 and the
// model has no way to know it was truncated. enumNarrowingDoc closes exactly this gap for
// enums; this is its numeric twin. Empty when every action agrees.
func (d *ToolDef) boundNarrowingDoc() string {
	union := make(map[string]Param, len(d.Params))
	for _, p := range d.Params {
		union[p.Name] = p
	}
	var b strings.Builder
	for _, name := range d.actionNames() {
		var parts []string
		for _, p := range d.ActionMeta[name].Params {
			u, ok := union[p.Name]
			if !ok {
				continue
			}
			// The union carries a bound or default only when every action agrees on it
			// (reconcileUnionParam), so an action-specific value is listed here whenever
			// the union shows none or a different one.
			if p.HasMax && (!u.HasMax || p.Max != u.Max) {
				parts = append(parts, fmt.Sprintf("%s max %s", p.Name, trimFloat(p.Max)))
			}
			if p.HasMin && (!u.HasMin || p.Min != u.Min) {
				parts = append(parts, fmt.Sprintf("%s min %s", p.Name, trimFloat(p.Min)))
			}
			if p.HasDefault && (!u.HasDefault || p.Default != u.Default) {
				parts = append(parts, fmt.Sprintf("%s defaults to %s", p.Name, p.Default))
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "- `%s.%s`: %s\n", d.Name, name, strings.Join(parts, "; "))
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\nPer-action bounds and defaults (the merged schema shows only values every action shares):\n" + b.String()
}

// trimFloat renders a bound the way it was authored: 200 not 200.000000.
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// SchemaNotes returns everything the union wire schema cannot say about a tool: which args
// each action requires, which actions narrow a shared enum, and which narrow a shared
// numeric bound or default. One home for the three, so every surface that hands a model a
// tool's schema can hand it the caveats too.
func (d *ToolDef) SchemaNotes() string {
	return strings.TrimSpace(d.requiredByActionDoc() + d.enumNarrowingDoc() + d.boundNarrowingDoc())
}
