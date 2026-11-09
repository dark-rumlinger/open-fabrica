package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// panicProvider panics on its first call, then behaves like the scripted fake.
type panicProvider struct {
	enginetest.Provider
	panicked bool
}

func (p *panicProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if !p.panicked {
		p.panicked = true
		panic("provider client blew up")
	}
	return p.Provider.Complete(ctx, req)
}

// A panic inside a LIVE chat wake must not unwind the worker goroutine (LIVE
// sessions run from the command loop, outside runStageSafely) and leave the
// project with no worker. It must be recovered at the command boundary, release
// the session's in-flight mark, and leave the worker serving.
func TestLiveChatPanicLeavesWorkerServing(t *testing.T) {
	dir := t.TempDir()
	gdb, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	defer gdb.Close()
	mgr := db.NewProjectDBManager(dir)
	defer mgr.CloseAll()
	if _, err := gdb.ExecWrite(
		`INSERT INTO projects (id, name, description, published, mode, llm_model_id, ambition_level)
		 VALUES (1, 'Tip Calc', 'a tip calculator', 1, 'live', 1, 'off')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	provider := &panicProvider{}
	deps := &Deps{
		DB:               gdb,
		ProjectDBManager: mgr,
		ToolRegistry:     tools.DefaultRegistry(),
		ToolExecutor:     tools.NewExecutor(tools.DefaultRegistry()),
	}
	w, err := newWorker(deps, 1, newLLMLimiter(2), context.Background())
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	defer w.dbLog.Close()
	w.providerOverride = staticProviderSource{rp: resolvedProvider{Provider: provider, Model: "fake-model", ProviderID: 1}}
	w.setState(StateLive)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	chat := Command{Type: CommandChat, ProjectID: 1, Payload: map[string]any{"message": "make the heading blue"}}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic escaped the command boundary: %v", r)
			}
		}()
		w.handleCommandSafely(ctx, chat)
	}()
	if !provider.panicked {
		t.Fatal("the scripted panic never fired; the test exercised nothing")
	}
	if n := w.sessionsInFlight.Load(); n != 0 {
		t.Fatalf("sessionsInFlight = %d after a recovered panic, want 0", n)
	}
	if got := w.State(); got != StateLive {
		t.Fatalf("state = %v after a recovered panic, want live", got)
	}

	// The same worker serves the next message normally.
	before := provider.CallCount()
	w.handleCommandSafely(ctx, chat)
	if provider.CallCount() == before {
		t.Fatal("worker did not serve the chat message after the recovered panic")
	}
}
