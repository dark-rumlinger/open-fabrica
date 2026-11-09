package project

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"openfabrica/db/models"
	"openfabrica/events"
	"openfabrica/internal/buildstate"
	"openfabrica/internal/stringutil"
	"openfabrica/store"
	"openfabrica/tools"
)

// manage_tasks is the build agent's interface to the build checklist (of_tasks): the
// agent's working set and progress trace, ticked off and grown as it learns.
//
//   - complete: mark an item done as you finish it (drives the owner's progress
//               view; the build stops once every item is resolved). Refused if
//               the item declares owned paths and none are built yet, or declares
//               no paths and the whole project is still empty.
//   - split:    an item turned out bigger than one cohesive change - replace it
//               with N smaller items
//   - add:      you discovered work the plan missed - add it to the checklist
//   - block:    ship without this item and tell the owner why (counts as resolved;
//               it does not pause the build, a chat question does)
//   - list:     read the current checklist
//
// There is no "start" action: begin building an item directly and complete it once its
// deliverables exist. The checklist is unbounded and expected to churn.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_tasks",
		Category: "project",
		// Build-only: the checklist exists for the duration of a build. LIVE, owner chat,
		// and scheduled runs preload the same core tier, and this hides it from all three
		// rather than shipping a checklist tool (and its build-framed routing prose) into
		// stages that have no checklist and are told not to expand scope.
		Stages:             []string{"build"},
		FullResultInEvents: true,
		MaxResult:          16000, // a high-ambition checklist must list in full, never cut mid-object
		OneLiner:           "Track the build checklist: complete items, split them, add discovered work, or block one you ship without.",
		WhenToUse: `The checklist is your working set. split an item too big for one cohesive
change; add work the plan missed; block an item you will ship without (counts as
resolved, the owner is told why). To wait for the owner's answer, ask a chat
question instead. Prefer split/add over silently doing extra work: work the
checklist does not record is invisible to the owner.`,
		Actions: []tools.Action{
			&TasksComplete{},
			&TasksSplit{},
			&TasksAdd{},
			&TasksBlock{},
			&TasksList{},
		},
	})
}

// --- queue helpers (inline SQL; the tool layer never imports the engine package) ---

type taskRow struct {
	id       string
	deps     []string
	position int
}

func tasksLoadRows(ctx *tools.ToolContext) ([]taskRow, error) {
	rows, err := ctx.DB.Query(`SELECT id, COALESCE(deps,''), position FROM of_tasks ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []taskRow
	for rows.Next() {
		var r taskRow
		var deps string
		if err := rows.Scan(&r.id, &deps, &r.position); err != nil {
			return nil, err
		}
		r.deps = decodeJSONStrings(deps)
		out = append(out, r)
	}
	return out, rows.Err()
}

func tasksTakenAndMaxPos(rows []taskRow) (map[string]bool, int) {
	taken := make(map[string]bool, len(rows))
	maxPos := 0
	for _, r := range rows {
		taken[r.id] = true
		if r.position > maxPos {
			maxPos = r.position
		}
	}
	return taken, maxPos
}

func decodeJSONStrings(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// taskNeedsPWAEnable reports whether a task's declared tools include
// manage_project, whose sole build action (enable_pwa) is what makes a project
// installable. Used to gate completion on that enablement having actually run.
func taskNeedsPWAEnable(toolNames []string) bool {
	for _, t := range toolNames {
		if strings.EqualFold(strings.TrimSpace(t), "manage_project") {
			return true
		}
	}
	return false
}

func encodeJSONStrings(s []string) string {
	if len(s) == 0 {
		return ""
	}
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b)
}

// tasksFindDuplicate returns the id of a still-pending task that the proposed
// title duplicates, or "" if none. See the dedup comment in TasksAdd.Run for the
// three match rules. Scans non-done tasks in Go (rather than SQL) so the
// "<id>-<title-slug>" echo rule can compare against each row's own id+title.
func tasksFindDuplicate(ctx *tools.ToolContext, title string) string {
	cand := stringutil.TaskSlug(title)
	wantTitle := strings.ToLower(strings.TrimSpace(title))
	rows, err := ctx.DB.Query(`SELECT id, title FROM of_tasks WHERE status != 'done' ORDER BY position, id`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var id, t string
		if err := rows.Scan(&id, &t); err != nil {
			continue
		}
		if cand == id ||
			strings.ToLower(strings.TrimSpace(t)) == wantTitle ||
			cand == id+"-"+stringutil.TaskSlug(t) {
			return id
		}
	}
	_ = rows.Err() // best-effort dedup; a missed match just allows the add
	return ""
}

// stringList coerces a JSON-decoded []interface{} of strings into []string,
// skipping non-string / empty entries. Used for deps/paths args.
func stringList(v []string) []string {
	out := make([]string, 0, len(v))
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// emitTaskQueue publishes an engine.tasks snapshot so the owner-facing chat reflects
// mid-session queue mutations (add/split/complete/block) immediately. Best-effort: the
// payload mirrors engine.BuildTaskQueuePayload (whole-queue counts + a position-ordered
// task list). A no-op when no bus is wired.
func emitTaskQueue(ctx *tools.ToolContext) {
	if ctx.Bus == nil {
		return
	}
	rows, err := ctx.DB.Query(`SELECT id, title, status, COALESCE(origin,'')
		FROM of_tasks ORDER BY position, id`)
	if err != nil {
		return
	}
	defer rows.Close()

	const maxTaskQueueEvent = 60
	var total, done, active, pending, blocked int
	list := make([]map[string]any, 0)
	for rows.Next() {
		var id, title, status, origin string
		if err := rows.Scan(&id, &title, &status, &origin); err != nil {
			return
		}
		total++
		switch status {
		case buildstate.TaskDone:
			done++
		case buildstate.TaskActive:
			active++
		case buildstate.TaskBlocked:
			blocked++
		default:
			pending++
		}
		if len(list) < maxTaskQueueEvent {
			list = append(list, map[string]any{
				"id": id, "title": title, "status": status, "origin": origin,
			})
		}
	}
	if rows.Err() != nil {
		return
	}
	ctx.Bus.Publish(events.NewEvent(events.EventEngineTasks, ctx.ProjectID, map[string]any{
		"total": total, "done": done, "active": active,
		"pending": pending, "blocked": blocked, "tasks": list,
	}))
}

// --- complete ---

type TasksComplete struct {
	TaskID      string `arg:"task_id,required" desc:"Id of the task you finished."`
	Result      string `arg:"result"           desc:"Optional one-line note on what you built."`
	Conventions string `arg:"conventions"      desc:"A rule later tasks must follow: a data shape ('list endpoints return {items,total}'), a shared helper ('fetches go through js/api.js'), a naming pattern, an auth gate. Shown to every later task."`
	Force       bool   `arg:"force"            desc:"Override the completion checks: a declared owned path that genuinely cannot be produced (e.g. binary audio/video/image - manage_files writes text only) or was satisfied a different way, or a listed dep this task genuinely does not need. Requires reason. Never use it to skip real work, and never write placeholder/fake files to satisfy a path instead."`
	Reason      string `arg:"reason"           desc:"Why the declared path or dep is not satisfied and how the task's intent was met instead - must be set when force is true. Surfaced to the owner."`
}

func (a *TasksComplete) Doc() string {
	// WHEN to emit this (same turn as its save, never a housekeeping turn) is the build
	// frame's Track step. This Doc carries only what the CALL refuses, read at the point of
	// the call - the frame cannot enumerate that and should not try.
	return "Mark a checklist item done. Refused while an owned path it declares is not built (no matching file, table, or endpoint), while the project is still empty for an item with no paths, or while a task in its `deps` is neither complete nor built (build dependencies first). `conventions` records a decision later items must match (a data shape, a shared helper). If a declared path genuinely cannot be produced (binary media) or a dep is genuinely not needed, use `force=true` with a `reason` instead of placeholder files."
}
func (a *TasksComplete) Example() any {
	return &TasksComplete{TaskID: "staff-page", Result: "staff queue with advance and undo", Conventions: "list endpoints return {items, total}"}
}
func (a *TasksComplete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	// Resolve the task up front: the deliverable guard below needs its declared paths and
	// its current status (to answer an unknown or already-done task distinctly), and the
	// existence check subsumes the post-update 0-row "no task" branch.
	var pathsJSON, status, toolsJSON, depsJSON string
	if qerr := ctx.DB.QueryRow(`SELECT COALESCE(paths,''), status, COALESCE(tools,''), COALESCE(deps,'') FROM of_tasks WHERE id = ?`, a.TaskID).Scan(&pathsJSON, &status, &toolsJSON, &depsJSON); qerr != nil {
		return tools.Fail("no task with id '%s'", a.TaskID), nil
	}
	// An "already done" task must return a clear stop signal, not be re-flipped: the
	// unconditional `updated_at = CURRENT_TIMESTAMP` would otherwise make every re-complete
	// report fresh success, giving a model that calls complete twice a green light.
	if status == buildstate.TaskDone {
		return tools.Ok(map[string]any{
			"completed":    a.TaskID,
			"already_done": true,
			"note":         fmt.Sprintf("task '%s' was already complete; nothing more to do - stop here and let the loop advance to the next task", a.TaskID),
		}), nil
	}
	// Force override: a declared owned path can be one the build genuinely cannot produce
	// (binary audio/video/image - manage_files writes text only) or that the task satisfied
	// another way. `force` bypasses the deliverable guard, but only with a reason (recorded
	// and surfaced to the owner) so the override is auditable. It does NOT bypass the PWA
	// guard below: enable_pwa is always callable, so a missing manifest is a real omission.
	forced := false
	if a.Force {
		if strings.TrimSpace(a.Reason) == "" {
			return tools.Fail("force-complete of task '%s' needs a reason: explain why the declared path cannot be produced (e.g. binary media the build cannot author) and how you satisfied the task's intent instead.", a.TaskID), nil
		}
		forced = true
	}

	// Dependency-order guard: refuse to complete a task while a task it declares in `deps`
	// is still unfinished. The planner expresses real build order through deps (a page deps
	// on the table it reads; a subsystem leaf deps on the spine task owning the route +
	// shared entry module), so enforcing them makes "entry first, then leaves" structural
	// rather than advisory. Fail-open and force-overridable: a dep id resolving to no task
	// is ignored (a trimmed/merged plan never deadlocks), and a genuine out-of-order
	// completion uses the same force=true + reason escape.
	if !forced {
		if blockers := tasksIncompleteDeps(ctx, decodeJSONStrings(depsJSON)); len(blockers) > 0 {
			return tools.Fail("task '%s' depends on %v, which %s not complete yet. Build and complete %s first - a task is ordered AFTER its dependencies so the shared contract it builds against already exists (a parent table, or the entry/spine module a subsystem wires into; building a leaf before its entry is how a window.__ bridge to a missing contract gets invented). If this task genuinely does not need them, complete with force=true and a reason.",
				a.TaskID, blockers, pluralIsAre(len(blockers)), strings.Join(blockers, ", ")), nil
		}
	}

	// Deliverable guard: refuse to complete a task whose owned paths are not all built.
	// With no "start" action, a model can reach for `complete` as an "I'm beginning this"
	// signal, or batch-tick items whose deliverables only partially exist; requiring every
	// owned path makes `complete` per-task closure evidence. Two shapes of refusal:
	//   - The task declares owned paths and at least one is missing: not fully built.
	//   - The task declares NO paths and the WHOLE project is still empty: nothing anywhere
	//     for it to have finished. Once any prior task has built something the project is
	//     non-empty, so a legitimate path-less wiring/table task still completes.
	if !forced {
		if paths := decodeJSONStrings(pathsJSON); len(paths) > 0 {
			if missing := tasksMissingPaths(ctx, paths); len(missing) > 0 {
				return tools.Fail("task '%s' is not fully built: of its owned paths %v, no matching file, table, or endpoint exists yet for %v.%s Every owned path is closure evidence for the task; build the missing deliverables, then complete it (there is no 'start' action; just begin building). If a missing path genuinely cannot be produced (e.g. binary audio/video/image the build cannot author) and you have satisfied the task's intent another way, call complete again with force=true and a reason explaining how - do NOT write placeholder or fake files to satisfy the path.", a.TaskID, paths, missing, closestBuiltNote(ctx, missing)), nil
			}
		} else if !tasksProjectHasAnyArtifact(ctx) {
			return tools.Fail("task '%s' has no built artifacts and the project is still empty - there is nothing to complete yet. Build its deliverables first, then complete it (there is no 'start' action; just begin building).", a.TaskID), nil
		}
	}

	// User-flows guard: the engine-added item owns no paths, so only this sees whether of_test.js
	// was written. A skipped or out-of-budget replay is fine, a missing script is not, unless the
	// owner turned user-flow tests off.
	if !forced && a.TaskID == buildstate.UserFlowsTaskID && !store.ArtifactExists(ctx.DB, "test", store.FlowScriptPath) && !userFlowsTurnedOff(ctx) {
		return tools.Fail("task '%s' writes the user flows, but no %s is saved yet. Save it with `manage_testing action=write_flows` (it replays as it saves), then complete this task.", a.TaskID, store.FlowScriptPath), nil
	}

	// PWA enablement guard: the planner ends an installable/offline project with a task
	// scoped to manage_project, whose only build action (enable_pwa) generates
	// manifest.json + service-worker.js and patches the layout. A model can otherwise tick
	// the task done with a prose result while never calling the action, shipping a
	// non-installable app. Refuse until enable_pwa has run, keyed on the manifest existing.
	if taskNeedsPWAEnable(decodeJSONStrings(toolsJSON)) && !store.ArtifactExists(ctx.DB, "file", "manifest.json") {
		return tools.Fail("task '%s' enables the PWA (it is scoped to manage_project) but no manifest.json exists - the enable_pwa action has not run. Call `manage_project action=enable_pwa` as the final step (after every page is saved), then complete this task.", a.TaskID), nil
	}

	// On a force-complete, record the override reason on the task so the owner's
	// progress view shows it was forced and why; the [forced] prefix marks it
	// distinctly from a cleanly-completed task (any result note rides along after).
	result := a.Result
	if forced {
		result = "[forced] " + strings.TrimSpace(a.Reason)
		if note := strings.TrimSpace(a.Result); note != "" {
			result += " - " + note
		}
	}

	// Guard the update on a non-terminal status as a race backstop; the
	// status check above already handles the common already-done call.
	res, err := ctx.DB.Exec(
		`UPDATE of_tasks SET status = 'done', result = ?, conventions = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status != 'done'`,
		result, a.Conventions, a.TaskID,
	)
	if err != nil {
		return tools.FailErr(err), nil
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The task existed and was non-done when resolved above, so a 0-row
		// update here means a concurrent complete won the race. Report it as the
		// same idempotent already-done stop signal.
		return tools.Ok(map[string]any{
			"completed":    a.TaskID,
			"already_done": true,
			"note":         fmt.Sprintf("task '%s' was already complete; nothing more to do - stop here and let the loop advance to the next task", a.TaskID),
		}), nil
	}
	emitTaskQueue(ctx)
	out := map[string]any{"completed": a.TaskID}
	if forced {
		out["forced"] = true
	}
	return tools.Ok(out), nil
}

// tasksIncompleteDeps returns, in declared order, the subset of depIDs that are a genuine
// blocker: a task that EXISTS, is not yet 'done', AND has not built its declared
// deliverables. A dep BUILT but not yet marked done is NOT a blocker (its shared contract
// already exists), which lets a model batch completes in one turn while the guard still
// refuses a complete whose dependency has built nothing. A dep id matching no task row is
// dropped, not treated as a blocker, so a trimmed/merged plan never wedges a task
// uncompletable. Fail-open on a query error, like the deliverable guards.
func tasksIncompleteDeps(ctx *tools.ToolContext, depIDs []string) []string {
	var blockers []string
	for _, id := range depIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		var depStatus, depPaths string
		// COALESCE: a path-less task stores NULL paths, which would fail a scan into
		// string and wrongly drop the dep (a scan error is treated as "no such task").
		if err := ctx.DB.QueryRow(`SELECT status, COALESCE(paths, '') FROM of_tasks WHERE id = ?`, id).Scan(&depStatus, &depPaths); err != nil {
			continue // no such task (trimmed/merged) or a transient error: do not block
		}
		// A blocked dep is resolved the way the build loop counts it (shipped without,
		// knowingly); holding its dependents back would wedge them behind a decision made.
		if depStatus == buildstate.TaskDone || depStatus == buildstate.TaskBlocked {
			continue
		}
		// Built-but-not-yet-marked-done satisfies the dep: its contract already exists.
		// A path-less dep has no deliverable to probe, so it stays strict (must be done).
		if paths := decodeJSONStrings(depPaths); len(paths) > 0 && tasksAnyPathBuilt(ctx, paths) {
			continue
		}
		blockers = append(blockers, id)
	}
	return blockers
}

// pluralIsAre picks the verb agreeing with n, for the dependency-guard message.
func pluralIsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// artifactStoreQueries lists, per artifact store, a query over the path-like column of
// every built artifact there. A declared task path is drawn from three namespaces
// (file/page/layout, table, or endpoint resource), so both deliverable guards consult
// every store: of_files holds files/pages/layouts (the live filter excludes soft-deleted);
// of_dynamic_tables holds tables; sqlite_master holds SQL views (NOT LIKE 'of_%').
//
// Endpoints are covered in two tiers. of_endpoints is the spine: every kind mounted on
// /api/ claims into it (api.ClaimEndpoint), recording the reserved sub_segment its routes
// use (stream, upload, ws, export, chat, ...) exactly as api.endpointRoutesFor - the single
// definition of those sub-segments - lays them out. So the spine is read TWICE: once for the
// bare resource path, and once for the public route form path||'/'||sub_segment. That route
// form is the identity a planner declares a sub-segment-routed task by: an SSE stream over
// the `orders` resource is declared "orders/stream" (the route it is reachable at,
// /api/orders/stream), never the bare "orders". Reconstructing it from the spine's own
// sub_segment is what makes this tier drift-proof - a new kind added to endpointRoutesFor is
// matched the instant it claims, with no per-kind route convention re-encoded here. The
// per-kind queries cover what the spine does not: the bare resource for feeds (their own
// /feed/ namespace, never claimed) and for projects predating the spine. A path found in any
// tier is built.
//
// Keep this list authoritative: omitting a real endpoint store makes its built endpoint
// look unbuilt, and tasksAnyPathBuilt and tasksProjectHasAnyArtifact must never drift apart.
var artifactStoreQueries = []string{
	`SELECT path FROM of_files WHERE is_deleted = 0`,
	`SELECT table_name FROM of_dynamic_tables`,
	`SELECT path FROM of_endpoints`,
	// Route form: <resource>/<sub_segment> for every spine route that has a reserved
	// sub-segment (SSE's "stream", upload's "upload", ws's "ws"/"rooms", ...). This is
	// the same string routeLabel renders as /api/<path>/<sub_segment>, minus the /api/
	// prefix task paths omit. sub_segment='' rows (crud/function, whose route IS the bare
	// path) are already covered by the query above and excluded here.
	`SELECT DISTINCT path || '/' || sub_segment FROM of_endpoints WHERE sub_segment != ''`,
	// Views are real, declarable deliverables (the platform recommends "view + read API").
	// They live only in sqlite_master (no of_views registry), so without this a task whose
	// deliverable IS a view can never satisfy the gate. NOT LIKE 'of_%' excludes
	// platform-internal views, matching the user-view convention in SchemaList.
	`SELECT name FROM sqlite_master WHERE type = 'view' AND name NOT LIKE 'of_%'`,
	`SELECT path FROM of_api_endpoints`,
	`SELECT path FROM of_function_endpoints`,
	`SELECT path FROM of_ws_endpoints`,
	`SELECT path FROM of_stream_endpoints`,
	`SELECT path FROM of_upload_endpoints`,
	`SELECT path FROM of_llm_endpoints`,
	`SELECT path FROM of_export_endpoints`,
	`SELECT path FROM of_document_endpoints`,
	`SELECT path FROM of_import_endpoints`,
	`SELECT path FROM of_feed_endpoints`,
	`SELECT path FROM of_push_endpoints`,
	// Query/FTS read endpoints (manage_api enable_query) plus TOTP and magic-link
	// endpoints each key on a `path` column, so a task owning a search / 2FA /
	// magic-link deliverable completes normally instead of being force-completed.
	`SELECT path FROM of_query_endpoints`,
	`SELECT path FROM of_totp_endpoints`,
	`SELECT path FROM of_magic_link_endpoints`,
	// of_auth_endpoints holds JWT auth endpoints (manage_auth create); its `path` column
	// keys the endpoint, so a task owning the auth path completes normally.
	// of_oauth_providers is deliberately NOT queried: it has no `path` column, and a
	// bad-column query fails open (disabling the whole guard), so OAuth tasks ride the auth
	// endpoint's path instead.
	`SELECT path FROM of_auth_endpoints`,
	// Scheduled tasks are deliverables too ("purge expired links nightly"): a task that owns
	// one by name completes once the schedule exists.
	`SELECT name FROM of_scheduled_tasks`,
}

// tasksAnyPathBuilt reports whether any of the task's declared paths already exists as a
// built artifact across artifactStoreQueries. A single match proves the task built
// something; the dependency guard uses it (a dep whose contract exists unblocks its
// dependents even before it is marked done), while the complete guard requires every
// path via tasksMissingPaths. Each query fails open on infra trouble so a build is
// never blocked by a transient error.
func tasksAnyPathBuilt(ctx *tools.ToolContext, paths []string) bool {
	want := make(map[string]bool, len(paths)*3)
	for _, p := range paths {
		for _, f := range taskPathForms(p) {
			want[f] = true
		}
	}
	delete(want, "")
	if len(want) == 0 {
		return true
	}
	for _, q := range artifactStoreQueries {
		if tasksStoreHasPath(ctx, q, want) {
			return true
		}
	}
	return false
}

// tasksMissingPaths returns the declared paths with no matching built artifact in any
// store. Closure evidence is per path: a task completes only when every owned path
// exists; force=true with a reason stays the escape for a path the build genuinely
// cannot produce. Store queries fail open per store (see tasksStoreHasPath), so a
// transient error can only hide a miss, never invent one.
func tasksMissingPaths(ctx *tools.ToolContext, paths []string) []string {
	var missing []string
	for _, p := range paths {
		want := map[string]bool{}
		for _, f := range taskPathForms(p) {
			want[f] = true
		}
		delete(want, "")
		if len(want) == 0 {
			continue
		}
		built := false
		for _, q := range artifactStoreQueries {
			if tasksStoreHasPath(ctx, q, want) {
				built = true
				break
			}
		}
		// A planner often declares a page by its bare name ("login") while the page is
		// saved at its route ("/login"). Page routes never collapse to a bare form, so try
		// the route form as a last resort before calling the deliverable missing.
		if !built && !strings.HasPrefix(strings.TrimSpace(p), "/") && !strings.ContainsAny(p, ".") {
			route := map[string]bool{}
			for _, f := range taskPathForms("/" + strings.TrimSpace(p)) {
				route[f] = true
			}
			delete(route, "")
			for _, q := range artifactStoreQueries {
				if tasksStoreHasPath(ctx, q, route) {
					built = true
					break
				}
			}
		}
		if !built {
			missing = append(missing, p)
		}
	}
	return missing
}

// tasksProjectHasAnyArtifact reports whether the project has built anything yet (at least
// one row in any artifactStoreQueries store). It backstops the deliverable guard for
// PATH-LESS tasks: the path check skips them, so a model could otherwise fire `complete` as
// its first action against an empty project. Once any task has built something a legitimate
// path-less wiring/table task completes. Fails open on a query error so a transient store
// failure never blocks a completion, matching tasksStoreHasPath.
func tasksProjectHasAnyArtifact(ctx *tools.ToolContext) bool {
	for _, q := range artifactStoreQueries {
		rows, err := ctx.DB.Query(q)
		if err != nil {
			return true // cannot prove emptiness; do not block the completion
		}
		has := rows.Next()
		iterErr := rows.Err()
		_ = rows.Close()
		if iterErr != nil {
			return true // cannot prove emptiness; do not block the completion
		}
		if has {
			return true
		}
	}
	return false
}

// tasksStoreHasPath runs a single-column query and reports whether any returned
// value, reduced to its path forms, is in want. Returns true (fail open) if the
// query itself errors, so a missing or locked store never blocks a completion.
func tasksStoreHasPath(ctx *tools.ToolContext, query string, want map[string]bool) bool {
	rows, err := ctx.DB.Query(query)
	if err != nil {
		return true
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			continue
		}
		for _, f := range taskPathForms(p) {
			if want[f] {
				return true
			}
		}
	}
	if rows.Err() != nil {
		return true // fail open, matching the query-error stance above
	}
	return false
}

// taskPathForms returns the comparable forms of a path, so planner-declared task
// paths and stored artifact paths line up despite cosmetic differences:
//   - lowercased raw form, plus a bare form (leading slash and "files/" URL prefix
//     stripped), so "/css/app.css", "files/css/app.css" and "css/app.css" all
//     reduce to "css/app.css" while "/" or "default" match on their raw form;
//   - route-param-normalized form, collapsing any [slug]/:slug/{slug}/<slug> to a
//     placeholder, so "/projects/[slug]" (or "/<username>") matches the page saved
//     at "/projects/:slug" (runtime router uses Express :slug; others steered to it);
//   - separator-unified form ('-' folded to '_') in the resource namespace, so a
//     view/table "metrics_overview" matches its endpoint "metrics-overview";
//   - a layout alias, so a shell task declaring "/layout" or "layout" matches the
//     shared layout stored at the canonical path "default".
func taskPathForms(p string) []string {
	raw := strings.ToLower(strings.TrimSpace(p))
	// A clean page route ("/activity", "/projects/[slug]") is a different
	// namespace than a resource name ("activity"): it must NOT reduce to a bare
	// form, or a same-named table/endpoint would falsely satisfy a page task -
	// the page-route/resource collision that lets a page task complete with only
	// its backend built. Keep the leading slash; still normalize route params so
	// a declared "/projects/[slug]" matches a page saved at "/projects/:slug".
	if isCleanPageRoute(raw) {
		forms := []string{raw}
		if norm := normalizeRouteParams(raw); norm != raw {
			forms = append(forms, norm)
		}
		return forms
	}
	bare := strings.TrimPrefix(raw, "/")
	bare = strings.TrimPrefix(bare, "files/")
	forms := []string{raw, bare}
	if norm := normalizeRouteParams(bare); norm != bare {
		forms = append(forms, norm)
	}
	// Unify '-' and '_' to a single separator so a declared resource name matches
	// an artifact stored with the opposite convention: a view/table uses the SQL
	// identifier "metrics_overview" while its read endpoint is the URL-style
	// "metrics-overview". Both sides run through taskPathForms, so emitting the
	// canonical form on each side makes them meet. Scoped to the resource (bare)
	// namespace, never the clean-page-route branch above, to preserve the
	// page-route/resource split.
	if sep := strings.ReplaceAll(bare, "-", "_"); sep != bare {
		forms = append(forms, sep)
	}
	if bare == "layout" || bare == "default" {
		forms = append(forms, "layout", "default")
	}
	// A module file also matches by basename: the plan owns "/files/app.js" while a
	// build that followed an older example saved "js/app.js". Both resolve to the same
	// bare import specifier, which must be unique in a project anyway, so the basename
	// identifies the module. Scoped to extensioned names (files), never resources.
	if base := path.Base(bare); strings.Contains(base, ".") && base != bare {
		forms = append(forms, "file:"+base)
	} else if strings.Contains(bare, ".") {
		forms = append(forms, "file:"+bare)
	}
	return forms
}

// isCleanPageRoute reports whether a path is a navigable page route rather than a resource
// name, static asset, or layout alias: root-absolute, not a /files/ asset, not the
// layout/home alias, and its final segment extension-less (a dotted final segment is a
// static asset). Such routes must never collapse to a bare form, so "/activity" is never
// satisfied by a table or endpoint named "activity". "/", "/layout", and "default" return
// false to preserve their bare-form / layout-alias handling.
func isCleanPageRoute(raw string) bool {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "/files/") {
		return false
	}
	bare := strings.TrimPrefix(raw, "/")
	if bare == "" || bare == "layout" || bare == "default" {
		return false
	}
	last := bare
	if i := strings.LastIndex(bare, "/"); i >= 0 {
		last = bare[i+1:]
	}
	return !strings.Contains(last, ".")
}

// normalizeRouteParams collapses every dynamic segment of a route to a single
// ":p" placeholder, so the param syntaxes ([slug], :slug, {slug}, <slug>) compare
// equal. Static segments are left untouched.
func normalizeRouteParams(route string) string {
	segs := strings.Split(route, "/")
	for i, s := range segs {
		if isRouteParam(s) {
			segs[i] = ":p"
		}
	}
	return strings.Join(segs, "/")
}

// isRouteParam reports whether a single path segment is a dynamic route parameter in any
// syntax a planner might emit: [slug], {slug}, <slug>, or :slug. The runtime router only
// understands :slug, but a task may declare its path in another form, so completion treats
// them all as the same param.
func isRouteParam(s string) bool {
	if len(s) < 2 {
		return false
	}
	return (s[0] == '[' && s[len(s)-1] == ']') ||
		(s[0] == '{' && s[len(s)-1] == '}') ||
		(s[0] == '<' && s[len(s)-1] == '>') ||
		s[0] == ':'
}

// --- split ---

type TasksSplit struct {
	TaskID   string `arg:"task_id,required" desc:"Id of the task to split."`
	Subtasks []any  `arg:"subtasks,required" desc:"Array of subtask objects: [{\"title\":\"...\",\"intent\":\"what to build\",\"done_when\":\"...\",\"paths\":[\"/files/x.js\"],\"edits\":[\"/files/shared.js\"]}, ...]. paths = files the subtask owns; edits (optional) = files another task owns that it modifies. Each becomes a pending task that inherits the parent's dependencies."`
}

func (a *TasksSplit) Doc() string {
	return "Replace one oversized task with focused subtasks; the parent is marked done and dependents are re-pointed to the subtasks."
}
func (a *TasksSplit) Example() any {
	return &TasksSplit{TaskID: "editor-page", Subtasks: []any{
		map[string]any{"title": "Canvas render loop", "intent": "render + raf loop", "paths": []any{"/files/render.js"}},
		map[string]any{"title": "Tool palette", "intent": "tool selection UI", "paths": []any{"/files/tools.js"}},
	}}
}
func (a *TasksSplit) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if len(a.Subtasks) == 0 {
		return tools.Fail("subtasks array is required"), nil
	}
	rows, err := tasksLoadRows(ctx)
	if err != nil {
		return tools.FailErr(err), nil
	}
	var parent *taskRow
	for i := range rows {
		if rows[i].id == a.TaskID {
			parent = &rows[i]
			break
		}
	}
	if parent == nil {
		return tools.FailCode(tools.CodeNotFound, "no task with id '%s' - manage_tasks action=list shows the checklist ids", a.TaskID), nil
	}
	var parentStatus string
	_ = ctx.DB.QueryRow(`SELECT status FROM of_tasks WHERE id = ?`, a.TaskID).Scan(&parentStatus)
	if parentStatus == "done" {
		return tools.FailCode(tools.CodeBadArg, "task '%s' is already done; add follow-up work with action=add instead of splitting it", a.TaskID), nil
	}
	taken, maxPos := tasksTakenAndMaxPos(rows)

	tx, err := ctx.DB.Begin()
	if err != nil {
		return tools.FailErr(err), nil
	}
	defer tx.Rollback()

	subIDs := make([]string, 0, len(a.Subtasks))
	for i, raw := range a.Subtasks {
		m, ok := raw.(map[string]any)
		if !ok {
			return tools.Fail("subtasks[%d] must be an object", i), nil
		}
		title, _ := m["title"].(string)
		if strings.TrimSpace(title) == "" {
			return tools.Fail("subtasks[%d].title is required", i), nil
		}
		intent, _ := m["intent"].(string)
		doneWhen, _ := m["done_when"].(string)
		paths := jsonAnyToStrings(m["paths"])
		edits := jsonAnyToStrings(m["edits"])
		id := stringutil.UniqueTaskSlug("", title, taken)
		taken[id] = true
		maxPos++
		if _, err := tx.Exec(`INSERT INTO of_tasks
			(id, title, intent, done_when, status, deps, paths, edits, origin, position)
			VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, 'split', ?)`,
			id, title, intent, doneWhen, encodeJSONStrings(parent.deps), encodeJSONStrings(paths), encodeJSONStrings(edits), maxPos,
		); err != nil {
			return tools.FailErr(err), nil
		}
		subIDs = append(subIDs, id)
	}

	if _, err := tx.Exec(
		`UPDATE of_tasks SET status = 'done', result = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		"split into "+strings.Join(subIDs, ", "), a.TaskID,
	); err != nil {
		return tools.FailErr(err), nil
	}

	// Re-point dependents so they wait for the whole split, not the (now-done) parent.
	for _, r := range rows {
		if !slices.Contains(r.deps, a.TaskID) {
			continue
		}
		newDeps := make([]string, 0, len(r.deps)+len(subIDs))
		for _, d := range r.deps {
			if d != a.TaskID {
				newDeps = append(newDeps, d)
			}
		}
		newDeps = append(newDeps, subIDs...)
		if _, err := tx.Exec(
			`UPDATE of_tasks SET deps = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
			encodeJSONStrings(newDeps), r.id,
		); err != nil {
			return tools.FailErr(err), nil
		}
	}

	if err := tx.Commit(); err != nil {
		return tools.FailErr(err), nil
	}
	emitTaskQueue(ctx)
	return tools.Ok(map[string]any{"split": a.TaskID, "into": subIDs}), nil
}

// --- add ---

type TasksAdd struct {
	Title    string   `arg:"title,required" desc:"Short task title."`
	Intent   string   `arg:"intent"         desc:"What to build, in prose."`
	DoneWhen string   `arg:"done_when"      desc:"Verifiable completion criteria for this task."`
	Deps     []string `arg:"deps"           desc:"Ids of tasks that must finish before this one is ready."`
	Paths    []string `arg:"paths"          desc:"Artifact paths this task owns - the files it creates. Scopes the task so no two tasks own the same file."`
	Edits    []string `arg:"edits"          desc:"Routes or resources this task extends but another task owns (a page route, or a leaf's spine entry module). List here, not in paths; they are modified in place, not re-created."`
}

func (a *TasksAdd) Doc() string { return "Enqueue a new task for work the plan missed." }
func (a *TasksAdd) Example() any {
	return &TasksAdd{Title: "Add CSV export", Intent: "export the reports table as CSV", Deps: []string{"reports-table"}}
}
func (a *TasksAdd) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := tasksLoadRows(ctx)
	if err != nil {
		return tools.FailErr(err), nil
	}

	// Dedup: refuse to enqueue a task that duplicates one already in the queue.
	// taskUniqueSlug would otherwise auto-suffix a fresh id (foo, foo-2, ...) so a model
	// re-issuing the same add never fails and silently grows the queue. A still-pending
	// task duplicates the proposed title when ANY of these match (via taskSlugify);
	// re-adding something already done is allowed:
	//   - the title slugifies to an existing id (the base slug), or
	//   - the title equals an existing title, or
	//   - the title slugifies to "<id>-<title-slug>" of an existing task (the agent echoing
	//     a rendered checklist line back as a new title). Exact slug equality, no false
	//     positives from a shared prefix.
	if existingID := tasksFindDuplicate(ctx, a.Title); existingID != "" {
		return tools.Fail("a task titled %q is already queued as '%s' and has not been built yet - it does not need to be added again; the loop will reach it. If you meant a different piece of work, give it a distinct title.", a.Title, existingID), nil
	}

	taken, maxPos := tasksTakenAndMaxPos(rows)
	for _, d := range stringList(a.Deps) {
		if !taken[d] {
			return tools.FailCode(tools.CodeBadArg, "deps names %q, which is not a task id - manage_tasks action=list shows the ids", d), nil
		}
	}
	id := stringutil.UniqueTaskSlug("", a.Title, taken)
	if _, err := ctx.DB.Exec(`INSERT INTO of_tasks
		(id, title, intent, done_when, status, deps, paths, edits, origin, position)
		VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, 'discovered', ?)`,
		id, a.Title, a.Intent, a.DoneWhen,
		encodeJSONStrings(stringList(a.Deps)), encodeJSONStrings(stringList(a.Paths)), encodeJSONStrings(stringList(a.Edits)), maxPos+1,
	); err != nil {
		return tools.FailErr(err), nil
	}
	emitTaskQueue(ctx)
	return tools.Ok(map[string]any{"added": id}), nil
}

// --- block ---

type TasksBlock struct {
	TaskID string `arg:"task_id,required" desc:"Id of the task you are blocked on."`
	Reason string `arg:"reason,required"  desc:"Why you are shipping without it (surfaced to the owner)."`
	// NeedsOwner marks a block only the owner's answer or secret can lift.
	NeedsOwner bool `arg:"needs_owner"     desc:"true if only the owner's answer or secret unblocks it."`
}

func (a *TasksBlock) Doc() string {
	return "Mark a task blocked: you ship without it and the owner sees why. Set needs_owner when only the owner's answer or secret can unblock it: their next answer reopens exactly those items. To wait for an answer, ask a chat question."
}
func (a *TasksBlock) Example() any {
	return &TasksBlock{TaskID: "stripe-webhook", Reason: "needs the owner's Stripe secret key"}
}
func (a *TasksBlock) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var status string
	if err := ctx.DB.QueryRow(`SELECT status FROM of_tasks WHERE id = ?`, a.TaskID).Scan(&status); err != nil {
		return tools.FailCode(tools.CodeNotFound, "no task with id '%s' - manage_tasks action=list shows the checklist ids", a.TaskID), nil
	}
	if status == "done" {
		return tools.FailCode(tools.CodeBadArg, "task '%s' is already done; block is for an item you will ship without", a.TaskID), nil
	}
	blockedOn := ""
	if a.NeedsOwner {
		blockedOn = "owner"
	}
	if _, err := ctx.DB.Exec(
		`UPDATE of_tasks SET status = 'blocked', blocked_on = ?, result = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		blockedOn, a.Reason, a.TaskID,
	); err != nil {
		return tools.FailErr(err), nil
	}
	emitTaskQueue(ctx)
	return tools.Ok(map[string]any{"blocked": a.TaskID}), nil
}

// --- list ---

type TasksList struct{}

func (a *TasksList) Doc() string    { return "Return the current work queue." }
func (a *TasksList) ReadOnly() bool { return true }
func (a *TasksList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(`SELECT id, title, status, COALESCE(done_when,''), COALESCE(deps,''), COALESCE(result,''), COALESCE(conventions,''),
		COALESCE(paths,''), COALESCE(edits,'')
		FROM of_tasks ORDER BY position, id`)
	if err != nil {
		return tools.FailErr(err), nil
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, title, status, doneWhen, deps, result, conventions, paths, edits string
		if err := rows.Scan(&id, &title, &status, &doneWhen, &deps, &result, &conventions, &paths, &edits); err != nil {
			return tools.FailErr(err), nil
		}
		item := map[string]any{"id": id, "title": title, "status": status}
		if doneWhen != "" {
			item["done_when"] = doneWhen
		}
		if d := decodeJSONStrings(deps); len(d) > 0 {
			item["deps"] = d
		}
		// paths are what complete gates on, so the list shows them: without them the
		// model learned what an item requires only from a refused complete.
		if p := decodeJSONStrings(paths); len(p) > 0 {
			item["paths"] = p
		}
		if e := decodeJSONStrings(edits); len(e) > 0 {
			item["edits"] = e
		}
		if result != "" {
			item["result"] = result
		}
		if conventions != "" {
			item["conventions"] = conventions
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return tools.FailErr(err), nil
	}
	return tools.Ok(map[string]any{"tasks": out, "count": len(out)}), nil
}

// --- shared small helpers ---

// jsonAnyToStrings coerces a JSON-decoded value (string, []interface{}, or nil)
// into a []string, used for subtask "paths" fields inside the split payload.
func jsonAnyToStrings(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(x) == "" {
			return nil
		}
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, el := range x {
			if s, ok := el.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// userFlowsTurnedOff reports whether the owner turned user-flow tests off, so the checklist
// item can be completed without a script. An unreadable project row counts as off, the
// same default the engine uses when it decides whether the item exists at all (a tool that
// demanded a script for an item the engine dropped could never be satisfied).
func userFlowsTurnedOff(ctx *tools.ToolContext) bool {
	if ctx.GlobalDB == nil {
		return false
	}
	p, err := models.GetProjectByID(ctx.GlobalDB, ctx.ProjectID)
	return err != nil || !models.ProjectUserFlowsEnabled(p)
}

// closestBuiltNote names built artifacts that share a word with a missing owned path, so
// a task whose path is spelled differently from what was built (a page saved at another
// route, a table under another name) can be completed with force and a reason instead of
// retrying the same call. Empty when nothing resembles any of them.
func closestBuiltNote(ctx *tools.ToolContext, missing []string) string {
	words := map[string]bool{}
	for _, m := range missing {
		for _, w := range pathWords(m) {
			words[w] = true
		}
	}
	if len(words) == 0 {
		return ""
	}
	var near []string
	seen := map[string]bool{}
	for _, q := range artifactStoreQueries {
		rows, err := ctx.DB.Query(q)
		if err != nil {
			continue
		}
		for rows.Next() && len(near) < 8 {
			var built string
			if rows.Scan(&built) != nil || seen[built] {
				continue
			}
			for _, w := range pathWords(built) {
				if words[w] {
					seen[built] = true
					near = append(near, built)
					break
				}
			}
		}
		_ = rows.Err()
		_ = rows.Close()
	}
	if len(near) == 0 {
		return ""
	}
	return fmt.Sprintf(" Built artifacts with a similar name: %s; if one of them IS the deliverable, complete with force=true and say which.", strings.Join(near, ", "))
}

// pathWords splits a path or name into its lowercase alphanumeric words of 3+ letters.
func pathWords(p string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(p), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(w) >= 3 && w != "files" && w != "api" {
			out = append(out, w)
		}
	}
	return out
}
