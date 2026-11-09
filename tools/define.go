package tools

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Action is the struct-per-action interface. Implement it on a struct whose
// exported fields carry the per-action JSON arg schema via `arg`/`desc`/`enum`
// tags; tools.Define() reflects over the struct to derive the schema, the
// LLM-facing Summary (from Doc()), the example call (from Example()), and the
// dispatch handler (Run gets the struct fully populated from args).
//
// Required methods: Doc, Run.
// Optional methods, detected via interface assertion at registration time:
//
//	ActionName() string   - override the action name (default: derived from struct name).
//	                        Use when the struct has a "Name" arg field, or the derived
//	                        name doesn't match the desired verb.
//	ReadOnly() bool       - mark the action read-only for the build loop streak detector
//	Example() interface{} - return a *Self populated with realistic sample args
//	ItemsSchema(arg string) map[string]any - full JSON Schema for the elements of an
//	                        array-of-objects arg (properties, required), which the Go
//	                        element type cannot express. nil keeps the default.
//
// The receiver passed to Run is always a freshly-allocated *T populated from the
// call's args; the value in Tool.Actions is only a type-carrier at registration.
type Action interface {
	Doc() string
	Run(ctx *ToolContext) (*Result, error)
}

type actionNamer interface{ ActionName() string }
type actionReadOnly interface{ ReadOnly() bool }
type actionExampler interface{ Example() any }
type actionItemsSchemer interface {
	ItemsSchema(arg string) map[string]any
}

// Tool is the declarative shape every tool registers itself with via Define().
// Per-action information (schema, summary, example, handler) lives on the
// Action structs in the Actions slice; tool-level metadata (catalogue routing,
// runtime hooks, capability gating) lives on Tool itself. Define() reflects
// this into the registry's ToolDef record.
type Tool struct {
	Name      string
	Category  string
	OneLiner  string
	WhenToUse string

	Actions []Action

	// Tool-level hooks, identical semantics to the ToolDef fields with the
	// same names. Kept here so a Tool definition is one self-contained literal.
	Summarize          func(result string) string
	Hints              func(action string, args map[string]any, result *Result) string
	RequestsPause      func(action string, args map[string]any) bool
	MaxResult          int
	FullResultInEvents bool
	RequiresCapability string

	// MaxDuration overrides the caller-supplied tool-execution timeout. Zero
	// means use the caller's default (the engine pipeline applies 2m). Set this
	// only when a tool legitimately needs longer (image generation, headless
	// browser, large PDF render) - slower tools still consume a stage's wall
	// budget, so don't reach for big values lightly.
	MaxDuration time.Duration

	// Stages, when non-empty, restricts LLM exposure of this tool to the
	// listed pipeline stages (lowercase, e.g. "build", "live").
	// An empty/nil slice means the tool is available in every stage that
	// renders the catalogue (BUILD and LIVE today). Use this for tools that
	// would burn budget if invoked during BUILD (active probes, broad
	// destructive escape hatches) but are legitimate during LIVE.
	Stages []string

	// Probe marks a tool that observes the running project (loads a page, hits an
	// endpoint, reads runtime errors) without mutating any artifact. The build
	// loop's repeat guard treats its failures as stale once a write elsewhere
	// succeeds, since the write may have fixed what the probe saw.
	Probe bool

	// RealtimeModel names the realtime wire model this tool creates ("relay",
	// "tick", "authoritative", "matchmaking", "webrtc"), matching the plan's
	// realtime-model decision. When a plan locks one model, the build preloads
	// only the tool whose RealtimeModel matches it. Empty for everything else.
	RealtimeModel string
}

// Define is the single registration entry point. It reflects each Action
// into a (name, params, summary, examples) tuple and inserts the equivalent
// ToolDef into the default registry. Errors are reported through
// recordRegistrationError so main() can surface every problem at boot.
func Define(t Tool) {
	defineInto(defaultRegistry, t, callerFile(), recordRegistrationError)
}

// Define registers t into this registry rather than the default one, returning
// every registration problem joined. Tests use it on a Clone of the default
// registry so fixture tools never leak into the shared catalogue.
func (r *Registry) Define(t Tool) error {
	var errs []error
	defineInto(r, t, callerFile(), func(err error) { errs = append(errs, err) })
	return errors.Join(errs...)
}

// callerFile returns the source file of Define's caller (two frames up), so a
// ToolDef records where it was declared and a guard test can hold the
// file/Name/Category convention.
func callerFile() string {
	_, file, _, ok := runtime.Caller(2)
	if !ok {
		return ""
	}
	return file
}

func defineInto(reg *Registry, t Tool, source string, report func(error)) {
	if t.Name == "" {
		report(fmt.Errorf("tools.Define: empty Name"))
		return
	}
	if len(t.Actions) == 0 {
		report(fmt.Errorf("tools.Define: %s has no Actions", t.Name))
		return
	}

	actionNames := make([]string, 0, len(t.Actions))
	actionsMap := make(map[string]ActionHandler, len(t.Actions))
	metaMap := make(map[string]ActionMeta, len(t.Actions))
	perActionFields := make(map[string][]actionFieldInfo, len(t.Actions))
	mergedParamSet := map[string]Param{}
	mergedOrder := []string{}

	for _, a := range t.Actions {
		if a == nil {
			report(fmt.Errorf("tools.Define %s: nil action in slice", t.Name))
			return
		}
		actionT := reflect.TypeOf(a)
		name := actionNameFor(t.Name, a, actionT)
		if name == "" {
			report(fmt.Errorf("tools.Define %s: cannot derive action name for %s", t.Name, actionT))
			return
		}
		if _, dup := actionsMap[name]; dup {
			report(fmt.Errorf("tools.Define %s: duplicate action name %q", t.Name, name))
			return
		}

		fields, err := extractActionFields(actionT)
		if err != nil {
			report(fmt.Errorf("tools.Define %s/%s: %w", t.Name, name, err))
			return
		}
		perActionFields[name] = fields

		// Synthesize an example: prefer Example() if implemented, else a bare
		// {"action": name} call. The validator below rejects examples that
		// don't satisfy required-ness for the action, so an empty example for
		// an action with required fields will fail registration loudly.
		var exampleArgs map[string]any
		if ex, ok := a.(actionExampler); ok {
			exampleArgs = argsFromStruct(name, ex.Example(), fields)
		} else {
			exampleArgs = map[string]any{"action": name}
		}

		readOnly := false
		if ro, ok := a.(actionReadOnly); ok {
			readOnly = ro.ReadOnly()
		}

		// Drift guard: an Example() that omits a required field is a
		// registration-time mistake, not a runtime one. Catch it here so the
		// boot loop fails alongside other registry validation.
		paramsForActionCheck := fieldsToParams(actionT, fields)
		if is, ok := a.(actionItemsSchemer); ok {
			for i := range paramsForActionCheck {
				if paramsForActionCheck[i].Type != "array" {
					continue
				}
				if items := is.ItemsSchema(paramsForActionCheck[i].Name); items != nil {
					paramsForActionCheck[i].Items = items
				}
			}
		}
		if r := validateActionArgs(paramsForActionCheck, exampleArgs); r != nil {
			report(fmt.Errorf("tools.Define %s/%s: example fails schema: %s", t.Name, name, r.Error))
		}

		metaMap[name] = ActionMeta{
			Summary:  strings.TrimSpace(a.Doc()),
			ReadOnly: readOnly,
			Examples: []Example{{Args: exampleArgs}},
			Params:   paramsForActionCheck,
		}

		// Build the per-action handler closure: validate, populate, run.
		paramsForAction := paramsForActionCheck
		actionsMap[name] = makeActionHandler(actionT, fields, paramsForAction)

		// Merge params into the tool-level union schema for Parameters().
		// Tool-level Required stays false; required-ness is enforced
		// per-action by the handler closure above.
		for _, p := range paramsForAction {
			if _, seen := mergedParamSet[p.Name]; !seen {
				mergedOrder = append(mergedOrder, p.Name)
			}
			merged := p
			merged.Required = false
			mergedParamSet[p.Name] = mergeParam(mergedParamSet[p.Name], merged)
		}
		actionNames = append(actionNames, name)
	}

	sort.Strings(actionNames)
	for name, p := range mergedParamSet {
		mergedParamSet[name] = reconcileUnionParam(p, actionNames, metaMap)
	}
	// Tool-level params: action enum first, then the merged union.
	finalParams := make([]Param, 0, 1+len(mergedOrder))
	finalParams = append(finalParams, Param{
		Name:        "action",
		Type:        "string",
		Required:    true,
		Description: actionEnumDescription(t.Name, metaMap, actionNames),
		Enum:        actionNames,
	})
	for _, name := range mergedOrder {
		finalParams = append(finalParams, mergedParamSet[name])
	}

	def := ToolDef{
		Name:               t.Name,
		Category:           t.Category,
		OneLiner:           t.OneLiner,
		WhenToUse:          t.WhenToUse,
		Params:             finalParams,
		Actions:            actionsMap,
		ActionMeta:         metaMap,
		Hints:              t.Hints,
		Summarize:          t.Summarize,
		MaxResult:          t.MaxResult,
		FullResultInEvents: t.FullResultInEvents,
		RequestsPause:      t.RequestsPause,
		RequiresCapability: t.RequiresCapability,
		MaxDuration:        t.MaxDuration,
		Stages:             t.Stages,
		Probe:              t.Probe,
		RealtimeModel:      t.RealtimeModel,
		SourceFile:         source,
	}
	if err := reg.add(def); err != nil {
		report(err)
	}
}

// makeActionHandler wraps Run with: (1) per-action arg validation against the
// action's own schema (catches required/type/enum violations the tool-level
// union schema cannot enforce) and (2) reflection-based struct population so
// Run sees typed fields instead of map[string]interface{}.
func makeActionHandler(actionT reflect.Type, fields []actionFieldInfo, params []Param) ActionHandler {
	return func(ctx *ToolContext, args map[string]any) (*Result, error) {
		if r := clarifyActionEnum(validateActionArgs(params, args)); r != nil {
			return r, nil
		}
		ptr, err := populateActionStruct(actionT, fields, args)
		if err != nil {
			return FailCode("invalid_type", "%s", err.Error()), nil
		}
		runner, ok := ptr.Interface().(Action)
		if !ok {
			return Fail("internal: %s does not implement Action", actionT), nil
		}
		return runner.Run(ctx)
	}
}

// actionNameFor resolves the action's name: an explicit Name() method wins,
// otherwise we derive it from the struct type name.
func actionNameFor(toolName string, a Action, t reflect.Type) string {
	if n, ok := a.(actionNamer); ok {
		return n.ActionName()
	}
	te := t
	if te.Kind() == reflect.Pointer {
		te = te.Elem()
	}
	return deriveActionName(toolName, te.Name())
}

// actionEnumDescription synthesizes the "action" param description from the
// per-action Summary lines, e.g. "store: save a value in the secrets store...".
// Gives the LLM enough at the tool-schema level to pick an action without
// loading per-action examples first.
func actionEnumDescription(toolName string, metas map[string]ActionMeta, names []string) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		s := strings.TrimSpace(metas[n].Summary)
		if s == "" {
			parts = append(parts, n)
			continue
		}
		s = strings.TrimSuffix(s, ".")
		parts = append(parts, fmt.Sprintf("%s - %s", n, s))
	}
	return "Verb: " + strings.Join(parts, "; ") + "."
}

// mergeParam combines two Param declarations that share a name across
// different actions. Description and enum are merged conservatively: the
// first non-empty description wins; enums are unioned (deduplicated, order
// preserved). The merged Param is what find_tool surfaces in the tool's
// JSON Schema; per-action validation always uses the original per-action
// Param for enforcement, so widening here doesn't loosen runtime checks.
func mergeParam(existing, incoming Param) Param {
	if existing.Name == "" {
		return incoming
	}
	out := existing
	if out.Description == "" {
		out.Description = incoming.Description
	}
	if out.AltType == "" {
		out.AltType = incoming.AltType
	}
	if out.Items == nil {
		out.Items = incoming.Items
	}
	if !out.HasDefault && incoming.HasDefault {
		out.Default = incoming.Default
		out.HasDefault = true
	}
	if !out.HasMin && incoming.HasMin {
		out.Min = incoming.Min
		out.HasMin = true
	}
	if !out.HasMax && incoming.HasMax {
		out.Max = incoming.Max
		out.HasMax = true
	}
	if len(incoming.Enum) > 0 {
		seen := make(map[string]bool, len(out.Enum))
		for _, e := range out.Enum {
			seen[e] = true
		}
		for _, e := range incoming.Enum {
			if !seen[e] {
				out.Enum = append(out.Enum, e)
				seen[e] = true
			}
		}
	}
	return out
}

// reconcileUnionParam makes one merged union param tell the truth about every action
// that declares it. The wire schema is a single object for all actions, so whatever
// it says about an arg is read as true for each of them:
//
//   - Scope: a param only some actions take is prefixed "[a, b]" so the model does not
//     send it to an action that drops it.
//   - Meaning: when actions describe the param differently (update's "omit to keep the
//     stored value" vs create's default), every distinct text is kept, the most common
//     one first and each other one labelled with its actions. mergeParam alone kept
//     only the first, so update-only semantics never reached the model.
//   - Default and bounds: kept on the union only when every declaring action agrees.
//     Otherwise the union shows none and SchemaNotes lists the per-action values, rather
//     than advertising one action's default (test_auth_flow's endpoint_path "auth") as
//     everyone's.
//
// Deterministic: groups and labels follow the sorted action order.
func reconcileUnionParam(p Param, actionNames []string, metas map[string]ActionMeta) Param {
	var users []string
	var decls []Param
	for _, a := range actionNames {
		for _, ap := range metas[a].Params {
			if ap.Name == p.Name {
				users = append(users, a)
				decls = append(decls, ap)
				break
			}
		}
	}
	if len(users) == 0 {
		return p
	}

	agreeDefault, agreeMin, agreeMax := true, true, true
	for _, d := range decls[1:] {
		agreeDefault = agreeDefault && d.HasDefault == decls[0].HasDefault && d.Default == decls[0].Default
		agreeMin = agreeMin && d.HasMin == decls[0].HasMin && d.Min == decls[0].Min
		agreeMax = agreeMax && d.HasMax == decls[0].HasMax && d.Max == decls[0].Max
	}
	if !agreeDefault {
		p.Default, p.HasDefault = "", false
	}
	if !agreeMin {
		p.Min, p.HasMin = 0, false
	}
	if !agreeMax {
		p.Max, p.HasMax = 0, false
	}

	// Group the actions by description text, preserving first-seen order.
	var texts []string
	byText := map[string][]string{}
	for i, d := range decls {
		t := strings.TrimSpace(d.Description)
		if t == "" {
			continue
		}
		if _, seen := byText[t]; !seen {
			texts = append(texts, t)
		}
		byText[t] = append(byText[t], users[i])
	}
	if len(texts) > 0 {
		// The most widely shared text leads unlabelled; among equally shared texts the
		// shortest (the most generic) leads, then first-seen order.
		lead := 0
		for i, t := range texts {
			n, ln := len(byText[t]), len(byText[texts[lead]])
			if n > ln || (n == ln && len(t) < len(texts[lead])) {
				lead = i
			}
		}
		parts := []string{texts[lead]}
		leadWords := descWords(texts[lead], nil)
		for i, t := range texts {
			if i == lead || subsumedBy(descWords(t, byText[t]), leadWords) {
				continue
			}
			parts = append(parts, strings.Join(byText[t], ", ")+": "+t)
		}
		p.Description = strings.Join(parts, " | ")
	}
	if len(users) < len(actionNames) {
		p.Description = actionScopeLabel(users, actionNames) + " " + p.Description
	}
	return p
}

// actionScopeLabel names the actions a param belongs to in the shorter of two forms:
// "[a, b]" or, when nearly every action takes it, "[all but c]".
func actionScopeLabel(users, all []string) string {
	in := map[string]bool{}
	for _, u := range users {
		in[u] = true
	}
	var others []string
	for _, a := range all {
		if !in[a] {
			others = append(others, a)
		}
	}
	pos := "[" + strings.Join(users, ", ") + "]"
	neg := "[all but " + strings.Join(others, ", ") + "]"
	if len(neg) < len(pos) {
		return neg
	}
	return pos
}

// descStopwords carry no meaning a variant description could add.
var descStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "of": true, "for": true, "and": true,
	"or": true, "in": true, "on": true, "by": true, "with": true, "is": true, "be": true,
	"it": true, "this": true, "its": true, "that": true,
}

// descWords returns the content words of a description, lowercased, minus stopwords and
// the names of the actions it belongs to ("Endpoint path to delete" on delete says
// nothing the shared text does not).
func descWords(s string, actions []string) map[string]bool {
	skip := map[string]bool{}
	for _, a := range actions {
		skip[a] = true
		for _, part := range strings.Split(a, "_") {
			skip[part] = true
		}
	}
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '*')
	}) {
		if !descStopwords[w] && !skip[w] {
			out[w] = true
		}
	}
	return out
}

// subsumedBy reports whether a variant description adds too little to the shared one to
// earn bytes on a schema every turn re-reads: fewer than three content words the shared
// text lacks ("Replace allowed HTTP methods", "Table to modify"). A variant carrying real
// semantics ("pass -1 to DISABLE caching", "only applied when auth_model is sent in the
// same call") brings several new words and is kept.
func subsumedBy(a, b map[string]bool) bool {
	novel := 0
	for w := range a {
		if !b[w] {
			novel++
		}
	}
	return novel < 3
}

// validateActionArgs runs the same kind of schema check as validateArgs but
// against the per-action param slice. Called from the action handler closure
// after the tool-level dispatch has chosen which action to run.
func validateActionArgs(params []Param, args map[string]any) *Result {
	def := &ToolDef{Params: params}
	return validateArgs(def, args)
}

// clarifyActionEnum appends the union-vs-action context to a per-action
// invalid_enum rejection. The tool-level wire schema advertises the enum UNION
// across actions (mergeParam), so a model can pick a value the schema showed
// yet the chosen action rejects - without this line the rejection looks like it
// contradicts the schema the model just read. Every other code passes through
// untouched.
func clarifyActionEnum(r *Result) *Result {
	if r != nil && r.Code == "invalid_enum" {
		r.Error += " for THIS action - the tool-level schema advertises the union of every action's values; this action accepts only the values listed here."
	}
	return r
}
