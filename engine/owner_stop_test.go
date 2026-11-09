package engine

// Owner stop: the chat stop button asks the pipeline to wind up at the next
// safe boundary. The mechanism is a flag rather than a command because the
// command channel is drained only between drives, so a command sent mid-build
// arrives after the build it was meant to interrupt. These cover both halves:
// the session loop ending its segment, and drive() parking the run resumably.

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/events"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// TestSessionOwnerStopBeforeFirstCall: a stop already pending when the session
// starts costs nothing - the loop ends before it commits to a single call.
func TestSessionOwnerStopBeforeFirstCall(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(echoCall("c1", "never reached")),
	}}
	s := testSession(t, p)
	s.stopFn = func() bool { return true }

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Paused || res.Ending != EndPaused || res.EndDetail != endOwnerStop.detail {
		t.Fatalf("paused=%v ending=%q detail=%q, want a paused owner_stop", res.Paused, res.Ending, res.EndDetail)
	}
	if p.CallCount() != 0 {
		t.Fatalf("provider calls = %d, want 0: a pending stop must not buy another turn", p.CallCount())
	}
}

// TestSessionOwnerStopEndsSegmentAfterTurn: a stop arriving mid-turn lets that
// turn's tool calls finish, then ends the segment. The next turn is never
// bought, and the ending names the stop rather than a budget or a question.
func TestSessionOwnerStopEndsSegmentAfterTurn(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(echoCall("c1", "first")),
		enginetest.Calls(echoCall("c2", "must not run")),
		enginetest.Text("must not run either"),
	}}
	s := testSession(t, p)
	// Stop lands while the first turn is in flight.
	s.stopFn = func() bool { return p.CallCount() >= 1 }

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Paused || res.Ending != EndPaused || res.EndDetail != endOwnerStop.detail {
		t.Fatalf("paused=%v ending=%q detail=%q, want a paused owner_stop", res.Paused, res.Ending, res.EndDetail)
	}
	if p.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1: the segment must end before buying another turn", p.CallCount())
	}
	// The turn that was already in flight ran to completion, so its work is not
	// half-applied: a stop is a clean boundary, not an abort.
	if res.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1: the in-flight turn must finish", res.ToolCalls)
	}
}

// TestSessionNoStopFnRunsNormally: the hook is optional, and a session without
// one behaves exactly as before.
func TestSessionNoStopFnRunsNormally(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(echoCall("c1", "first")),
		enginetest.Text("all done"),
	}}
	s := testSession(t, p)

	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Ending != EndDone || res.Paused {
		t.Fatalf("ending=%q paused=%v, want a clean done", res.Ending, res.Paused)
	}
}

// stopAfterCall wraps a scripted provider and fires a hook once it has served
// N completions, so a stop can be requested from inside the run without racing
// it from a goroutine.
type stopAfterCall struct {
	*enginetest.Provider
	after int
	fire  func()
	once  sync.Once
}

func (p *stopAfterCall) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	resp, err := p.Provider.Complete(ctx, req)
	if p.Provider.CallCount() >= p.after {
		p.once.Do(p.fire)
	}
	return resp, err
}

// newStopTestWorker wires a worker against temp DBs and a building project.
func newStopTestWorker(t *testing.T) (*Worker, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	gdb, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { gdb.Close() })
	mgr := db.NewProjectDBManager(dir)
	t.Cleanup(mgr.CloseAll)

	if _, err := gdb.ExecWrite(
		`INSERT INTO projects (id, name, description, published, mode, llm_model_id, ambition_level)
		 VALUES (1, 'Tip Calc', 'a one-page tip calculator', 1, 'building', 1, 'off')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	deps := &Deps{
		DB:               gdb,
		ProjectDBManager: mgr,
		ToolRegistry:     tools.DefaultRegistry(),
		ToolExecutor:     tools.NewExecutor(tools.DefaultRegistry()),
	}
	worker, err := newWorker(deps, 1, newLLMLimiter(2), context.Background())
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	t.Cleanup(func() { worker.dbLog.Close() })

	// No validators: the runtime probe needs a live server.
	validatorMu.Lock()
	saved := validatorSet
	validatorSet = nil
	validatorMu.Unlock()
	t.Cleanup(func() {
		validatorMu.Lock()
		validatorSet = saved
		validatorMu.Unlock()
	})

	return worker, gdb
}

// TestDriveOwnerStopParksRunResumably: a stop during the pipeline parks the run
// paused with the owner-stop reason, mirrors the pause into projects.mode, and
// leaves the worker alive - the whole point of an interrupt over StopProject.
// The stage that had already completed keeps its progress.
func TestDriveOwnerStopParksRunResumably(t *testing.T) {
	worker, gdb := newStopTestWorker(t)

	// PLAN completes, then the stop lands. BUILD must never start.
	scripted := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass
		enginetest.Text(planJSON),
		enginetest.Calls(llm.ToolCall{ID: "c1", Name: "manage_files",
			Arguments: `{"action":"save","kind":"page","file_path":"/","body":"<h1>nope</h1>","title":"nope"}`}),
	}}
	provider := &stopAfterCall{Provider: scripted, after: 3, fire: worker.requestStop}
	worker.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: provider, Model: "fake-model", ProviderID: 1}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	worker.setState(StateBuilding)
	worker.drive(ctx)

	if got := worker.State(); got != StatePaused {
		t.Fatalf("worker state = %v, want paused", got)
	}
	if scripted.CallCount() != 3 {
		t.Fatalf("provider calls = %d, want 3 (PLAN only): BUILD ran after the stop", scripted.CallCount())
	}

	run, err := activeRun(worker.pdb)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if run == nil {
		t.Fatal("no active run: an owner stop must leave the run resumable, not end it")
	}
	if run.Status != RunStatusPaused || run.PauseReason != PauseReasonOwnerStop {
		t.Fatalf("run status=%q reason=%q, want paused/%s", run.Status, run.PauseReason, PauseReasonOwnerStop)
	}
	// PLAN finished before the stop landed, so the resume must not pay for it again.
	if run.Stage != StageBuild {
		t.Fatalf("run stage = %q, want %q: a completed stage must keep its progress", run.Stage, StageBuild)
	}

	var mode string
	if err := gdb.QueryRow("SELECT mode FROM projects WHERE id = 1").Scan(&mode); err != nil {
		t.Fatalf("read mode: %v", err)
	}
	if mode != "paused" {
		t.Fatalf("projects.mode = %q, want paused: the stop must be sticky against wakes", mode)
	}

	// The request is consumed, so the next build is not killed on arrival.
	if worker.stopRequested() {
		t.Fatal("stop request still set after it was acted on")
	}
}

// TestDriveOwnerStopMidBuildLabelsTheStop: a stop landing inside the BUILD
// stage must be recorded as a stop, not as an owner question. BUILD leaves an
// owner stop to drive() and parks only genuine questions itself, so drive is
// the single authority for the owner_stop reason - labelling this one
// awaiting_owner_answers would leave the run waiting on an answer that is never
// coming.
func TestDriveOwnerStopMidBuildLabelsTheStop(t *testing.T) {
	worker, _ := newStopTestWorker(t)

	// Calls 1-2 are PLAN (pre-pass + main); call 3 is the first BUILD turn, and
	// the stop lands while it is in flight.
	scripted := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures,
		enginetest.Text(planJSON),
		enginetest.Calls(llm.ToolCall{ID: "c1", Name: "manage_files",
			Arguments: `{"action":"save","kind":"page","file_path":"/","body":"<h1>Tip calculator</h1>","title":"Tip calculator"}`}),
		enginetest.Calls(llm.ToolCall{ID: "c2", Name: "manage_tasks",
			Arguments: `{"action":"complete","task_id":"home-page","result":"must not run"}`}),
	}}
	provider := &stopAfterCall{Provider: scripted, after: 3, fire: worker.requestStop}
	worker.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: provider, Model: "fake-model", ProviderID: 1}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	worker.setState(StateBuilding)
	worker.drive(ctx)

	if got := worker.State(); got != StatePaused {
		t.Fatalf("worker state = %v, want paused", got)
	}
	if scripted.CallCount() != 3 {
		t.Fatalf("provider calls = %d, want 3: the build bought another turn after the stop", scripted.CallCount())
	}

	run, err := activeRun(worker.pdb)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if run == nil {
		t.Fatal("no active run: an owner stop must leave the run resumable")
	}
	if run.PauseReason != PauseReasonOwnerStop {
		t.Fatalf("pause reason = %q, want %q: a stop was recorded as a pending question",
			run.PauseReason, PauseReasonOwnerStop)
	}
	if run.Stage != StageBuild {
		t.Fatalf("run stage = %q, want %q: the resume must re-enter BUILD", run.Stage, StageBuild)
	}
}

// TestDriveClearsStaleStopRequest: a stop that raced an idle worker (nothing
// running to consume it) must not kill the next build.
func TestDriveClearsStaleStopRequest(t *testing.T) {
	worker, _ := newStopTestWorker(t)

	worker.requestStop() // nothing is running; the request has no consumer
	if !worker.stopRequested() {
		t.Fatal("requestStop did not set the flag")
	}

	provider := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures,
		enginetest.Text(planJSON),
	}}
	worker.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: provider, Model: "fake-model", ProviderID: 1}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	worker.setState(StateBuilding)
	worker.drive(ctx)

	// The stale request was dropped at drive entry, so PLAN ran and the pipeline
	// carried on into BUILD rather than parking on arrival.
	if provider.CallCount() < 2 {
		t.Fatalf("provider calls = %d, want >= 2: a stale stop request killed the fresh build", provider.CallCount())
	}
}

// TestEngineRequestStopWithoutWorker: interrupting a project with no worker is
// the caller's answer (nothing to stop), not a silent success.
func TestEngineRequestStopWithoutWorker(t *testing.T) {
	e := &Engine{workers: map[int]*workerHandle{}, logger: slog.Default()}
	if err := e.RequestStop(42); err == nil {
		t.Fatal("RequestStop on a project with no worker returned nil, want an error")
	}
}

// TestEngineRequestStopNeedsWorkInFlight: a worker exists for the whole life of
// a live project, so its mere existence cannot mean "something is running". A
// request accepted with nothing to consume it sits unread until some later run
// drops it as stale, which reads to the owner as a stop that never landed.
func TestEngineRequestStopNeedsWorkInFlight(t *testing.T) {
	cases := []struct {
		name     string
		state    State
		sessions int32
		wantErr  bool
	}{
		// State cannot see a live chat wake: it never leaves StateLive, so an idle
		// live project and one mid-answer look identical from the outside. The
		// session count is the only thing separating them.
		{"live and idle", StateLive, 0, true},
		{"live, answering a chat wake", StateLive, 1, false},
		// A build between its session and go-live is running a validator sweep with no
		// session open, and is still very much interruptible.
		{"building between sessions", StateBuilding, 0, false},
		{"building mid-session", StateBuilding, 1, false},
		{"already parked by an earlier stop", StatePaused, 0, true},
		{"blocked build", StateError, 0, true},
		// A scheduled prompt task runs a real session on a project whose worker is
		// otherwise parked, and consumes a stop like any other.
		{"scheduled task on a paused project", StatePaused, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &Worker{logger: slog.Default()}
			w.setState(tc.state)
			w.sessionsInFlight.Store(tc.sessions)
			e := &Engine{workers: map[int]*workerHandle{1: {worker: w}}, logger: slog.Default()}

			err := e.RequestStop(1)
			if tc.wantErr && err == nil {
				t.Fatal("RequestStop accepted a stop nothing would consume")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("RequestStop rejected a legitimate stop: %v", err)
			}
			if w.stopRequested() != !tc.wantErr {
				t.Fatalf("stop flag = %v, want %v: a rejected request must not set it",
					w.stopRequested(), !tc.wantErr)
			}
		})
	}
}

// TestSessionMarksItselfInFlight: the counter the gate above reads is kept by
// the session itself, and must be balanced however the run ends.
func TestSessionMarksItselfInFlight(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Text("done")}}
	s := testSession(t, p)

	var peak, delta int32
	s.busyFn = func(active bool) {
		if active {
			delta++
			peak = delta
			return
		}
		delta--
	}

	if _, err := s.Run(context.Background(), buildSpecForTest()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if peak != 1 {
		t.Fatalf("peak in-flight = %d, want 1: the session never announced itself", peak)
	}
	if delta != 0 {
		t.Fatalf("in-flight leaked %d: the counter must unwind however the run ends", delta)
	}
}

// captureEvents attaches a bus to the worker and returns a snapshot of
// everything published on it. Publish runs subscribers inline on the calling
// goroutine, so reads inside the subscriber see the state as it was when the
// event fired.
func captureEvents(t *testing.T, w *Worker, onEach func(events.Event)) func() []events.Event {
	t.Helper()
	bus := events.NewBus()
	var mu sync.Mutex
	var seen []events.Event
	bus.SubscribeAll(func(e events.Event) {
		mu.Lock()
		seen = append(seen, e)
		mu.Unlock()
		if onEach != nil {
			onEach(e)
		}
	})
	w.deps.Bus = bus
	return func() []events.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]events.Event(nil), seen...)
	}
}

// eventIndex returns the position of the first event of a type, or -1.
func eventIndex(evs []events.Event, t events.EventType) int {
	for i, e := range evs {
		if e.Type == t {
			return i
		}
	}
	return -1
}

// TestDriveOwnerStopAnnouncesLandingAfterThePark: the chat holds its button at
// "Stopping…" until it hears the stop landed, so the announcement must come
// after the run is really parked. Published early it would hand the owner back a
// live Stop button for work their stop had already won.
func TestDriveOwnerStopAnnouncesLandingAfterThePark(t *testing.T) {
	worker, _ := newStopTestWorker(t)

	pausedWhenAnnounced := false
	snapshot := captureEvents(t, worker, func(e events.Event) {
		if e.Type != events.EventEngineStopLanded {
			return
		}
		if run, err := activeRun(worker.pdb); err == nil && run != nil {
			pausedWhenAnnounced = run.Status == RunStatusPaused && run.PauseReason == PauseReasonOwnerStop
		}
	})

	scripted := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures,
		enginetest.Text(planJSON),
		enginetest.Calls(llm.ToolCall{ID: "c1", Name: "manage_files",
			Arguments: `{"action":"save","kind":"page","file_path":"/","body":"<h1>nope</h1>","title":"nope"}`}),
	}}
	provider := &stopAfterCall{Provider: scripted, after: 2, fire: worker.requestStop}
	worker.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: provider, Model: "fake-model", ProviderID: 1}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	worker.setState(StateBuilding)
	worker.drive(ctx)

	evs := snapshot()
	landed := eventIndex(evs, events.EventEngineStopLanded)
	if landed < 0 {
		t.Fatal("no stop_landed event: the chat has nothing to release its hold on")
	}
	if !pausedWhenAnnounced {
		t.Fatal("stop_landed fired before the run was parked with the owner-stop reason")
	}
	if reason, _ := evs[landed].Payload["reason"].(string); reason != "build" {
		t.Fatalf("stop_landed reason = %q, want build", reason)
	}
}

// stopDuringValidation is a validator that fires a stop request. It runs after
// the build session has already returned, which is the one window where a stop
// can land on a stage that will report Done.
type stopDuringValidation struct{ fire func() }

func (stopDuringValidation) Name() string { return "stop-during-validation" }

func (v stopDuringValidation) Validate(context.Context, *sql.DB, string, string) []Issue {
	v.fire()
	return nil
}

// TestDriveFinishedStageDropsStopRequest: a stop landing once the stage has
// finished is deliberately not honoured - the work is done and a completed
// build already went live, so parking it would un-live a shipped project. The
// request still has to be CONSUMED: left set it survives into the live project
// and counts toward the idle streak that auto-disables the next scheduled task.
// Every terminal branch is exposed, so the drop belongs with the exemption.
func TestDriveFinishedStageDropsStopRequest(t *testing.T) {
	worker, _ := newStopTestWorker(t)

	// The stop lands inside the post-build validator sweep: the session is over,
	// so nothing consumes it, and BUILD still reports Done.
	validatorMu.Lock()
	validatorSet = []Validator{stopDuringValidation{fire: worker.requestStop}}
	validatorMu.Unlock()

	provider := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures,
		enginetest.Text(planJSON),
		enginetest.Calls(llm.ToolCall{ID: "c1", Name: "manage_files",
			Arguments: `{"action":"save","kind":"page","file_path":"/","body":"<h1>Tip calculator</h1>","title":"Tip calculator"}`}),
		enginetest.Calls(llm.ToolCall{ID: "c2", Name: "manage_tasks",
			Arguments: `{"action":"complete","task_id":"home-page","result":"built the calculator page"}`}),
	}}
	worker.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: provider, Model: "fake-model", ProviderID: 1}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	worker.setState(StateBuilding)
	worker.drive(ctx)

	// The finished build shipped rather than parking.
	if got := worker.State(); got != StateLive {
		t.Fatalf("worker state = %v, want live: a stop must not un-live a finished build", got)
	}
	if worker.stopRequested() {
		t.Fatal("stop request survived a finished build: the first scheduled task on the live project would die on arrival")
	}
}
