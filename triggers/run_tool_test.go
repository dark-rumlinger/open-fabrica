package triggers

import (
	"log/slog"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/tools"
)

type triggerPanicAction struct{}

func (a *triggerPanicAction) ActionName() string { return "send" }
func (a *triggerPanicAction) Doc() string        { return "Panic mid-send." }
func (a *triggerPanicAction) Run(*tools.ToolContext) (*tools.Result, error) {
	panic("provider client blew up")
}

type triggerHandleAction struct{}

func (a *triggerHandleAction) ActionName() string { return "probe" }
func (a *triggerHandleAction) Doc() string        { return "Report the DB handle type." }
func (a *triggerHandleAction) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var n int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_triggers").Scan(&n); err != nil {
		return tools.Fail("read: %v", err), nil
	}
	return tools.Ok(nil), nil
}

func triggerToolRunner(t *testing.T) *Runner {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	if _, err := mgr.Create(1); err != nil {
		t.Fatalf("create project db: %v", err)
	}
	reg := tools.NewRegistry()
	for _, def := range []tools.Tool{
		{Name: "fixture_send", Category: "test", OneLiner: "Panicking sender.", WhenToUse: "Tests.",
			Actions: []tools.Action{&triggerPanicAction{}}},
		{Name: "fixture_probe", Category: "test", OneLiner: "DB probe.", WhenToUse: "Tests.",
			Actions: []tools.Action{&triggerHandleAction{}}},
	} {
		if err := reg.Define(def); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{projectDBMgr: mgr, logger: slog.New(slog.DiscardHandler)}
	r.SetTools(tools.NewExecutor(reg), &tools.Env{DataDir: t.TempDir()})
	return r
}

// A tool panicking inside a trigger action must not unwind the goroutine. runTool
// goes through Executor.Invoke, which recovers it into an ordinary action error the
// trigger logger records.
func TestRunToolRecoversToolPanic(t *testing.T) {
	r := triggerToolRunner(t)
	err := r.runTool(1, "fixture_send", map[string]any{"action": "send"})
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("runTool err = %v, want the recovered panic", err)
	}
}

// runTool resolves the project's routed handle itself rather than trusting a
// raw writer passed down by the caller.
func TestRunToolUsesProjectHandle(t *testing.T) {
	r := triggerToolRunner(t)
	if err := r.runTool(1, "fixture_probe", map[string]any{"action": "probe"}); err != nil {
		t.Fatalf("runTool: %v", err)
	}
	if err := r.runTool(99, "fixture_probe", map[string]any{"action": "probe"}); err == nil {
		t.Fatal("runTool on a project with no open database should fail, not run")
	}
}
