package project

import (
	"database/sql"
	"strings"
	"testing"

	"openfabrica/events"
	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// setupTasksDB is a migrated project database seeded with one active task. The
// deliverable guard on `complete` reads every artifact and endpoint store, which
// all exist here exactly as production has them, so a store the guard forgets or
// misnames fails the suite instead of failing open.
func setupTasksDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position) VALUES ('rooms-table', 'Rooms table', 'active', 0)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return db
}

// seedEndpoint records a built endpoint in both tiers the guard consults, the way
// api.ClaimEndpoint does in production.
func seedEndpoint(t *testing.T, db dbh.Handle, store, kind, path string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO `+store+` (path) VALUES (?)`, path); err != nil {
		t.Fatalf("seed %s %q: %v", store, path, err)
	}
	if _, err := db.Exec(`INSERT INTO of_endpoints (path, sub_segment, method, kind) VALUES (?, '', 'POST', ?)`, path, kind); err != nil {
		t.Fatalf("seed spine %q: %v", path, err)
	}
}

// seedFile records a non-deleted artifact so the deliverable guard sees it.
func seedFile(t *testing.T, db dbh.Handle, kind, path string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_files (path, kind, is_deleted) VALUES (?, ?, 0)`, path, kind); err != nil {
		t.Fatalf("seed file %q: %v", path, err)
	}
}

func taskStatus(t *testing.T, db dbh.Handle, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT status FROM of_tasks WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return s
}

func taskResult(t *testing.T, db dbh.Handle, id string) string {
	t.Helper()
	var r sql.NullString
	if err := db.QueryRow(`SELECT result FROM of_tasks WHERE id = ?`, id).Scan(&r); err != nil {
		t.Fatalf("read result: %v", err)
	}
	return r.String
}

// emitTaskQueue (fired by every queue mutation) must publish the engine.tasks
// event the live chat subscribes to, so mid-build queue changes reach the UI.
func TestComplete_PublishesEngineTasksEvent(t *testing.T) {
	db := setupTasksDB(t)
	bus := events.NewBus()
	var gotType events.EventType
	bus.Subscribe(events.EventEngineTasks, func(e events.Event) { gotType = e.Type })
	ctx := &tools.ToolContext{DB: db, Bus: bus, ProjectID: 1}

	// rooms-table declares no paths, so completing it needs the project to have
	// built something (the empty-project guard); seed one artifact.
	seedFile(t, db, "layout", "default")
	if _, err := (&TasksComplete{TaskID: "rooms-table", Result: "done"}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Publish is synchronous, so the handler has already run by the time Run returns.
	if gotType != events.EventEngineTasks {
		t.Fatalf("manage_tasks must publish %q, got %q", events.EventEngineTasks, gotType)
	}
}

// The first complete flips the task to done and reports a plain success.
func TestComplete_FirstCallSucceeds(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}

	// rooms-table is path-less; the empty-project guard requires something built.
	seedFile(t, db, "layout", "default")
	res, err := (&TasksComplete{TaskID: "rooms-table", Result: "done"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("first complete must succeed: %+v", res)
	}
	if got := taskStatus(t, db, "rooms-table"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
	data, _ := res.Data.(map[string]any)
	if data["already_done"] != nil {
		t.Fatalf("first complete must not report already_done: %+v", data)
	}
}

// Completing an already-done task must succeed idempotently AND surface
// already_done, so the loop can stop instead of treating the re-complete as
// fresh progress (the wasted-iteration spin this guard fixes).
func TestComplete_ReCompleteIsIdempotentAndFlagged(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}

	// rooms-table is path-less; the empty-project guard requires something built.
	seedFile(t, db, "layout", "default")
	if _, err := (&TasksComplete{TaskID: "rooms-table"}).Run(ctx); err != nil {
		t.Fatalf("first complete: %v", err)
	}

	res, err := (&TasksComplete{TaskID: "rooms-table", Result: "again"}).Run(ctx)
	if err != nil {
		t.Fatalf("re-complete run: %v", err)
	}
	if !res.Success {
		t.Fatalf("re-complete must succeed idempotently: %+v", res)
	}
	data, ok := res.Data.(map[string]any)
	if !ok || data["already_done"] != true {
		t.Fatalf("re-complete must report already_done=true: %+v", res.Data)
	}
}

// Completing a task that does not exist is a real failure, distinct from the
// idempotent already-done case.
func TestComplete_UnknownTaskFails(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&TasksComplete{TaskID: "no-such-task"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("completing an unknown task must fail: %+v", res)
	}
}

// A task that declares owned files but whose files do not exist yet must NOT
// complete: this is the premature "complete-as-start" misfire the guard blocks,
// which otherwise marks an unbuilt item done and corrupts the checklist.
func TestComplete_UnbuiltDeclaredPathsRefused(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('shell', 'Shell', 'pending', '["css/app.css","/","js/state.js"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	res, err := (&TasksComplete{TaskID: "shell", Result: "starting build"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("completing a task with no built deliverables must be refused: %+v", res)
	}
	if got := taskStatus(t, db, "shell"); got != "pending" {
		t.Fatalf("a refused complete must leave the task status untouched, got %q", got)
	}
}

// Every owned path is closure evidence: a task with only SOME of its declared
// paths built is refused, and completes once all of them exist. Path forms are
// matched tolerantly (declared "css/app.css" vs stored bare path, declared "/"
// page).
func TestComplete_AllOwnedPathsRequired(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('shell', 'Shell', 'pending', '["css/app.css","/","js/state.js"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	seedFile(t, db, "file", "css/app.css")

	res, err := (&TasksComplete{TaskID: "shell", Result: "built shell"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("a task with unbuilt owned paths must be refused: %+v", res)
	}
	if got := taskStatus(t, db, "shell"); got != "pending" {
		t.Fatalf("a refused complete must leave the task status untouched, got %q", got)
	}

	seedFile(t, db, "page", "/")
	seedFile(t, db, "file", "js/state.js")
	res, err = (&TasksComplete{TaskID: "shell", Result: "built shell"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a task with every deliverable built must complete: %+v", res)
	}
	if got := taskStatus(t, db, "shell"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
}

// A deleted artifact does not count as built: the guard reads only live files.
func TestComplete_DeletedPathDoesNotCount(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('shell', 'Shell', 'pending', '["css/app.css"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_files (path, kind, is_deleted) VALUES ('css/app.css', 'file', 1)`); err != nil {
		t.Fatalf("seed deleted file: %v", err)
	}

	res, err := (&TasksComplete{TaskID: "shell"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("a deleted artifact must not satisfy the deliverable guard: %+v", res)
	}
}

// A path-less task completed as the very first build action, against an empty
// project, must be refused. This is the empty-project misfire: the planner
// left the shared-shell task (a real layout + global CSS deliverable) with empty
// paths because it "owns no page route", so the path-based guard skipped it, and
// the model fired `complete` as a "starting build" signal before writing a single
// file. The empty-project backstop catches it.
func TestComplete_PathlessTaskRefusedWhenProjectEmpty(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position)
		VALUES ('shared-shell', 'Shared layout + global CSS', 'pending', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	res, err := (&TasksComplete{TaskID: "shared-shell", Result: "Starting build: shared shell first"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("a path-less complete against an empty project must be refused: %+v", res)
	}
	if got := taskStatus(t, db, "shared-shell"); got != "pending" {
		t.Fatalf("a refused complete must leave the task status untouched, got %q", got)
	}
}

// Once any task has built something, a path-less task (pure wiring, or a task the
// planner under-declared) completes - the project is no longer empty, so the
// backstop steps aside. Only the genuine nothing-built-anywhere case is blocked.
func TestComplete_PathlessTaskAllowedOnceProjectHasArtifacts(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position)
		VALUES ('wire-nav', 'Wire nav into shell', 'pending', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// A sibling task already produced the shell; the project is non-empty.
	seedFile(t, db, "layout", "default")

	res, err := (&TasksComplete{TaskID: "wire-nav", Result: "wired nav"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a path-less task must complete once the project has artifacts: %+v", res)
	}
	if got := taskStatus(t, db, "wire-nav"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
}

// A path-less complete is also allowed when the only built artifact lives in a
// non-file store (a table), proving tasksProjectHasAnyArtifact consults every
// store, not just of_files.
func TestComplete_PathlessTaskAllowedWithOnlyTableBuilt(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position)
		VALUES ('seed-rooms', 'Seed rooms data', 'pending', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('rooms', '{}')`); err != nil {
		t.Fatalf("seed table: %v", err)
	}

	res, err := (&TasksComplete{TaskID: "seed-rooms"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a built table must count as a non-empty project for a path-less complete: %+v", res)
	}
}

// A task whose only deliverable is a database table (paths:["messages"]) must
// complete once that table exists. Tables live in of_dynamic_tables, not of_files,
// so the completion guard checks the table store too - a files-only check would
// refuse these forever and drive the model to thrash splitting/renaming the task.
func TestComplete_TablePathSatisfiedByDynamicTable(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('messages-table', 'Contact messages table', 'pending', '["messages"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Before the table exists, the task has nothing built: refuse.
	if res, err := (&TasksComplete{TaskID: "messages-table"}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	} else if res.Success {
		t.Fatalf("a table task must be refused before its table exists: %+v", res)
	}

	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('messages', '{}')`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	res, err := (&TasksComplete{TaskID: "messages-table", Result: "created messages table"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a table task whose table exists must complete: %+v", res)
	}
	if got := taskStatus(t, db, "messages-table"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
}

// A task whose only deliverable is a websocket relay (paths:["ocean"]) must
// complete once the relay exists. Relays live in of_ws_endpoints, so the guard
// checks every endpoint store and the relay's own path resolves it. Without that,
// a websocket-only task is uncompletable: the model retries the same complete, then
// thrashes into fabricating a phantom file just to satisfy the gate.
func TestComplete_WebsocketPathSatisfiedByWsEndpoint(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('game-socket', 'Realtime ocean socket', 'pending', '["ocean"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Before the relay exists, the task has nothing built: refuse.
	if res, err := (&TasksComplete{TaskID: "game-socket"}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	} else if res.Success {
		t.Fatalf("a websocket task must be refused before its relay exists: %+v", res)
	}

	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path) VALUES ('ocean')`); err != nil {
		t.Fatalf("seed ws endpoint: %v", err)
	}
	res, err := (&TasksComplete{TaskID: "game-socket", Result: "ocean relay live"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a websocket task whose relay exists must complete: %+v", res)
	}
	if got := taskStatus(t, db, "game-socket"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
}

// A task whose deliverable is server-authoritative logic (paths:["booth"],
// tools:["manage_function"] - the shape PLAN authors for logic clients must not own)
// must complete once its function endpoint exists. The endpoint is the artifact;
// nothing else is written, so if the guard cannot see it the task is uncompletable
// no matter what the model does.
func TestComplete_FunctionPathSatisfiedByFunctionEndpoint(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('dj-logic-functions', 'Authoritative DJ booth', 'pending', '["booth"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Before the function exists, the task has nothing built: refuse.
	if res, err := (&TasksComplete{TaskID: "dj-logic-functions"}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	} else if res.Success {
		t.Fatalf("a function task must be refused before its endpoint exists: %+v", res)
	}

	seedEndpoint(t, db, "of_function_endpoints", "function", "booth")
	res, err := (&TasksComplete{TaskID: "dj-logic-functions", Result: "booth lock + queue live"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a function task whose endpoint exists must complete without force: %+v", res)
	}
	if got := taskStatus(t, db, "dj-logic-functions"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
	if r := taskResult(t, db, "dj-logic-functions"); strings.HasPrefix(r, "[forced]") {
		t.Fatalf("a cleanly-built function task must NOT be recorded as forced; got: %q", r)
	}
}

// The spine resolves an endpoint kind on its own, so a kind reaching the spine
// without a per-kind query here still completes. Every kind mounted on /api/ must
// claim into of_endpoints to route at all, which is what makes the spine tier
// drift-proof as kinds are added.
func TestComplete_PathSatisfiedBySpineAlone(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('booth-task', 'Booth', 'pending', '["booth"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_endpoints (path, sub_segment, method, kind)
		VALUES ('booth', '', 'GET', 'function')`); err != nil {
		t.Fatalf("seed spine: %v", err)
	}
	res, err := (&TasksComplete{TaskID: "booth-task", Result: "built"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a path present in the spine must resolve: %+v", res)
	}
}

// A sub-segment-routed endpoint (here an SSE stream) is declared by the planner as its
// public route "<resource>/stream", not the bare resource: the stream over the `orders`
// table is reachable at /api/orders/stream, and api.epCreateStream stores it keyed on
// `orders` while api.ClaimEndpoint records the "stream" sub_segment in the spine. The guard
// must reconstruct that route from the spine so the task completes CLEANLY - without this the
// declared "orders/stream" matches nothing, the complete is refused, and the model is pushed
// into a force-complete of a task that in fact built exactly the right thing. This is the
// general fix for every reserved sub-segment (upload/ws/export/...), not a stream special case.
func TestComplete_StreamRoutePathSatisfiedBySpineSubSegment(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('sse-stream', 'Order event stream', 'pending', '["orders/stream"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Before the stream exists, the task has nothing built: refuse.
	if res, err := (&TasksComplete{TaskID: "sse-stream"}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	} else if res.Success {
		t.Fatalf("an SSE task must be refused before its stream exists: %+v", res)
	}

	// Mirror what api.epCreateStream + api.ClaimEndpoint(KindSSE) persist: the config row
	// keyed on the bare resource, and a spine row carrying the "stream" sub_segment.
	if _, err := db.Exec(`INSERT INTO of_stream_endpoints (path) VALUES ('orders')`); err != nil {
		t.Fatalf("seed stream endpoint: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_endpoints (path, sub_segment, method, kind)
		VALUES ('orders', 'stream', 'GET', 'sse')`); err != nil {
		t.Fatalf("seed spine: %v", err)
	}

	res, err := (&TasksComplete{TaskID: "sse-stream", Result: "order stream live"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("an SSE task whose stream exists must complete on its declared route path: %+v", res)
	}
	if got := taskStatus(t, db, "sse-stream"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
	if r := taskResult(t, db, "sse-stream"); strings.HasPrefix(r, "[forced]") {
		t.Fatalf("a cleanly-built SSE task must NOT be recorded as forced; got: %q", r)
	}
}

// Feeds live in the /feed/ namespace and are deliberately absent from the spine,
// so the per-kind queries have to stay: a feed task must still complete.
func TestComplete_FeedPathSatisfiedWithoutSpineRow(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('rss', 'Public feed', 'pending', '["updates"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_feed_endpoints (path, table_name) VALUES ('updates', 'updates')`); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	res, err := (&TasksComplete{TaskID: "rss", Result: "feed live"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a feed task must complete from its own store, with no spine row: %+v", res)
	}
}

// A task whose only deliverable is a JWT auth endpoint (paths:["auth"]) must
// complete once the endpoint exists. Auth endpoints live in of_auth_endpoints - a
// store the guard did not query, so an auth-only task (the common "auth endpoint +
// roles" task owning paths:["auth"]) could never complete and was force-completed,
// stamping a misleading [forced] note on genuinely-built auth. The guard now
// checks the auth store too, so the endpoint's own path resolves it.
func TestComplete_AuthPathSatisfiedByAuthEndpoint(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('player-auth', 'Auth endpoint + roles', 'pending', '["auth"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Before the auth endpoint exists, the task has nothing built: refuse.
	if res, err := (&TasksComplete{TaskID: "player-auth"}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	} else if res.Success {
		t.Fatalf("an auth task must be refused before its endpoint exists: %+v", res)
	}

	if _, err := db.Exec(`INSERT INTO of_auth_endpoints (path, table_name) VALUES ('auth', 'users')`); err != nil {
		t.Fatalf("seed auth endpoint: %v", err)
	}
	res, err := (&TasksComplete{TaskID: "player-auth", Result: "auth endpoint live"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("an auth task whose endpoint exists must complete without force: %+v", res)
	}
	if got := taskStatus(t, db, "player-auth"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
	if r := taskResult(t, db, "player-auth"); strings.HasPrefix(r, "[forced]") {
		t.Fatalf("a cleanly-built auth task must NOT be recorded as forced; got: %q", r)
	}
}

// A task whose only deliverable is a query/FTS read endpoint (paths:["search"])
// must complete once the endpoint exists. Query endpoints live in
// of_query_endpoints - a store the guard did not query, so a search-only task
// could never complete and was force-completed, stamping a misleading [forced]
// note on a genuinely-built, tested endpoint. The guard now checks the query
// store too, so the endpoint's own path resolves it. (TOTP and magic-link
// endpoints, added alongside, follow the same shape.)
func TestComplete_QueryPathSatisfiedByQueryEndpoint(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('message-search', 'Full-text message search', 'pending', '["search"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Before the query endpoint exists, the task has nothing built: refuse.
	if res, err := (&TasksComplete{TaskID: "message-search"}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	} else if res.Success {
		t.Fatalf("a search task must be refused before its endpoint exists: %+v", res)
	}

	if _, err := db.Exec(`INSERT INTO of_query_endpoints (path) VALUES ('search')`); err != nil {
		t.Fatalf("seed query endpoint: %v", err)
	}
	res, err := (&TasksComplete{TaskID: "message-search", Result: "FTS search endpoint live"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a search task whose endpoint exists must complete without force: %+v", res)
	}
	if got := taskStatus(t, db, "message-search"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
	if r := taskResult(t, db, "message-search"); strings.HasPrefix(r, "[forced]") {
		t.Fatalf("a cleanly-built search task must NOT be recorded as forced; got: %q", r)
	}
}

// A task whose only deliverable is a SQL view (paths:["metrics_overview"]) must
// complete once that view exists. Views live only in sqlite_master (no of_views
// registry), a store the guard did not query, so a view-only task - the platform's
// own recommended "view + read API" shape - could never complete and was forced
// through, stamping a misleading [forced] note on genuinely-built work. The guard
// now enumerates user views, so the view's own name resolves it.
func TestComplete_ViewPathSatisfiedBySqliteView(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('metrics-view', 'Metrics aggregate view', 'pending', '["metrics_overview"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Before the view exists, the task has nothing built: refuse.
	if res, err := (&TasksComplete{TaskID: "metrics-view"}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	} else if res.Success {
		t.Fatalf("a view task must be refused before its view exists: %+v", res)
	}

	if _, err := db.Exec(`CREATE VIEW metrics_overview AS SELECT 1 AS total_users`); err != nil {
		t.Fatalf("create view: %v", err)
	}
	res, err := (&TasksComplete{TaskID: "metrics-view", Result: "built metrics_overview view"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a view task whose view exists must complete without force: %+v", res)
	}
	if got := taskStatus(t, db, "metrics-view"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
	if r := taskResult(t, db, "metrics-view"); strings.HasPrefix(r, "[forced]") {
		t.Fatalf("a cleanly-built view task must NOT be recorded as forced; got: %q", r)
	}
}

// A view task whose deliverable is exposed only through a read API completes via
// separator folding: the planner declares the SQL-identifier name "signups_daily"
// while the builder serves it at the URL-style endpoint "signups-daily". With no
// view of that exact name seeded, only '-'<->'_' normalization in the resource
// namespace can bridge the two.
func TestComplete_SeparatorFoldsHyphenEndpointToUnderscorePath(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('signups-view', 'Daily signups view', 'pending', '["signups_daily"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// Only the hyphenated read endpoint exists - no same-named view or table.
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name) VALUES ('signups-daily', 'signups')`); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}

	res, err := (&TasksComplete{TaskID: "signups-view", Result: "exposed signups_daily via read API"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("declared signups_daily must match endpoint signups-daily via separator folding: %+v", res)
	}
	if got := taskStatus(t, db, "signups-view"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
}

// taskPathForms must emit a shared comparable form for the two separator
// conventions, so a declared resource name and its opposite-separator artifact
// meet regardless of which side used '-' vs '_'.
func TestTaskPathForms_SeparatorVariantsShareAForm(t *testing.T) {
	shared := func(a, b string) bool {
		set := map[string]bool{}
		for _, f := range taskPathForms(a) {
			set[f] = true
		}
		for _, f := range taskPathForms(b) {
			if set[f] {
				return true
			}
		}
		return false
	}
	if !shared("metrics-overview", "metrics_overview") {
		t.Fatalf("taskPathForms(metrics-overview) and (metrics_overview) must share a form: %v vs %v",
			taskPathForms("metrics-overview"), taskPathForms("metrics_overview"))
	}
}

// Separator folding must NOT leak into the page-route namespace: a "/foo-bar" page
// task must still not be satisfied by a same-named "foo_bar" table, preserving the
// page-route/resource split that folding could otherwise erode.
func TestComplete_HyphenPageRouteNotSatisfiedByUnderscoreTable(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('foo-page', 'Foo bar page', 'pending', '["/foo-bar"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('foo_bar', '{}')`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	if res, err := (&TasksComplete{TaskID: "foo-page"}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	} else if res.Success {
		t.Fatalf("a page-route task must not complete when only a separator-variant table exists: %+v", res)
	}
}

// A path-less complete is allowed when the only built artifact is a SQL view,
// proving tasksProjectHasAnyArtifact now consults the view store too - a project
// whose only output so far is a view is no longer treated as empty.
func TestComplete_PathlessTaskAllowedWithOnlyViewBuilt(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position)
		VALUES ('wire-metrics', 'Wire metrics read API', 'pending', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(`CREATE VIEW metrics_overview AS SELECT 1 AS total_users`); err != nil {
		t.Fatalf("create view: %v", err)
	}

	res, err := (&TasksComplete{TaskID: "wire-metrics"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a built view must count as a non-empty project for a path-less complete: %+v", res)
	}
}

// The planner declares dynamic page routes with [slug] (plan.md examples); the
// builder saves them under the runtime router's Express :slug. A detail-page
// task declaring "/projects/[slug]" must complete once the page is saved at
// "/projects/:slug" - the two param syntaxes are normalized to the same form.
func TestComplete_RouteParamSyntaxNormalized(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('project-detail', 'Project detail page', 'pending', '["/projects/[slug]"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	seedFile(t, db, "page", "/projects/:slug")

	res, err := (&TasksComplete{TaskID: "project-detail", Result: "built detail page"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("declared /projects/[slug] must match a page saved at /projects/:slug: %+v", res)
	}
	if got := taskStatus(t, db, "project-detail"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
}

// A task declared with an angle-bracket placeholder (/<username>) must complete
// against the page the builder is forced to save at /:username (the save-time
// validator steers <slug> -> :slug). Without <slug> recognition, the declared
// path would never match the stored page and the task would be uncompletable.
func TestComplete_AngleBracketParamNormalized(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('blog-page', 'Public blog page', 'pending', '["/<username>"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	seedFile(t, db, "page", "/:username")

	res, err := (&TasksComplete{TaskID: "blog-page", Result: "built blog page"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("declared /<username> must match a page saved at /:username: %+v", res)
	}
	if got := taskStatus(t, db, "blog-page"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
}

// A shell task that declares the layout as "/layout" must complete once the
// shared layout is saved at its canonical path "default" - the two are aliased.
func TestComplete_LayoutPathAliasedToDefault(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('shell', 'Shared shell', 'pending', '["/layout"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	seedFile(t, db, "layout", "default")

	res, err := (&TasksComplete{TaskID: "shell", Result: "built shell"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("declared /layout must match the layout stored at default: %+v", res)
	}
}

// The page-route/resource collision: "/activity" (a page) and "activity" (a
// table) are different namespaces, so a same-named table must NOT satisfy a page
// task's completion - otherwise a planned page could be ticked done the moment its
// backend existed and silently dropped.
func TestComplete_PageRouteNotSatisfiedBySameNamedTable(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('activity-page', 'Activity page', 'pending', '["/activity"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// A table named "activity" exists, but the /activity PAGE does not.
	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('activity', '{}')`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	if res, err := (&TasksComplete{TaskID: "activity-page"}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	} else if res.Success {
		t.Fatalf("a page-route task must not complete when only a same-named table exists: %+v", res)
	}

	// Once the page is actually saved at /activity, the task completes.
	seedFile(t, db, "page", "/activity")
	res, err := (&TasksComplete{TaskID: "activity-page", Result: "built activity page"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a page-route task must complete once its page exists: %+v", res)
	}
	if got := taskStatus(t, db, "activity-page"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
}

// The PWA misfire: the enable-pwa task is scoped to
// manage_project (whose only build action, enable_pwa, generates manifest.json +
// service-worker.js) but the model ticks it done with a prose result while never
// calling the action. With no manifest the app is not installable, so the
// completion must be refused until enable_pwa has actually run.
func TestComplete_PWATaskRefusedWithoutManifest(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, tools, position)
		VALUES ('enable-pwa', 'Enable PWA', 'pending', '["manage_project"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// The project is non-empty (a page was built) so the empty-project guard
	// passes; the PWA guard must still fire because no manifest.json exists.
	seedFile(t, db, "page", "/")

	res, err := (&TasksComplete{TaskID: "enable-pwa", Result: "Enabled PWA: installable with offline support"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("a PWA task with no manifest must be refused: %+v", res)
	}
	if got := taskStatus(t, db, "enable-pwa"); got != "pending" {
		t.Fatalf("a refused complete must leave the task status untouched, got %q", got)
	}
}

// Once enable_pwa has actually run (manifest.json exists), the same task completes.
func TestComplete_PWATaskAllowedWithManifest(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, tools, position)
		VALUES ('enable-pwa', 'Enable PWA', 'pending', '["manage_project"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	seedFile(t, db, "page", "/")
	seedFile(t, db, "file", "manifest.json")

	res, err := (&TasksComplete{TaskID: "enable-pwa", Result: "ran enable_pwa"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a PWA task whose manifest exists must complete: %+v", res)
	}
	if got := taskStatus(t, db, "enable-pwa"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
}

func tasksCount(t *testing.T, db dbh.Handle) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM of_tasks`).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

// A first add of a novel task enqueues it; a second add of the SAME title must
// fail rather than silently auto-suffix a duplicate ("<title>-2"), so a model
// cannot spam the queue with repeated adds.
func TestAdd_DuplicateTitleIsRefused(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}

	first, err := (&TasksAdd{Title: "Mark Renderer JS", Intent: "draw MARK"}).Run(ctx)
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if !first.Success {
		t.Fatalf("first add of a novel task must succeed: %+v", first)
	}
	before := tasksCount(t, db)

	dup, err := (&TasksAdd{Title: "Mark Renderer JS", Intent: "draw MARK again"}).Run(ctx)
	if err != nil {
		t.Fatalf("dup add: %v", err)
	}
	if dup.Success {
		t.Fatalf("re-adding the same pending title must fail, not auto-suffix: %+v", dup)
	}
	if after := tasksCount(t, db); after != before {
		t.Fatalf("a refused duplicate add must not create a row: before=%d after=%d", before, after)
	}
}

// Case/whitespace variants of an existing pending title are still duplicates.
func TestAdd_DuplicateTitleCaseInsensitive(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := (&TasksAdd{Title: "Canvas Setup"}).Run(ctx); err != nil {
		t.Fatalf("first add: %v", err)
	}
	res, err := (&TasksAdd{Title: "  canvas setup  "}).Run(ctx)
	if err != nil {
		t.Fatalf("variant add: %v", err)
	}
	if res.Success {
		t.Fatalf("a case/whitespace variant of a pending title must be refused: %+v", res)
	}
}

// A title matching an already-DONE task is allowed through: that work may
// genuinely need redoing, so dedup only guards still-queued tasks.
func TestAdd_DoneTitleCanBeReAdded(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position) VALUES ('export', 'CSV Export', 'done', 5)`); err != nil {
		t.Fatalf("seed done task: %v", err)
	}
	res, err := (&TasksAdd{Title: "CSV Export", Intent: "redo export"}).Run(ctx)
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if !res.Success {
		t.Fatalf("re-adding a done task's title must be allowed: %+v", res)
	}
}

// The build agent re-listing a checklist line "`<id>` - <title>" back as a new
// title (it slugifies to "<id>-<title-slug>", matching neither the bare id nor
// the bare title) must be caught as a duplicate, not inserted as a fresh row.
func TestAdd_DuplicateIdPrefixedTitleEcho(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position) VALUES ('shell-design', 'Page shell + theme CSS', 'pending', 5)`); err != nil {
		t.Fatalf("seed plan task: %v", err)
	}
	before := tasksCount(t, db)

	res, err := (&TasksAdd{Title: "shell-design - Page shell + theme CSS", Intent: "build the shell"}).Run(ctx)
	if err != nil {
		t.Fatalf("echo add: %v", err)
	}
	if res.Success {
		t.Fatalf("an '<id> - <title>' echo of an existing task must be refused: %+v", res)
	}
	if after := tasksCount(t, db); after != before {
		t.Fatalf("a refused echo add must not create a row: before=%d after=%d", before, after)
	}
}

// The echo rule is exact slug equality against "<id>-<title-slug>", so a
// genuinely new task whose title merely shares a leading word with an existing
// id is still allowed through.
func TestAdd_DistinctTitleSharingPrefixWordAllowed(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position) VALUES ('playlist', 'Playlist panel', 'pending', 5)`); err != nil {
		t.Fatalf("seed plan task: %v", err)
	}
	res, err := (&TasksAdd{Title: "Playlist export to JSON", Intent: "export playlist"}).Run(ctx)
	if err != nil {
		t.Fatalf("distinct add: %v", err)
	}
	if !res.Success {
		t.Fatalf("a distinct title that only shares a prefix word must be allowed: %+v", res)
	}
}

// The defect this fixes: a task declaring a binary path the build cannot author
// (an .mp3 - manage_files writes text only) can never satisfy the existence guard,
// so a model fabricates a placeholder file to clear it. The rejection must instead
// advertise the legitimate exit (force=true + reason) and forbid the fake-file
// workaround, so the model takes the honest path.
func TestComplete_UnbuiltPathRejectionAdvertisesForce(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('demos', 'Demo MP3 tracks', 'pending', '["/files/demos/track-01.mp3"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	res, err := (&TasksComplete{TaskID: "demos", Result: "synthesized in browser"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("an unbuilt declared path must still be refused without force: %+v", res)
	}
	for _, want := range []string{"force=true", "do NOT write placeholder"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("rejection should advertise the force hatch (%q); got: %s", want, res.Error)
		}
	}
}

// force=true + a reason completes a task whose declared path cannot be produced,
// and records the override as a [forced] result so the owner sees what happened
// and why - the honest replacement for fabricating a placeholder file.
func TestComplete_ForceWithReasonCompletesAndRecords(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('demos', 'Demo MP3 tracks', 'pending', '["/files/demos/track-01.mp3"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	res, err := (&TasksComplete{
		TaskID: "demos",
		Force:  true,
		Reason: "binary MP3s cannot be authored; audio is synthesized in-browser via audio.demosynth.js",
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("force-complete with a reason must succeed: %+v", res)
	}
	if got := taskStatus(t, db, "demos"); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
	r := taskResult(t, db, "demos")
	if !strings.HasPrefix(r, "[forced]") {
		t.Errorf("a forced result must begin with [forced]; got: %q", r)
	}
	if !strings.Contains(r, "demosynth") {
		t.Errorf("a forced result must carry the reason; got: %q", r)
	}
	data, _ := res.Data.(map[string]any)
	if data["forced"] != true {
		t.Errorf("a forced complete must report forced=true; got: %+v", data)
	}
}

// force without a reason is refused: the override must always be auditable, never
// a silent skip.
func TestComplete_ForceWithoutReasonFails(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('demos', 'Demo MP3 tracks', 'pending', '["/files/demos/track-01.mp3"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	res, err := (&TasksComplete{TaskID: "demos", Force: true}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("force without a reason must fail: %+v", res)
	}
	if got := taskStatus(t, db, "demos"); got != "pending" {
		t.Fatalf("a refused force-complete must leave the task status untouched, got %q", got)
	}
}

// force bypasses only the deliverable-existence guard, NOT the PWA manifest guard:
// enable_pwa is always callable, so a missing manifest is a real omission, not an
// unproducible deliverable, and force must not ship a non-installable app.
func TestComplete_ForceDoesNotBypassPWAGuard(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, tools, position)
		VALUES ('enable-pwa', 'Enable PWA', 'pending', '["manage_project"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	seedFile(t, db, "page", "/")
	res, err := (&TasksComplete{TaskID: "enable-pwa", Force: true, Reason: "trying to skip"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatalf("force must not bypass the PWA manifest guard: %+v", res)
	}
	if got := taskStatus(t, db, "enable-pwa"); got != "pending" {
		t.Fatalf("a refused force-complete must leave the task status untouched, got %q", got)
	}
}

// The engine-added user-flows item owns no paths, so only this guard stops it being
// completed with no of_test.js ever written.
func TestComplete_UserFlowsNeedsTheScript(t *testing.T) {
	db := tooltest.ProjectDB(t)
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position) VALUES ('user-flows', 'User flows', 'pending', 0)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	seedFile(t, db, "page", "/")
	ctx := &tools.ToolContext{DB: db, ProjectID: 1}

	res, err := (&TasksComplete{TaskID: "user-flows", Result: "wrote flows"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "of_test.js") {
		t.Fatalf("completing user-flows with no of_test.js must be refused, got %+v", res)
	}
	if got := taskStatus(t, db, "user-flows"); got == "done" {
		t.Fatal("refused complete still marked the task done")
	}

	seedFile(t, db, "test", "of_test.js")
	res, err = (&TasksComplete{TaskID: "user-flows", Result: "wrote flows"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("complete with of_test.js saved should pass, got %+v err=%v", res, err)
	}
}

// With the owner's setting off, the user-flows item completes without a script: nothing
// will ever replay one.
func TestComplete_UserFlowsNeedsNoScriptWhenTurnedOff(t *testing.T) {
	db := tooltest.ProjectDB(t)
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position) VALUES ('user-flows', 'User flows', 'pending', 0)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	seedFile(t, db, "page", "/")
	global, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })
	for _, q := range []string{
		`CREATE TABLE projects (id INTEGER PRIMARY KEY, name TEXT, domain TEXT, description TEXT, llm_model_id INTEGER,
			published INTEGER, mode TEXT, config TEXT NOT NULL DEFAULT '', source_project_id INTEGER,
			ambition_level TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`INSERT INTO projects (id, name, published, mode, llm_model_id, ambition_level, config) VALUES (1, 'p', 0, 'building', 1, 'high', '{"user_flows":false}')`,
	} {
		if _, err := global.Exec(q); err != nil {
			t.Fatalf("seed global db: %v", err)
		}
	}
	ctx := &tools.ToolContext{DB: db, GlobalDB: global, ProjectID: 1}

	res, err := (&TasksComplete{TaskID: "user-flows", Result: "turned off"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("completing with user-flow tests off must pass without a script, got %+v err=%v", res, err)
	}
}

// A planned module path and the same module saved under a js/ directory share a form,
// so complete is not refused for a path the build did satisfy; a table named like the
// file stem never matches the file.
func TestTaskPathForms_ModuleMatchesByBasename(t *testing.T) {
	shares := func(a, b string) bool {
		set := map[string]bool{}
		for _, f := range taskPathForms(a) {
			set[f] = true
		}
		for _, f := range taskPathForms(b) {
			if set[f] {
				return true
			}
		}
		return false
	}
	if !shares("/files/app.state.js", "js/app.state.js") {
		t.Fatalf("/files/app.state.js and js/app.state.js must match: %v vs %v", taskPathForms("/files/app.state.js"), taskPathForms("js/app.state.js"))
	}
	if shares("app", "/files/app.js") {
		t.Fatal("a resource name must not match a module file")
	}
}

// A page declared by its bare name ("login") is satisfied by the page saved at its
// route ("/login"), and a schedule is satisfied by name; a genuine miss names similar
// built artifacts so the model need not retry blind.
func TestComplete_BarePageNameAndScheduleSatisfyOwnedPaths(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db}
	seedFile(t, db, "page", "/login")
	if _, err := db.Exec(`INSERT INTO of_scheduled_tasks (name, cron_expression) VALUES ('purge-expired', '0 3 * * *')`); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('auth', 'Auth', 'pending', '["login","purge-expired"]', 1)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	res, err := (&TasksComplete{TaskID: "auth", Result: "done"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("bare page name and schedule must satisfy the guard: %+v %v", res, err)
	}

	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('uploads', 'Uploads', 'pending', '["upload-page"]', 2)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	seedFile(t, db, "page", "/uploads/new-upload")
	res, err = (&TasksComplete{TaskID: "uploads", Result: "done"}).Run(ctx)
	if err != nil || res.Success || !strings.Contains(res.Error, "similar name") || !strings.Contains(res.Error, "/uploads/new-upload") {
		t.Fatalf("a miss must list similar built artifacts: %+v %v", res, err)
	}
}
