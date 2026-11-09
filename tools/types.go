package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"openfabrica/config"
	"openfabrica/events"
	"openfabrica/internal/dbh"
	"openfabrica/security"
)

// ErrToolTimeout is wrapped into the error returned by Executor.Execute when
// a tool's deadline elapses before the tool returns. Callers can use
// errors.Is(err, tools.ErrToolTimeout) to distinguish a timeout from a
// tool-reported error without scraping error strings.
var ErrToolTimeout = errors.New("tool execution timeout")

// ToolContext carries dependencies into tool execution.
type ToolContext struct {
	Ctx context.Context // cancellation/timeout context from the pipeline
	// DB is the project-scoped database. The tool loops supply
	// db.ProjectDB.ToolHandle, which serves Query/QueryRow from the read pool and
	// Exec/Begin from the single-connection write pool, so a tool's reads never
	// compete for the write connection. Tests may pass a plain *sql.DB, which
	// satisfies dbh.Handle but does no routing.
	DB        dbh.Handle
	GlobalDB  *sql.DB // global database (users, providers, projects)
	ProjectID int
	// RunID is the active build/update run this call belongs to, set by the engine
	// tool loop for every non-LIVE stage. The user-flow replay budget keys on it so
	// the cap bounds a whole run (build plus repair, across a crash-resume restart)
	// rather than one process lifetime. Empty for LIVE owner chat and scheduled
	// tasks, which fall back to the in-memory per-project budget.
	RunID      string
	Logger     *slog.Logger
	Bus        *events.Bus
	Encryptor  *security.Encryptor
	JWTManager *security.JWTManager // for self-testing: minting auth tokens for protected endpoints
	PublicPort int                  // public server port for self-testing (0 = use config.DefaultPublicPort)
	DataDir    string               // instance data directory ("" = use config.DefaultDataDir)
	EmbedFunc  func(ctx context.Context, texts []string) ([][]float32, error)

	// ReadTracker (optional) deduplicates full artifact-body reads within one
	// build session so a repeated get of an unchanged file returns a stub
	// instead of re-sending the body. Nil outside the engine tool loop, where it
	// is inert. Set by the engine tool loop from the per-session loopState.
	ReadTracker *ReadTracker

	// SeedGuides (optional) names the guide slugs whose CORES are already
	// inlined in this session's seed, so manage_guides recall serves only what
	// the context lacks instead of a near-duplicate body. Nil outside BUILD.
	SeedGuides map[string]bool

	// HiddenGuides (optional) names guide slugs no tool can serve here (every tool
	// they cover is gated off), which manage_guides leaves out. Nil means none.
	HiddenGuides map[string]bool
}

// ChangedBy is the write-attribution label for an artifact version: every
// engine-driven write records "brain" in of_file_history.changed_by.
// Centralized so every artifact write attributes consistently.
func (tc *ToolContext) ChangedBy() string {
	return "brain"
}

// GlobalDBOrProject returns the global database, falling back to the project handle when
// GlobalDB is unset. The rows this serves (providers, models) live only in the global
// database, so the fallback is for single-database setups where one handle is both. It
// yields nil when the project handle is a routed handle (two pools, no single one to hand
// back). Production always sets GlobalDB, so the fallback never runs there.
func (tc *ToolContext) GlobalDBOrProject() *sql.DB {
	if tc.GlobalDB != nil {
		return tc.GlobalDB
	}
	raw, _ := tc.DB.(*sql.DB)
	return raw
}

// DataDirOrDefault returns the instance data directory, or the built-in default
// (config.DefaultDataDir) when it is unset, so the fallback lives in one place instead of
// each disk-writing tool carrying its own. Tools that persist bytes resolve their path from
// this; the server reads them back through ProjectDBManager.ProjectDir (derived from the
// same DataDir), so a hardcoded path would disagree whenever OPENFABRICA_DATA_DIR is set.
func (tc *ToolContext) DataDirOrDefault() string {
	if tc.DataDir == "" {
		return config.DefaultDataDir
	}
	return tc.DataDir
}

// ProjectDir is this project's directory under the data dir, where disk-stored
// files (uploads, generated media) live. Mirrors db.ProjectDBManager.ProjectDir,
// which the servers read the same files back through.
func (tc *ToolContext) ProjectDir() string {
	return filepath.Join(tc.DataDirOrDefault(), "projects", strconv.Itoa(tc.ProjectID))
}

// PublicPortOrDefault returns the configured public server port, or the built-in
// default (config.DefaultPublicPort) when it is unset. Tool handlers that build
// self-test URLs use it instead of each re-hardcoding the fallback, so the
// default lives in exactly one place.
func (tc *ToolContext) PublicPortOrDefault() int {
	if tc.PublicPort == 0 {
		return config.DefaultPublicPort
	}
	return tc.PublicPort
}

// Result is the standard return value from a tool execution.
// Field order is the MARSHAL order, and result truncation cuts from the tail
// (TruncateResultAt), so code and error come before data: a clipped result
// loses the bulky payload, never the failure classification.
type Result struct {
	Success bool `json:"success"`
	// Code is a stable machine-readable error tag (e.g. "missing_arg",
	// "invalid_type", "unknown_action"). Set on framework-level errors so the
	// LLM and the UI can distinguish "you called the tool wrong" from
	// "the tool ran and the operation failed". Empty when the failure is a
	// runtime/business error described only in Error.
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
	// Hint is the tool's recovery or follow-up note when it cannot ride inside Data: on a
	// failure, or when Data is not an object (a list). A successful object result keeps
	// its hint at data.hint.
	Hint string `json:"hint,omitempty"`
	// IgnoredArgs names the arguments this call passed that the chosen action does not
	// declare, each with a did-you-mean when one is known. They had no effect, so a
	// setting the model meant to apply never landed.
	IgnoredArgs []string `json:"ignored_args,omitempty"`
	Data        any      `json:"data,omitempty"`
}

// CodeMissingArg is the single stable code for "a required argument was absent or empty".
// The schema check (validateArgs), the RequireString / RequireAction helpers, and any
// handler that hand-checks a required field all tag the failure with this value, so the
// model sees the same signal no matter which layer caught it (the human Error text may
// still vary by call site).
const CodeMissingArg = "missing_arg"

// CodeBadArg is the stable code for "the argument was present but its value is
// unusable" (bad name, empty list, nonexistent column). Distinct from
// CodeMissingArg so triage can tell an omitted arg from a wrong one.
const CodeBadArg = "bad_arg"

// ResultIsSuccess parses a marshaled Result JSON and returns its top-level
// "success" field. Falls back to true on parse error so non-Result strings
// (rare; tools always return Result) don't get treated as failures.
func ResultIsSuccess(raw string) bool {
	var probe struct {
		Success *bool `json:"success"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return true
	}
	if probe.Success == nil {
		return true
	}
	return *probe.Success
}

// ResultCode parses a marshaled Result JSON and returns its "code" field (empty
// when absent or unparseable). The engine session loop uses it to recognize the
// approval-pause signal (CodeApprovalRequired) on an otherwise-failed result.
func ResultCode(raw string) string {
	var probe struct {
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(raw), &probe) != nil {
		return ""
	}
	return probe.Code
}

// ActionHandler is the signature for tool action implementations.
type ActionHandler func(ctx *ToolContext, args map[string]any) (*Result, error)

// Param defines a single tool parameter with typed schema generation.
type Param struct {
	Name        string
	Type        string // "string", "integer", "number", "boolean", "array", "object"
	Description string
	Required    bool
	Enum        []string       // nil = no enum constraint
	Items       map[string]any // for array items schema (e.g. {"type":"string"})

	// AltType, when set, declares an additional acceptable JSON type. Lets a
	// single param accept either string or object (e.g. manage_http's body,
	// which is either a raw string or a structured payload that the handler
	// JSON-encodes). Emits `"type": [Type, AltType]` in the JSON schema and
	// loosens validateArgs to accept either concrete type.
	AltType string

	// Default carries the raw `default:"..."` tag value for an optional arg, and HasDefault
	// records whether the tag was present. The runtime applies it during struct population
	// (see reflect.go applyDefault); these fields also let Parameters() surface the default
	// in the JSON schema so the model knows the effective value when it omits the arg.
	// Default is the raw tag string; Parameters() coerces it via typedDefault.
	Default    string
	HasDefault bool

	// Min/Max carry the numeric bounds from `min:"..."`/`max:"..."` tags on an
	// integer/number arg, single-sourcing the bound: Parameters() renders them as JSON-schema
	// minimum/maximum, and the runtime REJECTS an out-of-range value during
	// struct population (see reflect.go checkNumericBound). HasMin/HasMax record which bounds were
	// declared.
	Min    float64
	Max    float64
	HasMin bool
	HasMax bool
}

// Example is one fully-formed call shown to the LLM in the rendered tool doc.
// At least one Example per action is mandatory; the validator unmarshals each
// Example.Args against the action's parameter schema to catch out-of-sync samples.
type Example struct {
	Title string
	Args  map[string]any
}

// ActionMeta carries the LLM-facing prose for one action: a one-line Summary,
// at least one Example, and a ReadOnly flag for the build loop's read-only
// streak detector. The handler itself lives in ToolDef.Actions[name].
type ActionMeta struct {
	Summary  string
	Examples []Example
	ReadOnly bool
	// Params is this action's own schema (required-ness and enums as the
	// handler actually enforces them). The tool-level Params union widens
	// enums across actions; find_tool docs use this to tell the model which
	// values each action really accepts.
	Params []Param
}

// ToolDef is the registry-side record for one registered tool, the runtime shape consumed
// by find_tool, the executor, the validator, and the prompt assembler. Tools do NOT
// construct it directly; they declare themselves via tools.Define(tools.Tool{...}), which
// reflects each Action struct into params/meta/handler. The LLM-facing doc is generated from
// this struct, so what the LLM reads and what the dispatcher routes cannot diverge.
type ToolDef struct {
	Name      string  // unique, e.g. "manage_files"
	Category  string  // grouping in the rendered catalogue
	OneLiner  string  // short description (<=130 chars), used as LLM tool.description
	WhenToUse string  // 1-3 sentence routing guidance returned with the schema by find_tool
	Params    []Param // JSON-schema parameters for the action arg envelope

	// Actions maps action name -> handler. Every action must have a matching
	// ActionMeta entry; the validator rejects registrations where one half is
	// missing.
	Actions    map[string]ActionHandler
	ActionMeta map[string]ActionMeta

	// ReadOnlyActions lists which Actions are read-only. Consumed by the build
	// loop's read-only streak detector. Derived from ActionMeta if empty.
	ReadOnlyActions map[string]bool

	// Runtime hooks consumed by the engine tool loop.
	Hints     func(action string, args map[string]any, result *Result) string
	Summarize func(result string) string
	MaxResult int // result byte budget; 0 = the engine session default (3000)

	// FullResultInEvents includes the full result string in event payloads
	// instead of truncating; set on tools whose UI relies on structured data.
	FullResultInEvents bool

	// RequestsPause, when set, decides after a successful call whether the
	// pipeline should pause for owner input. Used by question-asking tools.
	RequestsPause func(action string, args map[string]any) bool

	// RequiresCapability gates LLM exposure on provider availability. The
	// tool is hidden from the LLM unless a usable provider of the matching
	// kind exists. Valid values are the keys of llm.KnownCapabilities.
	RequiresCapability string

	// MaxDuration overrides the caller-supplied per-tool deadline. Zero means
	// the caller decides (the engine pipeline defaults to 2m). Tools that
	// legitimately need longer (image gen, headless browser, large PDFs)
	// declare it here so callers don't have to special-case them by name.
	MaxDuration time.Duration

	// Stages restricts catalogue exposure and find_tool lookup to a list of
	// lowercase stage names ("build", "live"). Empty/nil means no
	// restriction. Enforced by Registry.Catalogue / LookupFull / Search when a
	// non-empty stage filter is passed.
	Stages []string

	// Probe and RealtimeModel are the build-loop traits declared on tools.Tool
	// (see there); the engine keys its guard and preload policy on them rather
	// than on tool names.
	Probe         bool
	RealtimeModel string

	// SourceFile is the Go file whose Define call registered this tool, recorded
	// so TestToolFileNameCategoryAgree can hold the tools/{Category}/{Name}.go
	// convention. Empty for defs not built through Define.
	SourceFile string
}

// AvailableInStage reports whether this tool should be visible to the LLM in the named
// stage. An empty stage filter ("") disables stage gating; an empty Stages slice on the def
// means "all stages".
func (d *ToolDef) AvailableInStage(stage string) bool {
	if stage == "" || len(d.Stages) == 0 {
		return true
	}
	for _, s := range d.Stages {
		if s == stage {
			return true
		}
	}
	return false
}

// Parameters generates the JSON Schema for this tool's parameters.
func (d *ToolDef) Parameters() map[string]any {
	props := make(map[string]any, len(d.Params))
	var required []string
	for _, p := range d.Params {
		prop := map[string]any{
			"description": p.Description,
		}
		if p.AltType != "" {
			prop["type"] = []string{p.Type, p.AltType}
		} else {
			prop["type"] = p.Type
		}
		if len(p.Enum) > 0 {
			prop["enum"] = p.Enum
		}
		if p.Items != nil {
			prop["items"] = p.Items
		} else if p.Type == "array" {
			// Some providers reject an array schema with no items; an empty schema
			// leaves the elements unconstrained.
			prop["items"] = map[string]any{}
		}
		if p.HasDefault {
			prop["default"] = typedDefault(p)
		}
		if p.HasMin {
			prop["minimum"] = boundForType(p, p.Min)
		}
		if p.HasMax {
			prop["maximum"] = boundForType(p, p.Max)
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// typedDefault coerces a param's raw `default` tag string into the JSON type the param
// declares, so the rendered schema carries a typed value (true, not "true"; 30, not "30").
// Mirrors reflect.go applyDefault's scalar parsing so the two never diverge. Falls back to
// the raw string when parsing fails, so a malformed tag is surfaced rather than dropped.
func typedDefault(p Param) any {
	raw := strings.TrimSpace(p.Default)
	switch p.Type {
	case "boolean":
		switch strings.ToLower(raw) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no", "":
			return false
		}
		return p.Default
	case "integer":
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
		return p.Default
	case "number":
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return f
		}
		return p.Default
	case "array":
		if raw == "" {
			return []string{}
		}
		parts := strings.Split(p.Default, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	default: // string and any other type: emit the raw tag value
		return p.Default
	}
}

// boundForType renders a numeric min/max bound in the param's JSON type so the
// schema carries an integer 200 rather than 200.0 for an integer arg. Mirrors
// typedDefault's integer/number split.
func boundForType(p Param, v float64) any {
	if p.Type == "integer" {
		return int64(v)
	}
	return v
}

// Execute dispatches to the registered action handler via DispatchAction.
// Runs schema validation against d.Params before dispatch, so handlers do
// not need to re-check Required-ness, types, or enum membership.
func (d *ToolDef) Execute(ctx *ToolContext, args map[string]any) (*Result, error) {
	normalizeArgAliases(d, args)
	inferred := inferMissingAction(d, args)
	if r := validateArgs(d, args); r != nil {
		return r, nil
	}
	res, err := DispatchAction(ctx, args, d.Actions)
	if inferred != "" && err == nil && res != nil && res.Success {
		attachNotice(res, fmt.Sprintf("action was missing and inferred as %q from the argument shape; pass action explicitly next time", inferred))
	}
	if err == nil && res != nil {
		action, _ := args["action"].(string)
		res.IgnoredArgs = d.ignoredArgs(action, args)
	}
	return res, err
}

// ignoredArgs lists the keys of args the named action does not declare. The handler
// never reads them, so without this a misnamed or misplaced setting (a page title
// passed top-level instead of inside metadata, write_mode spelled mode) returns a
// clean success while nothing was applied. Each entry carries the most useful
// correction known: the sibling action that does take it, a nesting the action
// expects, or the closest declared name. Aliases the call was normalised from and
// keys starting with "_" are not reported. Nil when every key was used.
func (d *ToolDef) ignoredArgs(action string, args map[string]any) []string {
	meta, ok := d.ActionMeta[action]
	if !ok || len(args) == 0 {
		return nil
	}
	declared := map[string]bool{"action": true}
	for _, p := range meta.Params {
		declared[p.Name] = true
		for _, alias := range argAliases[p.Name] {
			declared[alias] = true
		}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		if !declared[k] && !strings.HasPrefix(k, "_") {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+d.ignoredArgSuggestion(action, k, declared))
	}
	return out
}

// metadataArgs are keys a model commonly passes top-level that an action taking a
// `metadata` object reads from inside it.
var metadataArgs = map[string]bool{"title": true, "description": true, "layout": true}

// hunkFieldArgs are patch-hunk keys a model commonly sends beside `patches` instead of in it.
var hunkFieldArgs = map[string]bool{
	"search": true, "replace": true, "old_string": true, "new_string": true, "find": true,
	"insert_after": true, "insert_before": true, "text": true, "append": true, "prepend": true,
}

// ignoredArgSuggestion returns the " (...)" correction for one ignored key, or "".
func (d *ToolDef) ignoredArgSuggestion(action, key string, declared map[string]bool) string {
	// A hunk field sent at the top level belongs inside the patches array; naming
	// another action that takes the key would send the model to the wrong edit.
	if declared["patches"] && hunkFieldArgs[key] {
		return " (hunk fields go inside the patches array: patches=[{search, replace}])"
	}
	var owners []string
	for _, name := range d.actionNames() {
		if name == action {
			continue
		}
		for _, p := range d.ActionMeta[name].Params {
			if p.Name == key {
				owners = append(owners, name)
				break
			}
		}
	}
	if len(owners) > 0 {
		return fmt.Sprintf(" (only action=%s takes it)", strings.Join(owners, "|"))
	}
	if metadataArgs[key] && declared["metadata"] {
		return fmt.Sprintf(" (did you mean metadata.%s?)", key)
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		if name != "action" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	best, bestDist := "", 3
	for _, name := range names {
		if dist := editDistance(key, name); dist < bestDist {
			best, bestDist = name, dist
		}
	}
	if best != "" && len(key) > 3 {
		return fmt.Sprintf(" (did you mean %s?)", best)
	}
	return " (not an argument of this action)"
}

// editDistance is the Levenshtein distance between a and b, for did-you-mean hints.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// inferMissingAction fills a missing action when the supplied args match
// exactly one action's schema: every required param of that action is present
// and non-empty, every supplied arg is one the action declares, and the action
// requires at least one param (so a bare call never infers a list-shaped
// action). The first-call missing-action slip is a common model error, and an
// unambiguous shape costs nothing to honor. Any ambiguity
// keeps the normal rejection, and destructive handlers still enforce their own
// confirm and approval gates, so a wrong guess cannot destroy anything
// silently. Returns the inferred name, or "".
func inferMissingAction(d *ToolDef, args map[string]any) string {
	if a, _ := args["action"].(string); a != "" {
		return ""
	}
	if len(d.ActionMeta) == 0 || len(args) == 0 {
		return ""
	}
	var supplied []string
	for k, v := range args {
		if k == "action" || v == nil {
			continue
		}
		supplied = append(supplied, k)
	}
	if len(supplied) == 0 {
		return ""
	}
	match := ""
	for name, meta := range d.ActionMeta {
		declared := map[string]bool{}
		required := 0
		ok := true
		for _, p := range meta.Params {
			declared[p.Name] = true
			if !p.Required {
				continue
			}
			required++
			v, present := args[p.Name]
			if !present || v == nil || isEmptyValue(v, p.Type) {
				ok = false
			}
		}
		if !ok || required == 0 {
			continue
		}
		for _, k := range supplied {
			if !declared[k] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if match != "" {
			return "" // two actions fit; keep the normal rejection
		}
		match = name
	}
	if match != "" {
		args["action"] = match
	}
	return match
}

// attachNotice adds an advisory line to a successful result without
// disturbing an existing data shape.
func attachNotice(r *Result, note string) {
	if m, ok := r.Data.(map[string]any); ok {
		if _, exists := m["notice"]; !exists {
			m["notice"] = note
		}
		return
	}
	if r.Data == nil {
		r.Data = map[string]any{"notice": note}
	}
}

// Context returns the cancellation/timeout context for this tool call.
// Callers must populate ToolContext.Ctx; Executor.Execute sets it automatically
// before dispatching. Direct callers (jobs, chat, tests) must set it themselves.
func (tc *ToolContext) Context() context.Context {
	if tc.Ctx == nil {
		slog.Warn("ToolContext.Ctx not set; falling back to context.Background", "caller", "tool")
		return context.Background()
	}
	return tc.Ctx
}

// RequireAction extracts and validates the "action" arg. Returns the action
// string on success, or an error Result if missing/empty.
func RequireAction(args map[string]any) (string, *Result) {
	action, _ := args["action"].(string)
	if action == "" {
		return "", &Result{Success: false, Code: CodeMissingArg, Error: "action is required"}
	}
	return action, nil
}

// Ok wraps a successful payload as a *Result. Pass any JSON-marshalable value;
// pass nil if there's nothing to report. Saves the &Result{Success: true, Data: ...}
// boilerplate at every happy-path return.
func Ok(data any) *Result {
	return &Result{Success: true, Data: data}
}

// Fail formats a human-readable error into a *Result. Use for business-logic
// failures the LLM should act on (missing record, validation fail). Pair with
// FailCode when the failure has a stable machine-readable tag.
func Fail(format string, args ...any) *Result {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return &Result{Success: false, Error: msg}
}

// FailErr returns a failing *Result carrying err's message. Shorthand for the
// common Fail("%s", err.Error()) idiom (the "%s" keeps any percent signs in the
// message literal). If err (or one it wraps) carries a machine-readable code via
// a Code() string method - e.g. store.CodedErrorf - the code is surfaced on the
// Result too, so a store-layer gate's rejection classifies the same as a
// tool-layer FailCode.
func FailErr(err error) *Result {
	r := Fail("%s", err.Error())
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		r.Code = coded.Code()
	}
	return r
}

// FailCode is Fail with an explicit machine-readable Code (e.g. "not_found",
// "conflict"). The engine tool loop and UI branch on Code without substring-
// matching Error.
func FailCode(code, format string, args ...any) *Result {
	r := Fail(format, args...)
	r.Code = code
	return r
}
