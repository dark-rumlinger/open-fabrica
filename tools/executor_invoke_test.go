package tools_test

import (
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openfabrica/events"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

type sleepAction struct{}

func (a *sleepAction) ActionName() string { return "sleep" }
func (a *sleepAction) Doc() string        { return "Block until the call's deadline." }
func (a *sleepAction) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	select {
	case <-ctx.Ctx.Done():
	case <-time.After(5 * time.Second):
	}
	return tools.Ok(nil), nil
}

type panicAction struct{}

func (a *panicAction) ActionName() string { return "boom" }
func (a *panicAction) Doc() string        { return "Panic." }
func (a *panicAction) Run(*tools.ToolContext) (*tools.Result, error) {
	panic("kaboom")
}

type okAction struct{}

func (a *okAction) ActionName() string { return "ok" }
func (a *okAction) Doc() string        { return "Succeed." }
func (a *okAction) Run(*tools.ToolContext) (*tools.Result, error) {
	return tools.Ok(map[string]any{"done": true}), nil
}

func fixtureRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, def := range []tools.Tool{
		{Name: "fixture_slow", Category: "test", OneLiner: "Slow fixture.", WhenToUse: "Tests.",
			MaxDuration: 50 * time.Millisecond, Actions: []tools.Action{&sleepAction{}}},
		{Name: "fixture_panic", Category: "test", OneLiner: "Panicking fixture.", WhenToUse: "Tests.",
			Actions: []tools.Action{&panicAction{}}},
		{Name: "fixture_ok", Category: "test", OneLiner: "Succeeding fixture.", WhenToUse: "Tests.",
			Actions: []tools.Action{&okAction{}}},
	} {
		if err := reg.Define(def); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// Invoke owns the per-tool deadline: a tool's MaxDuration applies on every
// caller (engine, chat, jobs, triggers), not only where a caller remembered it.
func TestInvokeAppliesToolMaxDuration(t *testing.T) {
	h := tooltest.New(t).WithRegistry(fixtureRegistry(t))
	if got := h.Executor.Timeout("fixture_slow"); got != 50*time.Millisecond {
		t.Fatalf("Timeout(fixture_slow) = %v, want its MaxDuration", got)
	}
	if got := h.Executor.Timeout("fixture_ok"); got != tools.DefaultToolTimeout {
		t.Fatalf("Timeout(fixture_ok) = %v, want DefaultToolTimeout", got)
	}

	start := time.Now()
	res := h.Call("fixture_slow", map[string]any{"action": "sleep"})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("slow tool ran %v, MaxDuration was not applied", elapsed)
	}
	if res.Success {
		t.Fatal("a call cut off by its deadline must not report success")
	}
}

func TestInvokeRecoversPanic(t *testing.T) {
	h := tooltest.New(t).WithRegistry(fixtureRegistry(t))
	res := h.MustFail("fixture_panic", map[string]any{"action": "boom"}, "")
	if !strings.Contains(res.Error, "panicked") {
		t.Fatalf("error = %q, want the panic reported", res.Error)
	}
}

func TestInvokeReportsTimeoutError(t *testing.T) {
	h := tooltest.New(t).WithRegistry(fixtureRegistry(t))
	_, out, err := h.Executor.Invoke(t.Context(), h.Env, tools.Call{
		Name: "fixture_slow", Args: map[string]any{"action": "sleep"},
		ProjectID: tooltest.ProjectID, Project: h.Project,
	})
	if !errors.Is(err, tools.ErrToolTimeout) {
		t.Fatalf("err = %v, want ErrToolTimeout", err)
	}
	if !strings.Contains(out, "timed out") {
		t.Fatalf("model-facing JSON = %s, want the timeout named", out)
	}
}

// A headless env (triggers, jobs) publishes no tool events: one raised inside a
// trigger action could match a trigger on that event name and loop.
func TestHeadlessEnvPublishesNothing(t *testing.T) {
	bus := events.NewBus()
	var seen atomic.Int32
	bus.SubscribeAll(func(events.Event) { seen.Add(1) })

	h := tooltest.New(t).WithRegistry(fixtureRegistry(t)).WithBus(bus)
	h.MustOK("fixture_ok", map[string]any{"action": "ok"})
	if seen.Load() == 0 {
		t.Fatal("an env with a bus should publish tool.executed")
	}

	seen.Store(0)
	h.Env = h.Env.Headless()
	h.MustOK("fixture_ok", map[string]any{"action": "ok"})
	if n := seen.Load(); n != 0 {
		t.Fatalf("headless env published %d events, want 0", n)
	}
}

type errAction struct{}

func (a *errAction) ActionName() string { return "fail" }
func (a *errAction) Doc() string        { return "Return an error." }
func (a *errAction) Run(*tools.ToolContext) (*tools.Result, error) {
	return nil, errors.New(`constraint failed: UNIQUE on "users"."email"`)
}

// A handler that returns an error reaches the model as the same structured failure a
// returned Fail does: valid JSON with the quotes escaped, success:false, and the tool's
// recovery hint, never a hand-formatted string.
func TestInvokeHandlerErrorIsStructuredAndHinted(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Define(tools.Tool{
		Name: "fixture_err", Category: "test", OneLiner: "Erroring fixture.", WhenToUse: "Tests.",
		Actions: []tools.Action{&errAction{}},
		Hints: func(action string, _ map[string]any, r *tools.Result) string {
			if r.Success {
				return ""
			}
			return "recovery note"
		},
	}); err != nil {
		t.Fatal(err)
	}
	h := tooltest.New(t).WithRegistry(reg)
	res, out, err := h.Executor.Invoke(t.Context(), h.Env, tools.Call{
		Name: "fixture_err", Args: map[string]any{"action": "fail"},
		ProjectID: tooltest.ProjectID, Project: h.Project,
	})
	if err == nil {
		t.Fatal("a handler error must still surface as an error to the caller")
	}
	var decoded struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Hint    string `json:"hint"`
	}
	if jerr := json.Unmarshal([]byte(out), &decoded); jerr != nil {
		t.Fatalf("model-facing JSON is invalid: %v\n%s", jerr, out)
	}
	if decoded.Success || !strings.Contains(decoded.Error, `"users"."email"`) {
		t.Fatalf("decoded = %+v, want success:false with the exact error text", decoded)
	}
	if decoded.Hint != "recovery note" || res.Hint != "recovery note" {
		t.Fatalf("hint = %q / %q, want the tool's recovery note on a handler error", decoded.Hint, res.Hint)
	}
}
