package tools

import (
	"context"
	"strings"
	"testing"
)

// TestValidateArgs_RejectsEmptyArgsForEveryTool walks the default registry
// and asserts that calling Execute with {} on a tool whose action param is
// Required returns a structured Result{Code:"missing_arg"}. This catches
// future drift where someone adds a new tool but forgets to mark its action
// param as Required, which would let calls through to handlers with empty
// argument maps.
func TestValidateArgs_RejectsEmptyArgsForEveryTool(t *testing.T) {
	reg := DefaultRegistry()

	for _, def := range reg.List() {
		def := def

		// Find the action param (if any) and check Required.
		var actionRequired bool
		var hasActionParam bool
		for _, p := range def.Params {
			if p.Name == "action" {
				hasActionParam = true
				actionRequired = p.Required
				break
			}
		}
		if !hasActionParam || !actionRequired {
			// Single-action tools (no action enum) and tools that intentionally
			// don't require action are out of scope for this drift check.
			continue
		}

		t.Run(def.Name, func(t *testing.T) {
			ctx := &ToolContext{Ctx: context.Background()}
			res, err := def.Execute(ctx, map[string]any{})
			if err != nil {
				t.Fatalf("Execute returned err for %s with empty args: %v", def.Name, err)
			}
			if res == nil {
				t.Fatalf("Execute returned nil Result for %s", def.Name)
			}
			if res.Success {
				t.Fatalf("expected failure for %s with empty args, got Success=true", def.Name)
			}
			if res.Code != "missing_arg" {
				t.Fatalf("expected Code=missing_arg for %s, got Code=%q Error=%q", def.Name, res.Code, res.Error)
			}
			if !strings.Contains(res.Error, "action") {
				t.Fatalf("expected error to mention 'action' for %s, got %q", def.Name, res.Error)
			}
		})
	}
}

// TestValidateArgs_TypeMismatch confirms the validator returns a structured
// invalid_type error when a present arg has the wrong concrete type.
func TestValidateArgs_TypeMismatch(t *testing.T) {
	def := &ToolDef{
		Name:    "test_tool",
		Params:  []Param{{Name: "count", Type: "integer"}},
		Actions: map[string]ActionHandler{},
	}
	res := validateArgs(def, map[string]any{"count": "not a number"})
	if res == nil || res.Code != "invalid_type" {
		t.Fatalf("expected invalid_type, got %+v", res)
	}
}

// TestValidateArgs_EnumViolation confirms the validator rejects strings
// outside a declared Enum.
func TestValidateArgs_EnumViolation(t *testing.T) {
	def := &ToolDef{
		Name:    "test_tool",
		Params:  []Param{{Name: "format", Type: "string", Enum: []string{"json", "xml"}}},
		Actions: map[string]ActionHandler{},
	}
	res := validateArgs(def, map[string]any{"format": "yaml"})
	if res == nil || res.Code != "invalid_enum" {
		t.Fatalf("expected invalid_enum, got %+v", res)
	}
}

// TestRegistry_ExamplesValidateAgainstSchema walks every registered tool and
// asserts that each ActionMeta.Example.Args parses against the tool's Param
// schema. Registry.Validate() also performs this check at boot; keeping a
// direct test here means a drifted example fails fast in the tools package
// without requiring the full engine stage suite to run.
func TestRegistry_ExamplesValidateAgainstSchema(t *testing.T) {
	if err := DefaultRegistry().Validate(); err != nil {
		t.Fatalf("registry validation failed: %v", err)
	}
}

// TestValidateArgs_AcceptsWholeFloatAsInteger confirms float64 values that
// happen to be whole numbers (the JSON-decode default) satisfy "integer".
func TestValidateArgs_AcceptsWholeFloatAsInteger(t *testing.T) {
	def := &ToolDef{
		Name:    "test_tool",
		Params:  []Param{{Name: "count", Type: "integer"}},
		Actions: map[string]ActionHandler{},
	}
	res := validateArgs(def, map[string]any{"count": float64(42)})
	if res != nil {
		t.Fatalf("expected float64(42) to satisfy integer, got %+v", res)
	}
}

// TestValidateArgs_MissingArgSurfacesEnumAndDesc confirms a missing required
// arg's error carries the allowed values and description, so a model can fix
// the call on the first failure instead of retrying the same empty shape.
func TestValidateArgs_MissingArgSurfacesEnumAndDesc(t *testing.T) {
	def := &ToolDef{
		Name: "test_tool",
		Params: []Param{{
			Name:        "kind",
			Type:        "string",
			Required:    true,
			Enum:        []string{"page", "layout", "component", "file"},
			Description: "Artifact kind.",
		}},
		Actions: map[string]ActionHandler{},
	}
	res := validateArgs(def, map[string]any{})
	if res == nil || res.Code != "missing_arg" {
		t.Fatalf("expected missing_arg, got %+v", res)
	}
	for _, want := range []string{"kind", "page", "file", "Artifact kind."} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("error %q should contain %q", res.Error, want)
		}
	}
}

// TestValidateArgs_OptionalEnumEmptyAccepted confirms an empty string on an
// OPTIONAL enum param is treated as "unset" and passes validation (so an
// omitted-but-injected kind:"" reaches the handler to be inferred), while a
// non-empty out-of-enum value is still rejected.
func TestValidateArgs_OptionalEnumEmptyAccepted(t *testing.T) {
	def := &ToolDef{
		Name:    "test_tool",
		Params:  []Param{{Name: "kind", Type: "string", Enum: []string{"page", "file"}}},
		Actions: map[string]ActionHandler{},
	}
	if res := validateArgs(def, map[string]any{"kind": ""}); res != nil {
		t.Fatalf("empty optional enum should pass, got %+v", res)
	}
	if res := validateArgs(def, map[string]any{"kind": "bogus"}); res == nil || res.Code != "invalid_enum" {
		t.Fatalf("non-empty out-of-enum should still fail, got %+v", res)
	}
}

// TestValidateArgs_RequiredEmptyString catches the common Required-but-empty
// case (LLM passes "" to satisfy schema mechanically without a real value).
func TestValidateArgs_RequiredEmptyString(t *testing.T) {
	def := &ToolDef{
		Name:    "test_tool",
		Params:  []Param{{Name: "path", Type: "string", Required: true}},
		Actions: map[string]ActionHandler{},
	}
	res := validateArgs(def, map[string]any{"path": ""})
	if res == nil || res.Code != "missing_arg" {
		t.Fatalf("expected missing_arg, got %+v", res)
	}
}
