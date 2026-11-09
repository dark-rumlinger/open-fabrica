package engine

// Doc<->registry consistency guard.
//
// The build loop feeds the model the embedded packs (knowledge/) and the
// contract blocks transcluded into them; both name tools and `action=` verbs the
// model then calls, and nothing else checks those stay in sync with the
// reflection-defined tool registry. These tests parse the shipped docs and
// assert every manage_*/web_search/find_tool token is a registered tool, and
// every `tool ... action=verb` reference names a real action of that tool.
//
// Scope note: the `action=` form (the engine's literal call syntax) is
// validated line-by-line against the nearest tool token. Space-form prose like
// "manage_api create" is intentionally not checked, to keep false positives at
// zero. Bare backticked shorthands with no tool on the line (`create_api`,
// `set_reducer`, ...) are covered separately by TestDocsShorthandVerbsResolve
// via the explicit shorthandActions table at the bottom of this file.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"openfabrica/contracts"
	"openfabrica/knowledge"
	"openfabrica/pack"
	"openfabrica/tools"

	// Blank-import every tool subpackage so their init() registers each tool
	// into the default registry. Without these the registry is empty and every
	// doc reference would false-flag as unregistered. Mirrors main.go's set.
	_ "openfabrica/tools/all" // register every tool category via init()
)

// TestNoToolRegistrationErrors pins, in CI, that every blank-imported tool
// subpackage registered cleanly via init() and that the default registry passes
// the same structural Validate main() runs at boot. Engine fixtures register
// into a Clone (testToolRegistry), so the shared catalogue stays production-shaped.
func TestNoToolRegistrationErrors(t *testing.T) {
	for _, e := range tools.RegistrationErrors() {
		t.Errorf("tool registration error: %v", e)
	}
	if err := tools.DefaultRegistry().Validate(); err != nil {
		t.Fatal(err)
	}
}

// knownExtraTools are real tools the manage_* scan won't surface on its own:
// find_tool is the built-in discovery meta-tool the prompts reference by name.
var knownExtraTools = []string{"find_tool"}

// toolToken matches a tool name as it appears in prose: the manage_* family
// plus the find_tool meta-tool the docs reference by name (the only tool that
// does not carry the manage_ prefix).
var toolToken = regexp.MustCompile(`\b(manage_[a-z0-9_]+|find_tool)\b`)

// actionRef matches an `action=verb` reference (optionally quoted). The \b
// keeps it from matching inside words like "transaction=". RE2 has no
// backreferences, so a quoted value is matched leniently (the closing quote is
// not required to balance) - fine, since only the verb capture is used.
var actionRef = regexp.MustCompile(`\baction="?([a-z0-9_]+)`)

// registryTools returns the set of registered tool names and, per tool, the set
// of its valid action names (from ActionMeta, the same metadata find_tool
// renders). Fails loudly if the registry looks unpopulated, so a broken
// blank-import set can never make these tests pass vacuously.
func registryTools(t *testing.T) (names map[string]bool, actions map[string]map[string]bool) {
	t.Helper()
	names = map[string]bool{}
	actions = map[string]map[string]bool{}
	for _, td := range tools.DefaultRegistry().List() {
		names[td.Name] = true
		as := make(map[string]bool, len(td.ActionMeta))
		for a := range td.ActionMeta {
			as[a] = true
		}
		actions[td.Name] = as
	}
	if len(names) < 20 {
		t.Fatalf("registry looks unpopulated (%d tools) - are the tool subpackages imported?", len(names))
	}
	for _, n := range knownExtraTools {
		names[n] = true
	}
	return names, actions
}

// docSources returns every doc body to scan, keyed by a human-readable label:
// every embedded pack (stage/capability/path/fragment), every contract block
// (transcluded into packs at assembly, so its tool references must be real too),
// and the mental model (it rides every prompt but is not a pack, so pack.Embedded
// never sees it).
func docSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("loading embedded packs: %v", err)
	}
	for _, p := range ps {
		out["pack/"+p.Role+"/"+p.Slug] = p.Body
	}
	cs, err := contracts.List()
	if err != nil {
		t.Fatalf("loading contracts: %v", err)
	}
	for _, c := range cs {
		out["contract/"+c.ID] = c.Body
	}
	mm, err := knowledge.MentalModel()
	if err != nil {
		t.Fatalf("loading mental model: %v", err)
	}
	out["knowledge/mental-model.md"] = mm
	return out
}

// TestDocsReferenceRealTools asserts every manage_*/web_search/find_tool token
// in the prompts and guides is a registered tool.
func TestDocsReferenceRealTools(t *testing.T) {
	known, _ := registryTools(t)
	for label, body := range docSources(t) {
		seen := map[string]bool{}
		for _, m := range toolToken.FindAllString(body, -1) {
			if known[m] || seen[m] {
				continue
			}
			seen[m] = true
			t.Errorf("%s: references tool %q, which is not a registered tool", label, m)
		}
	}
}

// TestDocsReferenceRealActions asserts every `tool ... action=verb` reference
// names a real action of that tool. For each action= match, the nearest tool
// token earlier on the same line is taken as the subject; references with no
// tool on the line (verb implied by surrounding prose) are not checked.
func TestDocsReferenceRealActions(t *testing.T) {
	known, actions := registryTools(t)
	for label, body := range docSources(t) {
		for i, line := range strings.Split(body, "\n") {
			for _, loc := range actionRef.FindAllStringSubmatchIndex(line, -1) {
				verb := line[loc[2]:loc[3]]
				tool := lastToolBefore(line[:loc[0]])
				if tool == "" || !known[tool] {
					continue // unattributable, or unknown tool (the other test catches it)
				}
				if valid := actions[tool]; valid != nil && !valid[verb] {
					t.Errorf("%s:%d: %q action=%q is not a real action of %s (valid: %s)",
						label, i+1, tool, verb, tool, sortedKeys(valid))
				}
			}
		}
	}
}

// lastToolBefore returns the last tool token in s, or "" if there is none.
func lastToolBefore(s string) string {
	m := toolToken.FindAllString(s, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}

func sortedKeys(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

// --- Tool arg-tag vs prose consistency -------------------------------------
//
// The tests above check the prompts/guides against the registry; these check the
// tool source itself: that each action arg's `desc` prose agrees with the
// `arg`/`default` tags, the only faithful source of truth for required-ness and
// defaults (merged tool-level Params force per-action Required to false). They
// guard: a desc that opens "Required" while the tag leaves the arg optional, and
// a desc restating a "default <value>" that disagrees with the tag. Conditional
// phrasings and behavioral defaults on untagged args are intentionally not
// flagged, to keep false positives at zero.

// argTagField is one parsed action arg: its tag-declared name/required/default
// and its LLM-facing desc prose.
type argTagField struct {
	file     string // path relative to tools/, slash-separated
	line     int
	arg      string
	required bool
	desc     string
	def      string
	hasDef   bool
	maxTag   string
	hasMax   bool
	numeric  bool // Go field type is an int/uint/float kind (pointers followed)
}

// toolArgFields parses every non-test .go file under the sibling tools/ tree and
// returns one entry per `arg`-tagged struct field. Fails loudly if the scan
// finds implausibly few fields, so a broken path can never make these tests pass
// vacuously (mirrors registryTools' unpopulated-registry guard).
func toolArgFields(t *testing.T) []argTagField {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the tools source tree")
	}
	toolsDir := filepath.Join(filepath.Dir(thisFile), "..", "tools")

	var out []argTagField
	walkErr := filepath.WalkDir(toolsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, _ := filepath.Rel(toolsDir, path)
		rel = filepath.ToSlash(rel)
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil {
					continue
				}
				tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
				argv, ok := tag.Lookup("arg")
				if !ok {
					continue
				}
				parts := strings.Split(argv, ",")
				af := argTagField{
					file: rel,
					line: fset.Position(field.Pos()).Line,
					arg:  strings.TrimSpace(parts[0]),
					desc: tag.Get("desc"),
				}
				for _, m := range parts[1:] {
					if strings.TrimSpace(m) == "required" {
						af.required = true
					}
				}
				af.def, af.hasDef = tag.Lookup("default")
				af.maxTag, af.hasMax = tag.Lookup("max")
				af.numeric = isNumericFieldType(field.Type)
				out = append(out, af)
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("scanning tools source: %v", walkErr)
	}
	if len(out) < 50 {
		t.Fatalf("scanned only %d arg fields under %s - did the scan path break?", len(out), toolsDir)
	}
	return out
}

// numericTypeNames are the Go type idents min/max tags can bound.
var numericTypeNames = map[string]bool{
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true,
}

// isNumericFieldType reports whether an AST field type is a numeric kind,
// following one pointer level - the same shapes parseBoundTag accepts.
func isNumericFieldType(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	id, ok := expr.(*ast.Ident)
	return ok && numericTypeNames[id.Name]
}

// requiredConditionalPrefixes are desc openers where "required" is conditional,
// so the static `,required` flag legitimately cannot express it. Not flagged.
var requiredConditionalPrefixes = []string{
	"required when", "required if", "required unless",
	"required only", "required for", "required to", "required in",
}

// TestToolRequiredDescMatchesTag flags an arg whose desc opens with "Required"
// (an unconditional claim) while the tag leaves it optional - the schema would
// then advertise the arg as optional and the model could omit it.
func TestToolRequiredDescMatchesTag(t *testing.T) {
	for _, f := range toolArgFields(t) {
		lc := strings.ToLower(strings.TrimSpace(f.desc))
		if f.required || !strings.HasPrefix(lc, "required") {
			continue
		}
		conditional := false
		for _, c := range requiredConditionalPrefixes {
			if strings.HasPrefix(lc, c) {
				conditional = true
				break
			}
		}
		if conditional {
			continue
		}
		t.Errorf("tools/%s:%d: arg %q desc opens with %q but the tag is not marked ,required - the JSON schema advertises it as optional. Mark the arg ,required or soften the desc.",
			f.file, f.line, f.arg, firstWords(f.desc, 4))
	}
}

var (
	descDefaultWord = regexp.MustCompile(`(?i)\bdefault\b`)
	descTokenRe     = regexp.MustCompile(`[A-Za-z0-9.]+`)
)

// TestToolDefaultDescMatchesTag flags an arg that carries a `default:` tag whose
// desc also makes a "default <value>" claim that disagrees with the tag value.
// Because Parameters() surfaces the tag default to the model, the value should
// live in the tag alone; a restatement that drifts is the failure mode. A desc
// with no "default" word, or a behavioral default on an untagged arg, is fine.
func TestToolDefaultDescMatchesTag(t *testing.T) {
	for _, f := range toolArgFields(t) {
		if !f.hasDef || !descDefaultWord.MatchString(f.desc) {
			continue
		}
		tokens := map[string]bool{}
		for _, tok := range descTokenRe.FindAllString(strings.ToLower(f.desc), -1) {
			tokens[tok] = true
		}
		for _, part := range strings.Split(strings.ToLower(strings.TrimSpace(f.def)), ",") {
			part = strings.TrimSpace(part)
			if part == "" || tokens[part] {
				continue
			}
			t.Errorf("tools/%s:%d: arg %q has default:%q but its desc says \"default\" without stating %q (prose/tag drift). Keep the default in the tag only, or make the prose match.",
				f.file, f.line, f.arg, f.def, part)
			break
		}
	}
}

// descCapRe extracts a numeric cap stated in desc prose ("capped at 200", "up
// to 500", "max 1000", "clamped to 1-30000"), used to assert the prose cap
// agrees with the `max:` tag, which Parameters() renders as the schema
// maximum. A "clamped to N-M" range takes its cap from the SECOND number - the
// alternation matches the whole "1-30000" and captures only the upper end.
var descCapRe = regexp.MustCompile(`(?i)(?:capped at|cap of|up to|clamped to(?:\s*[0-9]+\s*-)?|max(?:imum)?(?: of)?)\s*([0-9]+)`)

// TestToolCapDescMatchesTag flags an arg that carries a `max:` tag whose desc
// also states a numeric cap that disagrees with the tag value. The cap is the
// runtime-enforced bound and the schema maximum, so a drifting restatement in
// the prose is the failure mode this guards. A desc with no cap phrase is fine.
func TestToolCapDescMatchesTag(t *testing.T) {
	for _, f := range toolArgFields(t) {
		if !f.hasMax {
			continue
		}
		want := strings.TrimSpace(f.maxTag)
		for _, m := range descCapRe.FindAllStringSubmatch(f.desc, -1) {
			if m[1] != want {
				t.Errorf("tools/%s:%d: arg %q has max:%q but its desc states a cap of %q (prose/tag drift). Reconcile the prose cap with the max tag.",
					f.file, f.line, f.arg, f.maxTag, m[1])
			}
		}
	}
}

// proseCapWithoutTagExemptions lists (file, arg) pairs whose desc states a
// numeric cap on purpose WITHOUT a `max:` tag: the handler REJECTS an
// out-of-range value with a specific teaching message, and a bound tag would
// pre-empt it with only the generic bound message. Every entry must name a
// handler-enforced bound; a plain numeric cap belongs in the tag.
var proseCapWithoutTagExemptions = map[string]bool{
	"api/manage_tick.go|max_msg_size":         true, // rejects with DataChannel guidance
	"api/manage_relay.go|max_msg_size":        true, // rejects with DataChannel guidance
	"api/manage_signaling.go|max_msg_size":    true, // rejects with DataChannel guidance
	"api/manage_matchmaking.go|max_msg_size":  true, // rejects with DataChannel guidance
	"api/manage_function.go|timeout_ms":       true, // rejects with the background-job guidance (timeoutCeilingMsg)
	"api/manage_relay.go|message_ttl_seconds": true, // rejects with the 1-year teaching message
	"api/manage_tick.go|message_ttl_seconds":  true, // rejects with the 1-year teaching message
}

// TestToolProseCapHasTag flags a numeric arg whose desc states a cap while the
// field carries no `max:` tag: the schema advertises no bound and nothing
// enforces it, so the stated cap is either enforced by hidden handler code
// (move it to the tag) or not enforced at all (fix the prose). Reject-style
// bounds are exempted above; string and array args are out of scope.
func TestToolProseCapHasTag(t *testing.T) {
	for _, f := range toolArgFields(t) {
		if !f.numeric || f.hasMax || !descCapRe.MatchString(f.desc) {
			continue
		}
		if proseCapWithoutTagExemptions[f.file+"|"+f.arg] {
			continue
		}
		t.Errorf("tools/%s:%d: arg %q states a cap in its desc (%q) but has no max tag. Add the tag (framework bound-check) or exempt it as a reject-style bound.",
			f.file, f.line, f.arg, firstWords(f.desc, 12))
	}
}

// firstWords returns the first n whitespace-separated words of s, for compact
// error messages.
func firstWords(s string, n int) string {
	fields := strings.Fields(s)
	if len(fields) > n {
		fields = fields[:n]
	}
	return strings.Join(fields, " ")
}

// --- Bare shorthand verbs in prose ------------------------------------------
//
// The prompts and guides also use bare capability shorthands with no tool on
// the line; the scope note at the top of this file excludes those from the
// action= check, so a renamed action would strand them silently.
// shorthandActions is the explicit contract: every bare shorthand the docs use,
// mapped to the (tool, action) pair(s) that realize it. A shorthand several
// tools realize lists every pair, so a rename in ANY of them surfaces.

// shorthandActions maps each documented bare shorthand to the registered
// (tool, action) pairs that realize it. The table itself is pack.Shorthands - the same
// one the mental model renders as the model's glossary - so the test and the prompt
// cannot disagree about what a shorthand means.
var shorthandActions = func() map[string][][2]string {
	out := make(map[string][][2]string, len(pack.Shorthands))
	for _, s := range pack.Shorthands {
		out[s.Name] = s.Tools
	}
	return out
}()

// shorthandRe matches a bare backticked capability shorthand in doc prose: a
// verb from the closed shorthand grammar plus a snake_case object. Backtick-
// anchored so ordinary prose and arg names outside code spans never match.
var shorthandRe = regexp.MustCompile("`((?:create|enable|set|add|test|alter)_[a-z0-9_]+)`")

// TestDocsShorthandVerbsResolve guards the bare shorthands in three directions:
// every mapped pair names a real registered tool + action (an action rename
// fails here and points at the prose to update); every backticked shorthand the
// shipped docs use is in the table (a new shorthand cannot ship unmapped); and
// every table entry is still used by some doc (the table cannot rot).
func TestDocsShorthandVerbsResolve(t *testing.T) {
	known, actions := registryTools(t)

	for sh, pairs := range shorthandActions {
		for _, p := range pairs {
			tool, verb := p[0], p[1]
			if !known[tool] {
				t.Errorf("shorthand %q maps to unregistered tool %q - update shorthandActions", sh, tool)
				continue
			}
			if valid := actions[tool]; valid != nil && !valid[verb] {
				t.Errorf("shorthand %q maps to %s action=%q, which is not a real action (valid: %s) - the docs' shorthand has drifted; fix the prose and this table together",
					sh, tool, verb, sortedKeys(valid))
			}
		}
	}

	used := map[string]bool{}
	for label, body := range docSources(t) {
		for _, m := range shorthandRe.FindAllStringSubmatch(body, -1) {
			sh := m[1]
			used[sh] = true
			if _, ok := shorthandActions[sh]; !ok {
				t.Errorf("%s: uses bare shorthand `%s` with no entry in shorthandActions - add its (tool, action) mapping, or rephrase with the explicit tool + action= form",
					label, sh)
			}
		}
	}
	for sh := range shorthandActions {
		if !used[sh] {
			t.Errorf("shorthandActions maps %q but no shipped doc uses it - drop the stale entry", sh)
		}
	}
}
