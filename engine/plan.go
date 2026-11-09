package engine

// Plan data model + JSON parsing. The PLAN stage emits a slim brief (goal, voice, design)
// plus a flat task queue. Parsing is tolerant (code fences and a trailing-brace overrun
// are repaired) but does not validate shape; the build agent and probe handle correctness.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"openfabrica/analysis"
	"openfabrica/internal/jsonrepair"
	"openfabrica/pack"
)

// Plan is the single project specification produced by the PLAN stage.
type Plan struct {
	Goal string `json:"goal,omitempty"`
	// Intro: short conversational explanation shown in chat after PLAN parses.
	Intro string `json:"intro,omitempty"`
	// Voice and Design are free-text design intent: an editorial voice for copy and a prose
	// visual direction. The build agent reads them as guidance; nothing validates them.
	Voice  string `json:"voice,omitempty"`
	Design string `json:"design,omitempty"`
	// Theme is the planner's read of what the owner wants: audience, mood, and why the
	// ground and palette suit it. Design and DesignSystem must trace back to it.
	Theme string `json:"theme,omitempty"`
	// DesignSystem is the same visual direction in concrete values, so the look survives
	// the trip to BUILD without being re-derived from prose. Optional; Design stays the
	// voice of the look. Read by the PWA manifest and the admin chat's token block.
	DesignSystem *PlanDesignSystem `json:"design_system,omitempty"`
	// Tasks is the build checklist PLAN seeds into of_tasks: the single dial that scales the
	// build, from a few for a one-page tool to dozens for a large CMS, on one session loop.
	Tasks []PlanTaskSpec `json:"tasks,omitempty"`
	// ExternalLibraries are CDN assets the renderer auto-injects. Mutable mid-build
	// via manage_libraries (add_library upserts; remove_library drops an entry).
	ExternalLibraries []LibrarySpec `json:"external_libraries,omitempty"`
	// Guides are guide slugs the planner declares the BUILD needs but which its task
	// `tools` / `external_libraries` don't already imply (e.g. e2ee). The
	// build prefetches the union of these and the tool/library signals and inlines those
	// guides' bodies into the seed; unknown slugs are dropped at prefetch time.
	Guides []string `json:"guides,omitempty"`
	// Exclusions are things the planner explicitly decided NOT to build.
	Exclusions LooseStrings `json:"exclusions,omitempty"`
	// Features are the brief's features as the engine extracted them (never written by
	// the planner): the list the coverage check, the build seed and the spec audit read.
	Features []PlanFeature `json:"features,omitempty"`
	// Decisions are the load-bearing architecture choices the planner LOCKED (realtime
	// model, auth, persistence, rendering). The build seed renders them as binding
	// constraints, and the conformance validator checks the ones carrying a machine-checkable
	// Check against the built artifacts, so a decision can't be silently reinterpreted.
	Decisions []PlanDecision `json:"decisions,omitempty"`
	// Questions pause the pipeline for owner input (max MaxPlanQuestions).
	Questions []PlanQuestion `json:"questions,omitempty"`
}

// PlanDesignSystem carries the token values a look is built from. Prose alone is a
// lossy channel: it survives PLAN but arrives at BUILD as something to re-invent.
type PlanDesignSystem struct {
	Colors      PlanDesignColors `json:"colors,omitempty"`
	FontBody    string           `json:"font_body,omitempty"`    // body/UI family, matching a declared font library
	FontDisplay string           `json:"font_display,omitempty"` // heading/display family
	TypeScale   string           `json:"type_scale,omitempty"`   // e.g. "1.25 major third, 14px base"
	SpacingBase string           `json:"spacing_base,omitempty"` // e.g. "4px"
	Radius      string           `json:"radius,omitempty"`       // e.g. "2px" or "14px"
	Elevation   string           `json:"elevation,omitempty"`    // the shadow recipe, e.g. "two-layer, warm-tinted"
	Motion      string           `json:"motion,omitempty"`       // signature easing/duration
}

// PlanDesignColors is the palette in roles, not a single accent. Primary and Bg also
// drive the generated PWA manifest.
type PlanDesignColors struct {
	Primary string `json:"primary,omitempty"`
	Bg      string `json:"bg,omitempty"`
	Surface string `json:"surface,omitempty"`
	Text    string `json:"text,omitempty"`
	Accent  string `json:"accent,omitempty"`
	Muted   string `json:"muted,omitempty"`
}

// PlanDecision is one locked architecture choice the build must honor.
type PlanDecision struct {
	Topic     string         `json:"topic"`               // e.g. "realtime model"
	Choice    string         `json:"choice"`              // e.g. "tick"
	Rationale string         `json:"rationale,omitempty"` // one line, tied to the brief
	Check     *DecisionCheck `json:"check,omitempty"`     // optional machine-checkable assertion
}

// DecisionCheck is an optional assertion over a built artifact: the conformance validator
// resolves Artifact+ID to a property and compares it with Op/Value. An unresolvable check
// (unknown artifact, missing row, unknown field) is SKIPPED, so it can never fabricate a finding.
type DecisionCheck struct {
	Artifact string `json:"artifact"`        // websocket | trigger | endpoint (the types resolveDecisionField reads)
	ID       string `json:"id"`              // the artifact's path/name
	Field    string `json:"field"`           // the property to read
	Op       string `json:"op"`              // gt | gte | lt | lte | eq | ne | exists | nexists
	Value    any    `json:"value,omitempty"` // the comparand
}

// PlanTaskSpec is one task the planner seeds into the build queue (of_tasks). Deps express
// ordering; Paths declare the artifacts this task owns; Edits names files this task MODIFIES
// but another task owns (edited in place, not re-created). Id is optional (the queue assigns
// a slug from the title); declare one only when another task's Deps reference it.
type PlanTaskSpec struct {
	ID       string   `json:"id,omitempty"`
	Title    string   `json:"title"`
	Intent   string   `json:"intent,omitempty"`
	DoneWhen string   `json:"done_when,omitempty"`
	Deps     []string `json:"deps,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Edits    []string `json:"edits,omitempty"`
	// Exports names the symbols this task's module(s) publish for sibling tasks to import:
	// the cross-leaf data contract (Deps is ordering, Edits is "I wire into your file",
	// Exports is "I import this FROM you"). Surfaced so a consumer imports the accessor
	// instead of stubbing it on a window global.
	Exports []string `json:"exports,omitempty"`
	// Tools names the non-core tools this task needs (from the PLAN-prompt catalogue, e.g.
	// manage_relay, manage_auth). They preload into the task session's tool set so no
	// find_tool call, and no mid-session provider-cache bust, is needed for planned work.
	Tools []string `json:"tools,omitempty"`
	// Covers names the brief features (PlanFeature ids) this task builds, so a feature
	// no task covers is caught at plan time instead of shipping unbuilt.
	Covers []string `json:"covers,omitempty"`
}

// UnmarshalJSON decodes a task, tolerating shape drift in its id lists. A model under
// a schema it does not enforce writes "tools":[[]], a lone string, or a nested list;
// failing the whole plan over that costs a full re-emit of every task. Each list keeps
// the strings it holds at any depth and drops everything else.
func (t *PlanTaskSpec) UnmarshalJSON(b []byte) error {
	type alias PlanTaskSpec
	aux := struct {
		*alias
		Deps    json.RawMessage `json:"deps,omitempty"`
		Paths   json.RawMessage `json:"paths,omitempty"`
		Edits   json.RawMessage `json:"edits,omitempty"`
		Exports json.RawMessage `json:"exports,omitempty"`
		Tools   json.RawMessage `json:"tools,omitempty"`
		Covers  json.RawMessage `json:"covers,omitempty"`
	}{alias: (*alias)(t)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	t.Deps, t.Paths, t.Edits = idList(aux.Deps), idList(aux.Paths), idList(aux.Edits)
	t.Exports, t.Tools, t.Covers = idList(aux.Exports), toolNames(idList(aux.Tools)), idList(aux.Covers)
	return nil
}

// toolNames turns a planner's `tools` list into registered tool names. The prompts speak
// in capability shorthands (`create_auth`), and a planner that copies one into `tools`
// would preload nothing (it is not a tool) while still marking the task as declaring its
// own tools, which suppresses the keyword inference that would have found the right one.
// A shorthand realized by exactly one tool becomes that tool; anything else is left alone.
func toolNames(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, n := range in {
		name := strings.TrimSpace(n)
		for _, sh := range pack.Shorthands {
			if sh.Name == name && len(sh.Tools) == 1 {
				name = sh.Tools[0][0]
				break
			}
		}
		out = append(out, name)
	}
	return dedupeStrings(out)
}

// idList decodes one id list: a string array as is, otherwise every non-empty string
// found at any depth. Absent or null stays nil.
func idList(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var out []string
	if json.Unmarshal(raw, &out) == nil {
		return out
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	out = []string{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if s := strings.TrimSpace(x); s != "" {
				out = append(out, s)
			}
		case []any:
			for _, el := range x {
				walk(el)
			}
		}
	}
	walk(v)
	return out
}

// LibrarySpec is one external CDN library.
type LibrarySpec struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Purpose string `json:"purpose,omitempty"`
	// Kind is "classic" (default, UMD/IIFE <script src>) or "module" (ES module
	// via importmap, which also needs ModuleName). Omitted for CSS/fonts.
	Kind       string `json:"kind,omitempty"`
	ModuleName string `json:"module_name,omitempty"`
	// GlobalName is the `window.<name>` a classic <script src> defines (`marked`,
	// `Chart`, `qrcode`). Naming it is what lets the unbound-reference gates block
	// with certainty instead of degrading to a non-blocking warning for the whole
	// project (see analysis.LibraryGlobals). Empty for module/CSS/font libraries.
	GlobalName string `json:"global_name,omitempty"`
	// Integrity is an optional SRI hash on the auto-injected tag.
	Integrity string `json:"integrity,omitempty"`
}

// ExternalLib converts the spec into the dependency-free shape the shared
// library-globals decision takes, so analysis never has to import the engine.
func (l LibrarySpec) ExternalLib() analysis.ExternalLib {
	return analysis.ExternalLib{
		Name:       l.Name,
		URL:        l.URL,
		Kind:       l.Kind,
		ModuleName: l.ModuleName,
		GlobalName: l.GlobalName,
	}
}

// ExternalLibs converts a whole spec slice for analysis.LibraryGlobals.
func ExternalLibs(libs []LibrarySpec) []analysis.ExternalLib {
	out := make([]analysis.ExternalLib, 0, len(libs))
	for _, l := range libs {
		out = append(out, l.ExternalLib())
	}
	return out
}

// PlanQuestion is a question the planner asks the owner during planning.
type PlanQuestion struct {
	ID         string          `json:"id,omitempty"`
	Question   string          `json:"question"`
	Type       string          `json:"type"`
	Options    []string        `json:"options,omitempty"`
	SecretName string          `json:"secret_name,omitempty"`
	Fields     json.RawMessage `json:"fields,omitempty"`
}

// UnmarshalJSON accepts the bare-string shorthand (`"questions": ["..."]`) for
// the open-ended case; otherwise decodes the full object with a default Type.
func (pq *PlanQuestion) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		pq.Question = s
		pq.Type = "text"
		return nil
	}
	type alias PlanQuestion
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("question: %w", err)
	}
	*pq = PlanQuestion(a)
	if pq.Type == "" {
		if len(pq.Options) > 0 {
			pq.Type = "single_choice"
		} else {
			pq.Type = "text"
		}
	}
	return nil
}

// ParsePlan decodes a Plan from raw JSON. Structured output isn't always honoured, so
// jsonrepair repairs the deviations reasoning models emit (code fences, a plan buried in
// prose, a trailing-brace overrun, control bytes, Python True/False/None, trailing commas).
// The first-byte guard yields a friendly "expected object" error for a non-object reply.
func ParsePlan(raw string) (*Plan, error) {
	trimmed := jsonrepair.UnwrapJSON(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("invalid plan JSON: empty input")
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("invalid plan JSON: expected object, got %q", trimmed[:1])
	}
	repaired := jsonrepair.Repair(trimmed)
	var p Plan
	if err := json.Unmarshal([]byte(repaired), &p); err != nil {
		return nil, fmt.Errorf("invalid plan JSON: %w%s", err, jsonErrorSite(repaired, err))
	}
	canonicalizePlan(&p)
	return &p, nil
}

// jsonErrorSite renders " (at byte N: <excerpt>)" for a decoder error carrying an offset,
// "" otherwise. The offset indexes the REPAIRED text (what the decoder actually read), so
// the excerpt lands on the true failure site. It rides the error string because that is
// what gets logged and fed back as corrective feedback.
func jsonErrorSite(s string, err error) string {
	offset := -1
	var serr *json.SyntaxError
	var terr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &serr):
		offset = int(serr.Offset)
	case errors.As(err, &terr):
		offset = int(terr.Offset)
	}
	if offset < 0 || offset > len(s) {
		return ""
	}
	const window = 160
	// Slicing by byte can split a rune at either edge; drop the fragments.
	excerpt := strings.ToValidUTF8(s[max(0, offset-window):min(len(s), offset+window)], "")
	return fmt.Sprintf(" (at byte %d: %q)", offset, excerpt)
}

// LooseStrings decodes a JSON array of strings, tolerating elements a model emits as
// objects ("what" plus a "why") rather than failing the whole plan over one advisory list.
// An object reduces to its first recognised label field, an unrecognised element keeps its
// raw JSON, and marshalling re-emits a plain string array.
type LooseStrings []string

// looseStringKeys are the fields an object element is mined for, in preference
// order: what the entry IS outranks why it is there.
var looseStringKeys = []string{"item", "exclusion", "feature", "what", "title", "name", "text", "value", "description"}

// UnmarshalJSON implements json.Unmarshaler.
func (l *LooseStrings) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		// A lone string where a list was asked for is the same drift, one
		// element in. Anything else is a real error and stays one.
		var single string
		if json.Unmarshal(b, &single) != nil {
			return err
		}
		*l = nil
		if s := strings.TrimSpace(single); s != "" {
			*l = LooseStrings{s}
		}
		return nil
	}
	out := make(LooseStrings, 0, len(raw))
	for _, el := range raw {
		if s := looseString(el); s != "" {
			out = append(out, s)
		}
	}
	*l = out
	return nil
}

// looseString reduces one array element to its text: a string yields its value, an object
// its first recognised label field, anything else its compact JSON ("" if no text, dropped).
func looseString(el json.RawMessage) string {
	var s string
	if err := json.Unmarshal(el, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(el, &obj); err == nil {
		labelled := false
		for _, key := range looseStringKeys {
			raw, ok := obj[key]
			if !ok {
				continue
			}
			labelled = true
			var v string
			if json.Unmarshal(raw, &v) == nil && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		if labelled {
			// The shape was understood and its label is empty: nothing to say.
			return ""
		}
	}
	if bytes.Equal(bytes.TrimSpace(el), []byte("null")) {
		return ""
	}
	// Keyed by nothing we recognise: keep the text so the intent still reaches
	// whoever reads the list.
	var compact bytes.Buffer
	if err := json.Compact(&compact, el); err == nil {
		return compact.String()
	}
	return strings.TrimSpace(string(el))
}

// MaxPlanQuestions is the documented question cap (mirrors the "Ask at most 2
// questions" rule in the plan prompt).
const MaxPlanQuestions = 2

// largePlanTaskThreshold is the soft task/file count above which a single continuous build
// session accumulates enough resident context to be worth flagging. Advisory only: the
// planner legitimately scales tasks with project size, so this never blocks a build;
// PlanScopeAdvisory surfaces it in the run log.
const largePlanTaskThreshold = 24

// PlanScopeAdvisory returns a human-readable advisory when a plan is large enough that its
// single-session build accumulates substantial context, or "" otherwise. It counts both
// tasks and the distinct files they declare, since context grows with files, not task count.
func PlanScopeAdvisory(p *Plan) string {
	if p == nil {
		return ""
	}
	paths := make(map[string]bool)
	for _, t := range p.Tasks {
		for _, pth := range t.Paths {
			if pth = strings.TrimSpace(pth); pth != "" {
				paths[pth] = true
			}
		}
	}
	if len(p.Tasks) <= largePlanTaskThreshold && len(paths) <= largePlanTaskThreshold {
		return ""
	}
	return fmt.Sprintf("large plan: %d tasks declaring %d distinct files build in one continuous session; project context accumulates across the whole build (token-efficiency factor)", len(p.Tasks), len(paths))
}

// allowedColumnTypes mirrors the column-type contract in the plan prompt.
// Surfaced via the {{ALLOWED_COLUMN_TYPES}} sentinel.
var allowedColumnTypes = []string{
	"BLOB", "BOOLEAN", "ENCRYPTED", "INTEGER", "PASSWORD", "REAL", "TEXT",
}

// canonicalizePlan normalises planner output after JSON decode: dedupe guide slugs, ensure
// module libraries carry a module_name, drop a standalone library-registration task
// (auto-registered work the build agent would only thrash on), and clear inverted deps a
// split-out store task must not carry.
func canonicalizePlan(p *Plan) {
	if p == nil {
		return
	}
	p.Guides = dedupeStrings(p.Guides)
	fillModuleNames(p)
	fillGlobalNames(p)
	dropRedundantLibraryTasks(p)
	clearStateLeafDeps(p)
	normalizeDecisionOps(p)
	normalizeRealtimeChoices(p)
	normalizeDesignSystem(p)
}

// normalizeDesignSystem cleans the plan's token block. A provider that does not
// honour ResponseSchema can still emit an empty shell (`{"colors":{}}`), which
// would serialize into plan_json and render as an empty tokens block downstream;
// an all-empty block is dropped instead. Colour values are trimmed to the bare
// hex a consumer can use: planners annotate them ("#d8b4b8 (rose quartz, hover)")
// and the PWA manifest writes colours through verbatim.
func normalizeDesignSystem(p *Plan) {
	if p == nil || p.DesignSystem == nil {
		return
	}
	ds := p.DesignSystem
	c := &ds.Colors
	for _, f := range []*string{&c.Primary, &c.Bg, &c.Surface, &c.Text, &c.Accent, &c.Muted} {
		*f = canonicalHexColor(*f)
	}
	if ds.isEmpty() {
		p.DesignSystem = nil
	}
}

// isEmpty reports whether the block carries no values at all. A provider that does
// not honour ResponseSchema can still emit a shell like `{"colors":{}}`, which is
// worth no heading and no tokens anywhere downstream.
func (ds *PlanDesignSystem) isEmpty() bool {
	return ds == nil || *ds == (PlanDesignSystem{})
}

// canonicalHexColor reduces a colour field to the leading hex literal, dropping a
// trailing annotation. A value with no hex at all is left as written: a named
// colour or a var() reference is still meaningful to the build agent, and only
// the manifest needs a literal (it validates separately).
func canonicalHexColor(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || !strings.HasPrefix(v, "#") {
		return v
	}
	end := 1
	for end < len(v) && isHexDigit(v[end]) {
		end++
	}
	switch end - 1 {
	case 3, 4, 6, 8:
		return v[:end]
	}
	return v
}

func isHexDigit(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

// validDecisionOps is the closed op set decisionConforms evaluates. Any other op is
// skipped-as-pass by the conformance validator, so an op typo silently disables its check.
var validDecisionOps = map[string]bool{
	"gt": true, "gte": true, "lt": true, "lte": true,
	"eq": true, "ne": true, "exists": true, "nexists": true,
}

// decisionOpSynonyms maps the spellings planners actually emit onto the canonical op set,
// so a `">="` or `"equals"` check stays live instead of being silently skipped.
var decisionOpSynonyms = map[string]string{
	">": "gt", ">=": "gte", "<": "lt", "<=": "lte",
	"=": "eq", "==": "eq", "equal": "eq", "equals": "eq",
	"!=": "ne", "<>": "ne", "not_equal": "ne", "not_equals": "ne",
	"greater_than": "gt", "greater_than_or_equal": "gte",
	"less_than": "lt", "less_than_or_equal": "lte",
	"not_exists": "nexists", "notexists": "nexists", "absent": "nexists",
	"present": "exists",
}

// normalizeDecisionOps lowercases/trims every DecisionCheck.Op and maps common synonyms
// onto the canonical set; ops it cannot canonicalize are left for planCheckWarnings to surface.
func normalizeDecisionOps(p *Plan) {
	for i := range p.Decisions {
		c := p.Decisions[i].Check
		if c == nil {
			continue
		}
		op := strings.ToLower(strings.TrimSpace(c.Op))
		if canon, ok := decisionOpSynonyms[op]; ok {
			op = canon
		}
		c.Op = op
	}
}

// realtimeChoices is the closed set of realtime-model decision choices, one per model
// tool (webrtc = manage_signaling). Readers compare the choice exactly, so it is
// canonicalized once at parse time rather than each reader matching its own way.
var realtimeChoices = []string{"authoritative", "matchmaking", "webrtc", "relay", "tick"}

// realtimeNone is the realtime-model choice of a project with no live shared state (a
// single-user or offline app). The plan stage asks for the decision on every project,
// so "none" is a real answer, not an unreadable one: it locks NO realtime tool.
const realtimeNone = "none"

// realtimeNoneSpellings are the leading labels a planner writes for "no realtime".
var realtimeNoneSpellings = []string{"none", "n/a", "not applicable", "not needed", "no realtime", "no real-time", "offline"}

// isRealtimeModelTopic reports whether a decision topic is the realtime-model decision.
func isRealtimeModelTopic(topic string) bool {
	t := strings.ToLower(strings.TrimSpace(topic))
	return strings.Contains(t, "realtime") || strings.Contains(t, "real-time") || strings.Contains(t, "real time")
}

// normalizeRealtimeChoice maps a planner's free-text realtime-model choice onto the closed
// set by its leading label ("authoritative reducer" -> "authoritative", "tick - cursors"
// -> "tick", "signaling"/"manage_signaling" -> "webrtc", "none - single user" -> "none").
// "" when it names no model and is not "none" either.
func normalizeRealtimeChoice(choice string) string {
	c := strings.ToLower(strings.TrimSpace(choice))
	c = strings.TrimPrefix(c, "manage_")
	for _, canon := range realtimeChoices {
		if strings.HasPrefix(c, canon) {
			return canon
		}
	}
	if strings.HasPrefix(c, "signaling") || strings.HasPrefix(c, "signalling") {
		return "webrtc"
	}
	for _, none := range realtimeNoneSpellings {
		if strings.HasPrefix(c, none) {
			return realtimeNone
		}
	}
	return ""
}

// normalizeRealtimeChoices canonicalizes the realtime-model decision's topic and choice,
// so every downstream reader (the reducer-contract build seed, realtime tool pruning, the
// coherence check) sees "realtime model" / one of realtimeChoices. Without it, a choice
// of "authoritative reducer" silently skips the reducer-contract seed.
func normalizeRealtimeChoices(p *Plan) {
	for i := range p.Decisions {
		d := &p.Decisions[i]
		if !isRealtimeModelTopic(d.Topic) {
			continue
		}
		if canon := normalizeRealtimeChoice(d.Choice); canon != "" {
			d.Topic = "realtime model"
			d.Choice = canon
		}
	}
}

// planCheckWarnings returns one line per decision check the conformance validator cannot
// evaluate, so each disabled check is logged at plan time instead of silently enforcing
// nothing. Two defects are surfaced: an OP outside the canonical set, and an ARTIFACT/FIELD
// outside resolveDecisionField's vocabulary. A check with no field is "no check".
func planCheckWarnings(p *Plan) []string {
	if p == nil {
		return nil
	}
	var warnings []string
	for _, d := range p.Decisions {
		if isRealtimeModelTopic(d.Topic) && normalizeRealtimeChoice(d.Choice) == "" {
			warnings = append(warnings, fmt.Sprintf(
				"realtime-model decision %q has choice %q, which is none of %s | %s - the build cannot tell which realtime tool the plan locked",
				d.Topic, d.Choice, strings.Join(realtimeChoices, " | "), realtimeNone))
		}
		if d.Check == nil || strings.TrimSpace(d.Check.Field) == "" {
			continue
		}
		if !validDecisionOps[d.Check.Op] {
			warnings = append(warnings, fmt.Sprintf(
				"decision %q carries unknown check op %q (valid: gt gte lt lte eq ne exists nexists) - the conformance validator will skip this check",
				d.Topic, d.Check.Op))
		}
		if !decisionCheckResolvable(d.Check.Artifact, d.Check.Field) {
			warnings = append(warnings, fmt.Sprintf(
				"decision %q carries a check the conformance validator cannot resolve: artifact %q field %q is outside its vocabulary (checkable artifacts: websocket, trigger, endpoint) - the check will be skipped, enforcing nothing. Emit a check only when a guide's `## Decision:` block gives you one; otherwise omit it.",
				d.Topic, d.Check.Artifact, d.Check.Field))
		}
	}
	return warnings
}

// dropRedundantLibraryTasks removes a planner antipattern: a standalone task whose only job
// is registering external libraries. Those are auto-registered BEFORE the build, so the task
// owns no files and the build agent thrashes trying to `complete` it. Fires only when
// external_libraries is non-empty, and scrubs the dropped id from remaining tasks' deps.
func dropRedundantLibraryTasks(p *Plan) {
	if p == nil || len(p.ExternalLibraries) == 0 || len(p.Tasks) == 0 {
		return
	}
	dropped := make(map[string]bool)
	kept := make([]PlanTaskSpec, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		if len(t.Paths) == 0 && len(t.Edits) == 0 && isLibraryRegistrationTask(t, p.ExternalLibraries) {
			if id := strings.TrimSpace(t.ID); id != "" {
				dropped[id] = true
			}
			continue
		}
		kept = append(kept, t)
	}
	if len(dropped) == 0 {
		return
	}
	for i := range kept {
		if len(kept[i].Deps) == 0 {
			continue
		}
		deps := make([]string, 0, len(kept[i].Deps))
		for _, dep := range kept[i].Deps {
			if !dropped[strings.TrimSpace(dep)] {
				deps = append(deps, dep)
			}
		}
		if len(deps) == 0 {
			deps = nil
		}
		kept[i].Deps = deps
	}
	p.Tasks = kept
}

// clearStateLeafDeps drops any deps a planner hangs on a split-out STORE task - a
// task that owns a `*.state.js` client module and edits nothing. That store module
// is the foundational, unimported source of the shared contract (the entry and
// every subsystem leaf import FROM it), so nothing it could depend on needs to
// exist first; a dep on the entry/spine is inverted and only makes the store
// task's completion wait on work it precedes. The entry-exports coherence fix
// (entryExportsProblem) and plan/STAGE.md offer exactly this task shape (no page
// route, no deps) - this makes the no-deps half true by construction rather than
// trusting each plan to. Subsystem leaves (which `edits` the entry) keep their
// spine dep.
func clearStateLeafDeps(p *Plan) {
	if p == nil {
		return
	}
	for i := range p.Tasks {
		if len(p.Tasks[i].Deps) > 0 && isZeroImportStateLeaf(p.Tasks[i]) {
			p.Tasks[i].Deps = nil
		}
	}
}

// isZeroImportStateLeaf reports whether a task is the split-out store task: it owns
// a `*.state.js` client module, modifies no other task's file (no Edits), and owns
// no page route. Its file merely existing is all its completion needs, so it depends
// on nothing.
func isZeroImportStateLeaf(t PlanTaskSpec) bool {
	if len(t.Edits) > 0 || len(t.Paths) == 0 {
		return false
	}
	hasStateLeaf := false
	for _, p := range t.Paths {
		if isPageRoutePath(p) {
			return false // owns a route -> a spine/page task, not a store task
		}
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(p)), ".state.js") {
			hasStateLeaf = true
		}
	}
	return hasStateLeaf
}

// isLibraryRegistrationTask reports whether a task's work is merely registering external
// libraries, by naming the concept ("library") or a declared library's SPECIFIER (the
// full name "three.js", or the module path form "three/addons"). The caller has already
// established the task owns no artifacts. A bare module name is deliberately not enough:
// names like "three" are ordinary words, and a false match silently deletes a real task.
func isLibraryRegistrationTask(t PlanTaskSpec, libs []LibrarySpec) bool {
	text := taskText(t)
	// "library" alone is an ordinary word in a library-management app ("Seed the library
	// catalogue"); a registration task also says that it registers, declares or loads
	// external/CDN libraries.
	if strings.Contains(text, "librar") { // library, libraries, external_libraries
		for _, verb := range []string{"external", "regist", "declar", "cdn", "importmap", "import map"} {
			if strings.Contains(text, verb) {
				return true
			}
		}
	}
	for _, l := range libs {
		name := strings.ToLower(strings.TrimSpace(l.Name))
		if len(name) >= 4 && containsWord(text, name) {
			return true
		}
		mod := strings.ToLower(strings.TrimSpace(l.ModuleName))
		if len(mod) >= 4 && strings.Contains(text, mod+"/") {
			return true
		}
	}
	return false
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// taskText is the lowercased title + intent + done_when of a task, for keyword matching.
func taskText(t PlanTaskSpec) string {
	return strings.ToLower(t.Title + " " + t.Intent + " " + t.DoneWhen)
}

// fillModuleNames guarantees every kind="module" external library carries a non-empty
// module_name, the specifier the importmap is keyed on: the renderer SILENTLY DROPS a
// module lib whose module_name is empty, breaking every `import ... from "<spec>"`. Derives
// it from the library name ("three.js" -> "three"); descriptive names yield "".
func fillModuleNames(p *Plan) {
	for i := range p.ExternalLibraries {
		lib := &p.ExternalLibraries[i]
		// Promote an ES-module library that omitted `kind`: an ESM URL left `classic` is
		// NEVER added to the importmap, so every `import ... from "<spec>"` throws at runtime.
		// Only when `kind` is unset and the URL is clearly ESM; an explicit `classic` is honoured.
		if strings.TrimSpace(lib.Kind) == "" && looksLikeESMURL(lib.URL) {
			lib.Kind = "module"
		}
		if !strings.EqualFold(strings.TrimSpace(lib.Kind), "module") {
			continue
		}
		if strings.TrimSpace(lib.ModuleName) != "" {
			continue
		}
		lib.ModuleName = deriveModuleName(lib.Name)
	}
}

// looksLikeESMURL reports whether url is unambiguously an ES-module build, so a library
// that omitted `kind` can be inferred as kind="module" rather than the default classic.
// Matches the ESM markers CDNs use (jsdelivr/unpkg/esm.sh); a font or CSS URL never matches.
func looksLikeESMURL(url string) bool {
	u := strings.ToLower(strings.TrimSpace(url))
	if u == "" {
		return false
	}
	bare := u
	if i := strings.IndexAny(bare, "?#"); i >= 0 {
		bare = bare[:i]
	}
	for _, suf := range []string{".mjs", ".module.js", ".esm.js"} {
		if strings.HasSuffix(bare, suf) {
			return true
		}
	}
	for _, mark := range []string{"/esm/", "+esm", "?module", "/es2015/", "esm.sh/"} {
		if strings.Contains(u, mark) {
			return true
		}
	}
	return false
}

// deriveModuleName best-effort derives a bare import specifier from a library
// name, returning "" when the name is not a clean specifier token.
func deriveModuleName(name string) string {
	s := strings.TrimSpace(name)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return ""
	}
	if at := strings.LastIndexByte(s, '@'); at > 0 { // >0 keeps a scoped "@org/pkg" intact
		s = s[:at]
	}
	for _, ext := range []string{".module.js", ".esm.js", ".min.js", ".mjs", ".js"} {
		if strings.HasSuffix(strings.ToLower(s), ext) {
			s = s[:len(s)-len(ext)]
			break
		}
	}
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, " \t\r\n") {
		return ""
	}
	return s
}
