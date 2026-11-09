package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"openfabrica/providers/llm"
)

// defineFixture registers a Define fixture into the default registry and removes it when
// the test ends.
//
// Define writes into the process-wide default registry and refuses a duplicate name, so a
// fixture left behind fails every repeat run inside one binary - which is exactly what
// `go test -count=2 ./tools/` does, and what blocked flake hunting on this package. The
// fixture name cannot simply be made unique instead: deriveActionName strips the tool name
// off each action type, so `manage_fake_2` would derive `fake_configure` rather than the
// `configure` these tests assert on. Dropping the registration afterwards keeps the name
// fixed AND every run exercising the real Define path.
func defineFixture(t *testing.T, tool Tool) *ToolDef {
	t.Helper()
	dropFixture(tool.Name)
	t.Cleanup(func() { dropFixture(tool.Name) })

	Define(tool)
	for _, e := range RegistrationErrors() {
		if strings.Contains(e.Error(), tool.Name) {
			t.Fatalf("registration error: %v", e)
		}
	}
	def, err := DefaultRegistry().Get(tool.Name)
	if err != nil {
		t.Fatalf("Get(%q): %v", tool.Name, err)
	}
	return def
}

func dropFixture(name string) {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	delete(defaultRegistry.tools, name)
}

// Test action structs used by the reflection/Define unit tests. Kept in this
// file (not a separate fixtures package) so they have direct access to the
// reflect-layer helpers under test.

type fakeConfigure struct {
	EndpointPath string   `arg:"endpoint_path"        desc:"Endpoint path; '*' = global."`
	Provider     string   `arg:"provider,required"    desc:"Delivery provider." enum:"email,sms,push"`
	PublicKey    string   `arg:"public_key,required"  desc:"Public key."`
	MinScore     float64  `arg:"minimum_score"        desc:"Score threshold (default 0.5)."`
	Enforced     bool     `arg:"enforced"             desc:"Hard-block when verify fails."`
	Tags         []string `arg:"tags"                desc:"Optional labels."`
}

func (a *fakeConfigure) Doc() string                           { return "Attach a provider to an endpoint." }
func (a *fakeConfigure) Run(ctx *ToolContext) (*Result, error) { return &Result{Success: true}, nil }
func (a *fakeConfigure) Example() any {
	return &fakeConfigure{Provider: "push", PublicKey: "pk_...", EndpointPath: "contact"}
}

type fakeList struct{}

func (a *fakeList) Doc() string                           { return "List provider configurations." }
func (a *fakeList) ReadOnly() bool                        { return true }
func (a *fakeList) Run(ctx *ToolContext) (*Result, error) { return &Result{Success: true}, nil }

func TestExtractActionFields_TagsAndTypes(t *testing.T) {
	fields, err := extractActionFields(reflect.TypeOf(&fakeConfigure{}))
	if err != nil {
		t.Fatalf("extractActionFields: %v", err)
	}
	want := map[string]struct {
		typ      string
		required bool
		enum     []string
	}{
		"endpoint_path": {"string", false, nil},
		"provider":      {"string", true, []string{"email", "sms", "push"}},
		"public_key":    {"string", true, nil},
		"minimum_score": {"number", false, nil},
		"enforced":      {"boolean", false, nil},
		"tags":          {"array", false, nil},
	}
	if len(fields) != len(want) {
		t.Fatalf("expected %d fields, got %d", len(want), len(fields))
	}
	for _, f := range fields {
		w, ok := want[f.ArgName]
		if !ok {
			t.Errorf("unexpected field %q", f.ArgName)
			continue
		}
		if f.Type != w.typ {
			t.Errorf("%s: type %q, want %q", f.ArgName, f.Type, w.typ)
		}
		if f.Required != w.required {
			t.Errorf("%s: required %v, want %v", f.ArgName, f.Required, w.required)
		}
		if !reflect.DeepEqual(f.Enum, w.enum) {
			t.Errorf("%s: enum %v, want %v", f.ArgName, f.Enum, w.enum)
		}
		if f.Desc == "" {
			t.Errorf("%s: desc must be non-empty", f.ArgName)
		}
	}
}

// TestExtractActionFields_PromotesEmbeddedArgs asserts that extractActionFields
// descends into anonymous embedded struct fields via reflect.VisibleFields rather
// than skipping those without an `arg` tag. Every action declared as
// `struct{ commonXxxArgs }` (manage_data, manage_api, manage_webhooks, etc.)
// depends on this: without promoted fields it registers with an empty schema, the
// LLM never learns the required arg names, and any call fails at runtime with
// "X is required".
func TestExtractActionFields_PromotesEmbeddedArgs(t *testing.T) {
	type commonInner struct {
		Kind     string `arg:"kind,required" desc:"Artifact kind." enum:"page,layout"`
		FilePath string `arg:"file_path" desc:"Path identifier."`
		Body     string `arg:"body"          desc:"Full artifact body."`
	}
	type outer struct{ commonInner }
	fields, err := extractActionFields(reflect.TypeOf(&outer{}))
	if err != nil {
		t.Fatalf("extractActionFields: %v", err)
	}
	want := map[string]bool{"kind": true, "file_path": true, "body": true}
	if len(fields) != len(want) {
		t.Fatalf("expected %d promoted fields, got %d (%+v)", len(want), len(fields), fields)
	}
	for _, f := range fields {
		if !want[f.ArgName] {
			t.Errorf("unexpected field %q", f.ArgName)
		}
	}
	// Populate via the outer type and confirm the embedded fields receive the
	// values - the second half of the failure mode: even if the schema were
	// correct, populateActionStruct must traverse the promotion path to set them.
	ptr, err := populateActionStruct(reflect.TypeOf(&outer{}), fields, map[string]any{
		"kind":      "page",
		"file_path": "/index.html",
		"body":      "<h1>hi</h1>",
	})
	if err != nil {
		t.Fatalf("populate: %v", err)
	}
	got := ptr.Interface().(*outer)
	if got.Kind != "page" || got.FilePath != "/index.html" || got.Body != "<h1>hi</h1>" {
		t.Fatalf("embedded fields not populated: %+v", got.commonInner)
	}
}

func TestExtractActionFields_RejectsMissingDesc(t *testing.T) {
	type bad struct {
		Foo string `arg:"foo"`
	}
	_, err := extractActionFields(reflect.TypeOf(&bad{}))
	if err == nil || !strings.Contains(err.Error(), "desc") {
		t.Fatalf("expected missing-desc error, got %v", err)
	}
}

func TestPopulateActionStruct_TypedFields(t *testing.T) {
	fields, err := extractActionFields(reflect.TypeOf(&fakeConfigure{}))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	args := map[string]any{
		"action":        "configure",
		"endpoint_path": "contact",
		"provider":      "push",
		"public_key":    "pk_ABC",
		"minimum_score": float64(0.7),
		"enforced":      true,
		"tags":          []any{"x", "y"},
	}
	ptr, err := populateActionStruct(reflect.TypeOf(&fakeConfigure{}), fields, args)
	if err != nil {
		t.Fatalf("populate: %v", err)
	}
	got := ptr.Interface().(*fakeConfigure)
	if got.EndpointPath != "contact" || got.Provider != "push" || got.PublicKey != "pk_ABC" {
		t.Fatalf("string fields: %+v", got)
	}
	if got.MinScore != 0.7 || got.Enforced != true {
		t.Fatalf("scalar fields: %+v", got)
	}
	if !reflect.DeepEqual(got.Tags, []string{"x", "y"}) {
		t.Fatalf("slice field: %+v", got.Tags)
	}
}

// TestPopulateActionStruct_AppliesDefaults confirms that absent args fall
// back to default-tagged values, but a present arg still wins. Also checks
// the registration-time guard: required+default is rejected.
func TestPopulateActionStruct_AppliesDefaults(t *testing.T) {
	type withDefaults struct {
		Score    float64  `arg:"score"   desc:"Score threshold."     default:"0.5"`
		Limit    int      `arg:"limit"   desc:"Max items returned."  default:"25"`
		Verbose  bool     `arg:"verbose" desc:"Enable verbose logs." default:"true"`
		Tags     []string `arg:"tags"    desc:"Optional labels."     default:"alpha,beta"`
		Override string   `arg:"label"   desc:"Display label."       default:"unset"`
	}
	fields, err := extractActionFields(reflect.TypeOf(&withDefaults{}))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Absent -> defaults applied.
	ptr, err := populateActionStruct(reflect.TypeOf(&withDefaults{}), fields, map[string]any{})
	if err != nil {
		t.Fatalf("populate: %v", err)
	}
	got := ptr.Interface().(*withDefaults)
	if got.Score != 0.5 || got.Limit != 25 || got.Verbose != true || got.Override != "unset" {
		t.Fatalf("defaults not applied: %+v", got)
	}
	if !reflect.DeepEqual(got.Tags, []string{"alpha", "beta"}) {
		t.Fatalf("slice default: %+v", got.Tags)
	}
	// Present arg wins over default.
	ptr, err = populateActionStruct(reflect.TypeOf(&withDefaults{}), fields, map[string]any{
		"score": float64(0.9),
		"label": "explicit",
	})
	if err != nil {
		t.Fatalf("populate2: %v", err)
	}
	got = ptr.Interface().(*withDefaults)
	if got.Score != 0.9 || got.Override != "explicit" {
		t.Fatalf("present args lost: %+v", got)
	}
	// Required + default is a registration error.
	type bad struct {
		X string `arg:"x,required" desc:"Bad mix." default:"v"`
	}
	if _, err := extractActionFields(reflect.TypeOf(&bad{})); err == nil {
		t.Fatalf("expected error for required+default")
	}
}

// TestMinMaxRejectAndSchema confirms the min/max tags: an out-of-range present
// value is REJECTED during population (the model learns the real bound rather
// than getting a silently-truncated value), an in-range value passes through,
// the schema renders integer minimum/maximum, and a bound on a non-numeric arg
// is a registration error.
func TestMinMaxRejectAndSchema(t *testing.T) {
	type bounded struct {
		Limit   int     `arg:"limit"   desc:"Capped." default:"50" max:"200"`
		Quality int     `arg:"quality" desc:"Bounded." default:"80" min:"1" max:"100"`
		Ratio   float64 `arg:"ratio"   desc:"Fractional." min:"0" max:"1"`
	}
	ft := reflect.TypeOf(&bounded{})
	fields, err := extractActionFields(ft)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	// Present, over max -> rejected.
	if _, err := populateActionStruct(ft, fields, map[string]any{"limit": float64(900)}); err == nil {
		t.Fatal("expected rejection for limit over max")
	}
	// Present, under min -> rejected.
	if _, err := populateActionStruct(ft, fields, map[string]any{"quality": float64(0)}); err == nil {
		t.Fatal("expected rejection for quality under min")
	}
	// Float over max -> rejected.
	if _, err := populateActionStruct(ft, fields, map[string]any{"ratio": float64(2.5)}); err == nil {
		t.Fatal("expected rejection for ratio over max")
	}

	// In-range present values pass through untouched.
	ptr, err := populateActionStruct(ft, fields, map[string]any{"limit": float64(75), "quality": float64(90)})
	if err != nil {
		t.Fatalf("in-range populate: %v", err)
	}
	got := ptr.Interface().(*bounded)
	if got.Limit != 75 || got.Quality != 90 {
		t.Fatalf("in-range rejected wrongly: %+v", got)
	}

	// Schema renders typed minimum/maximum.
	params := fieldsToParams(ft, fields)
	def := &ToolDef{Params: params}
	props := def.Parameters()["properties"].(map[string]any)
	limitProp := props["limit"].(map[string]any)
	if limitProp["maximum"] != int64(200) {
		t.Fatalf("limit maximum = %v (%T), want int64(200)", limitProp["maximum"], limitProp["maximum"])
	}
	if _, hasMin := limitProp["minimum"]; hasMin {
		t.Fatalf("limit should have no minimum")
	}

	// A bound on a non-numeric arg is rejected at registration.
	type badBound struct {
		Name string `arg:"name" desc:"Not numeric." max:"10"`
	}
	if _, err := extractActionFields(reflect.TypeOf(&badBound{})); err == nil {
		t.Fatal("expected error for max tag on string arg")
	}
}

// TestEnumOnNonStringRejected: runtime enum validation is string-only, so an
// enum tag on any other type would render string members on a differently-typed
// schema param and never be enforced. Registration must reject it.
func TestEnumOnNonStringRejected(t *testing.T) {
	type badEnum struct {
		Code int `arg:"code" desc:"Not a string." enum:"301,302"`
	}
	if _, err := extractActionFields(reflect.TypeOf(&badEnum{})); err == nil {
		t.Fatal("expected error for enum tag on integer arg")
	}

	type goodEnum struct {
		Mode string `arg:"mode" desc:"A string." enum:"a,b"`
	}
	if _, err := extractActionFields(reflect.TypeOf(&goodEnum{})); err != nil {
		t.Fatalf("enum on string arg must register: %v", err)
	}
}

// TestPopulateActionStruct_NullArrayElement guards a real crash: a tool with an
// []interface{} arg (e.g. manage_testing's test_module `args`) called with a null
// element - `args:[1,null]` - decodes to []interface{}{1.0, nil}. assignReflect
// recursed into the slice and hit the nil at the reflect.Interface case, where
// reflect.ValueOf(nil) is a zero Value and .Set panicked ("reflect.Value.Set on
// zero Value"), surfacing to the model as an opaque tool panic. The null must map
// to a nil element and round-trip, never panic.
func TestPopulateActionStruct_NullArrayElement(t *testing.T) {
	type withArgs struct {
		Args []any `arg:"args" desc:"Positional arguments."`
	}
	ft := reflect.TypeOf(&withArgs{})
	fields, err := extractActionFields(ft)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	ptr, err := populateActionStruct(ft, fields, map[string]any{
		"args": []any{float64(1), nil, "x"},
	})
	if err != nil {
		t.Fatalf("populate with null element: %v", err)
	}
	got := ptr.Interface().(*withArgs)
	if len(got.Args) != 3 {
		t.Fatalf("want 3 args, got %d (%+v)", len(got.Args), got.Args)
	}
	if got.Args[0] != float64(1) || got.Args[1] != nil || got.Args[2] != "x" {
		t.Fatalf("null element not preserved as nil: %+v", got.Args)
	}
}

func TestDeriveActionName(t *testing.T) {
	cases := []struct {
		tool, typ, want string
	}{
		{"manage_tenancy", "TenancyEnable", "enable"},
		{"manage_tenancy", "TenancySetSignupMode", "set_signup_mode"},
		{"manage_2fa", "TwoFactorEnable", "two_factor_enable"}, // no prefix match -> full snake
		{"data_query", "DataQueryRun", "run"},
		{"manage_files", "FileAttach", "attach"},
	}
	for _, tc := range cases {
		got := deriveActionName(tc.tool, tc.typ)
		if got != tc.want {
			t.Errorf("deriveActionName(%q, %q) = %q, want %q", tc.tool, tc.typ, got, tc.want)
		}
	}
}

// TestDefine_EndToEnd registers a synthetic tool through Define() and
// verifies the derived ToolDef looks like a hand-written one would: action
// enum is populated, summaries come from Doc(), examples come from Example(),
// ReadOnly is captured, and the dispatched handler receives a fully-populated
// struct.
func TestDefine_EndToEnd(t *testing.T) {
	// A name no init()-registered tool uses, so this cannot collide with the real
	// registry the test package boots. defineFixture removes it afterwards.
	const name = "manage_fake"
	def := defineFixture(t, Tool{
		Name:      name,
		Category:  "test",
		OneLiner:  "Sanity tool for Define() coverage.",
		WhenToUse: "Test-only - never exposed to the LLM.",
		Actions: []Action{
			&fakeConfigure{},
			&fakeList{},
		},
	})
	if _, ok := def.Actions["configure"]; !ok {
		t.Fatalf("missing action 'configure'; have %v", actionNames(def))
	}
	if _, ok := def.Actions["list"]; !ok {
		t.Fatalf("missing action 'list'; have %v", actionNames(def))
	}
	if def.ActionMeta["configure"].Summary == "" {
		t.Fatalf("configure summary not derived from Doc()")
	}
	if !def.ActionMeta["list"].ReadOnly {
		t.Fatalf("list should be marked ReadOnly")
	}

	// Dispatch a real call and confirm the populated-struct handler runs.
	res, err := def.Execute(&ToolContext{Ctx: context.Background()}, map[string]any{
		"action":     "configure",
		"provider":   "push",
		"public_key": "pk_ABC",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %+v", res)
	}

	// Missing required field surfaces as a per-action missing_arg.
	res, _ = def.Execute(&ToolContext{Ctx: context.Background()}, map[string]any{
		"action": "configure",
	})
	if res.Success || res.Code != "missing_arg" {
		t.Fatalf("expected missing_arg, got %+v", res)
	}
}

// TestDefine_FindToolZeroTouch proves the design promise: defining a brand-new
// tool requires zero changes to find_tool. The tool becomes discoverable via
// Registry.Search and loadable via Registry.LookupFull immediately, with the
// derived schema (action enum + per-action params merged) intact.
func TestDefine_FindToolZeroTouch(t *testing.T) {
	name := "manage_zerotouch"
	Define(Tool{
		Name:      name,
		Category:  "test",
		OneLiner:  "Zero-touch find_tool discovery proof.",
		WhenToUse: "Test-only.",
		Actions:   []Action{&fakeConfigure{}, &fakeList{}},
	})
	caps := llm.Capabilities{}
	matches := DefaultRegistry().HybridSearch("zerotouch", nil, caps, 5, "")
	found := false
	for _, m := range matches {
		if m.Name == name {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Search did not surface %q; got %+v", name, matches)
	}
	defs := DefaultRegistry().LookupFull([]string{name}, caps, "")
	if len(defs) != 1 || defs[0].Name != name {
		t.Fatalf("LookupFull did not return %q; got %+v", name, defs)
	}
	// Schema must include the derived action enum.
	props, _ := defs[0].Parameters["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	enum, _ := action["enum"].([]string)
	if len(enum) != 2 {
		t.Fatalf("expected derived action enum of size 2, got %+v", enum)
	}
}

func actionNames(d *ToolDef) []string {
	out := make([]string, 0, len(d.Actions))
	for k := range d.Actions {
		out = append(out, k)
	}
	return out
}

// fakeDefaults exercises every default-tag type for the schema-rendering test.
type fakeDefaults struct {
	Mode    string  `arg:"mode"    desc:"Provider family."          default:"generic"`
	Timeout int     `arg:"timeout" desc:"Seconds before giving up." default:"30"`
	Ratio   float64 `arg:"ratio"   desc:"Sampling ratio."           default:"0.5"`
	Loud    bool    `arg:"loud"    desc:"Verbose logging."          default:"true"`
	Plain   string  `arg:"plain"   desc:"No default here."`
}

func (a *fakeDefaults) Doc() string                           { return "Defaults-rendering fixture." }
func (a *fakeDefaults) Run(ctx *ToolContext) (*Result, error) { return &Result{Success: true}, nil }

// TestParameters_RendersTypedDefaults proves Define -> Parameters() surfaces an
// arg's `default` tag into the JSON schema the LLM reads, coerced to the param's
// declared type (true not "true", 30 not "30"). Without this the model never
// learns the effective value of an omitted optional arg.
func TestParameters_RendersTypedDefaults(t *testing.T) {
	def := defineFixture(t, Tool{
		Name: "manage_defaults_fixture", Category: "test",
		OneLiner: "Defaults rendering fixture.", WhenToUse: "Test-only.",
		Actions: []Action{&fakeDefaults{}},
	})
	props, _ := def.Parameters()["properties"].(map[string]any)
	want := map[string]any{
		"mode":    "generic",
		"timeout": int64(30),
		"ratio":   0.5,
		"loud":    true,
	}
	for arg, wantDef := range want {
		p, _ := props[arg].(map[string]any)
		got, ok := p["default"]
		if !ok {
			t.Errorf("%s: schema carries no default", arg)
			continue
		}
		if !reflect.DeepEqual(got, wantDef) {
			t.Errorf("%s: default = %#v (%T), want %#v (%T)", arg, got, got, wantDef, wantDef)
		}
	}
	// An arg without a default tag must not emit a default key.
	if p, _ := props["plain"].(map[string]any); p != nil {
		if _, ok := p["default"]; ok {
			t.Errorf("plain: schema has an unexpected default key")
		}
	}
}
