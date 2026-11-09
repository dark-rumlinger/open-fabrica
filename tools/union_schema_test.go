package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Fixture actions for the union-schema and ignored-arg tests.

type widgetCreate struct {
	Name     string         `arg:"name,required" desc:"Widget name."`
	Size     int            `arg:"size"          desc:"Widget size." default:"3"`
	Metadata map[string]any `arg:"metadata"      desc:"Free-form metadata."`
}

func (a *widgetCreate) ActionName() string { return "create" }
func (a *widgetCreate) Doc() string        { return "Create a widget." }
func (a *widgetCreate) Example() any       { return &widgetCreate{Name: "w"} }
func (a *widgetCreate) Run(*ToolContext) (*Result, error) {
	return Ok(map[string]any{"name": a.Name}), nil
}

type widgetUpdate struct {
	Name string `arg:"name,required" desc:"Widget name."`
	Size int    `arg:"size"          desc:"Pass -1 to reset the size to its platform default value."`
}

func (a *widgetUpdate) ActionName() string { return "update" }
func (a *widgetUpdate) Doc() string        { return "Update a widget." }
func (a *widgetUpdate) Example() any       { return &widgetUpdate{Name: "w"} }
func (a *widgetUpdate) Run(*ToolContext) (*Result, error) {
	return Fail("widget %q not found", a.Name), nil
}

type widgetList struct{}

func (a *widgetList) ActionName() string { return "list" }
func (a *widgetList) Doc() string        { return "List widgets." }
func (a *widgetList) ReadOnly() bool     { return true }
func (a *widgetList) Run(*ToolContext) (*Result, error) {
	var rows []map[string]any // nil: no widgets
	return Ok(rows), nil
}

func widgetRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := DefaultRegistry().Clone()
	if err := reg.Define(Tool{
		Name:     "manage_widget_fixture",
		Category: "project",
		OneLiner: "Fixture tool for the union-schema tests.",
		Actions:  []Action{&widgetCreate{}, &widgetUpdate{}, &widgetList{}},
		Hints: func(action string, _ map[string]any, r *Result) string {
			if action == "update" && r != nil && !r.Success {
				return "list the widgets to find the right name"
			}
			return ""
		},
	}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func unionParam(t *testing.T, reg *Registry, name string) Param {
	t.Helper()
	def, err := reg.Get("manage_widget_fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range def.Params {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("param %q not in the union", name)
	return Param{}
}

// The union shows a default only when every action agrees on it, keeps every action's
// distinct meaning, and names the actions a partial param belongs to.
func TestUnionParamReconciliation(t *testing.T) {
	reg := widgetRegistry(t)

	size := unionParam(t, reg, "size")
	if size.HasDefault {
		t.Errorf("size: create defaults to 3 but update does not, so the union must carry no default, got %q", size.Default)
	}
	if !strings.HasPrefix(size.Description, "[all but list] ") {
		t.Errorf("size: description should name its actions, got %q", size.Description)
	}
	if !strings.Contains(size.Description, "update: Pass -1 to reset") {
		t.Errorf("size: update's own meaning must reach the union, got %q", size.Description)
	}

	name := unionParam(t, reg, "name")
	if strings.Contains(name.Description, "|") {
		t.Errorf("name: identical texts must collapse to one, got %q", name.Description)
	}

	def, _ := reg.Get("manage_widget_fixture")
	if notes := def.SchemaNotes(); !strings.Contains(notes, "`manage_widget_fixture.create`: size defaults to 3") {
		t.Errorf("the per-action default dropped from the union must be listed in SchemaNotes, got %q", notes)
	}
}

func execWidget(t *testing.T, reg *Registry, args map[string]any) (*Result, string) {
	t.Helper()
	res, out, err := NewExecutor(reg).execute(context.Background(), &ToolContext{}, "manage_widget_fixture", args)
	if err != nil {
		t.Fatal(err)
	}
	return res, out
}

// An argument the chosen action does not declare is reported, with the correction known.
func TestIgnoredArgsAreReported(t *testing.T) {
	reg := widgetRegistry(t)
	res, _ := execWidget(t, reg, map[string]any{"action": "create", "name": "w", "title": "Hi", "nme": "x"})
	got := strings.Join(res.IgnoredArgs, "; ")
	if !strings.Contains(got, "title (did you mean metadata.title?)") {
		t.Errorf("a top-level title on an action taking metadata should point at metadata.title, got %q", got)
	}
	if !strings.Contains(got, "nme (not an argument of this action)") && !strings.Contains(got, "nme (did you mean") {
		t.Errorf("an unknown arg must be reported, got %q", got)
	}

	res, _ = execWidget(t, reg, map[string]any{"action": "update", "name": "w", "metadata": map[string]any{}})
	if got := strings.Join(res.IgnoredArgs, "; "); !strings.Contains(got, "metadata (only action=create takes it)") {
		t.Errorf("an arg another action owns must say which, got %q", got)
	}

	res, _ = execWidget(t, reg, map[string]any{"action": "create", "name": "w"})
	if res.IgnoredArgs != nil {
		t.Errorf("a clean call reports nothing, got %v", res.IgnoredArgs)
	}
}

// Hints reach a failed call (top-level hint), and an empty list marshals as [].
func TestFailureHintAndEmptyList(t *testing.T) {
	reg := widgetRegistry(t)
	res, _ := execWidget(t, reg, map[string]any{"action": "update", "name": "nope"})
	if res.Success || res.Hint != "list the widgets to find the right name" {
		t.Errorf("a failure-recovery hint must ride the failed result, got success=%v hint=%q", res.Success, res.Hint)
	}

	_, out := execWidget(t, reg, map[string]any{"action": "list"})
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatal(err)
	}
	if data, ok := decoded["data"].([]any); !ok || len(data) != 0 {
		t.Errorf("an empty list must marshal as [], got %s", out)
	}
}
