package engine

// The engine's durable build checklist. Every plan item - seeded by PLAN or
// added by the agent or owner - is one row in of_tasks. There is no in-memory
// queue: of_tasks IS the checklist, so a crash mid-build resumes by
// re-reading it (done rows render checked, the rest re-queue), and
// buildTurnEnd stops the build session once every row is done or blocked.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/db"
	"openfabrica/internal/buildstate"
	"openfabrica/internal/stringutil"
)

// Task status values (defined in internal/buildstate, shared with manage_tasks).
const (
	TaskPending = buildstate.TaskPending
	TaskActive  = buildstate.TaskActive
	TaskDone    = buildstate.TaskDone
	TaskBlocked = buildstate.TaskBlocked
)

// BlockedOnOwner is the of_tasks.blocked_on value for an item only the owner can unblock.
const BlockedOnOwner = "owner"

// TaskOriginPlan marks rows seeded by the PLAN stage. The manage_tasks tool
// writes its own origins ('discovered', 'split') for agent-added work.
const TaskOriginPlan = "plan"

// Task is one unit of work in the queue.
type Task struct {
	ID       string   `json:"id"`
	RunID    string   `json:"run_id,omitempty"`
	Title    string   `json:"title"`
	Intent   string   `json:"intent"`
	DoneWhen string   `json:"done_when,omitempty"`
	Status   string   `json:"status"`
	Deps     []string `json:"deps,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Edits    []string `json:"edits,omitempty"`
	// Tools are the non-core tool names the planner declared this task needs;
	// they preload into the build session so no mid-session find_tool (and no
	// provider-cache bust) is needed for planned work.
	Tools []string `json:"tools,omitempty"`
	// Exports are the symbols this task's module(s) publish for sibling tasks to
	// import - the planner-declared leaf interface, rendered in the build checklist
	// so a consumer imports the accessor instead of stubbing it on a window global.
	Exports     []string `json:"exports,omitempty"`
	Origin      string   `json:"origin,omitempty"`
	Result      string   `json:"result,omitempty"`
	Conventions string   `json:"conventions,omitempty"`
	Position    int      `json:"position"`
}

// LoadTasks returns every task in queue order (position, then id).
func LoadTasks(d *sql.DB) ([]Task, error) {
	rows, err := d.Query(`SELECT id, COALESCE(run_id,''), title, intent, COALESCE(done_when,''), status,
		COALESCE(deps,''), COALESCE(paths,''), COALESCE(edits,''), COALESCE(tools,''), COALESCE(exports,''),
		COALESCE(origin,''), COALESCE(result,''), COALESCE(conventions,''), position
		FROM of_tasks ORDER BY position, id`)
	if err != nil {
		return nil, fmt.Errorf("load tasks: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var t Task
		var deps, paths, edits, tools, exports string
		if err := rows.Scan(&t.ID, &t.RunID, &t.Title, &t.Intent, &t.DoneWhen, &t.Status,
			&deps, &paths, &edits, &tools, &exports, &t.Origin, &t.Result, &t.Conventions, &t.Position); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		t.Deps = decodeStringSlice(deps)
		t.Paths = decodeStringSlice(paths)
		t.Edits = decodeStringSlice(edits)
		t.Tools = decodeStringSlice(tools)
		t.Exports = decodeStringSlice(exports)
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddTasks inserts new tasks, assigning a unique slug id (from the title) to
// any task with an empty id and appending to the queue end. Returns the ids
// assigned, in input order.
func AddTasks(sdb *db.ProjectDB, runID string, tasks []Task) ([]string, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	existing, err := LoadTasks(sdb.Reader())
	if err != nil {
		return nil, err
	}
	taken := make(map[string]bool, len(existing))
	maxPos := 0
	for _, t := range existing {
		taken[t.ID] = true
		if t.Position > maxPos {
			maxPos = t.Position
		}
	}

	ids := make([]string, 0, len(tasks))
	for i := range tasks {
		t := tasks[i]
		t.ID = stringutil.UniqueTaskSlug(t.ID, t.Title, taken)
		taken[t.ID] = true
		if t.Status == "" {
			t.Status = TaskPending
		}
		if t.RunID == "" {
			t.RunID = runID
		}
		maxPos++
		if _, err := sdb.ExecWrite(`INSERT INTO of_tasks
			(id, run_id, title, intent, done_when, status, deps, paths, edits, tools, exports, origin, result, position)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.ID, t.RunID, t.Title, t.Intent, t.DoneWhen, t.Status,
			encodeStringSlice(t.Deps), encodeStringSlice(t.Paths), encodeStringSlice(t.Edits),
			encodeStringSlice(t.Tools), encodeStringSlice(t.Exports), t.Origin, t.Result, maxPos,
		); err != nil {
			return ids, fmt.Errorf("insert task %q: %w", t.ID, err)
		}
		ids = append(ids, t.ID)
	}
	return ids, nil
}

// ClearTasks empties the queue. Called when a fresh plan reseeds so a re-plan
// never inherits a stale queue.
func ClearTasks(sdb *db.ProjectDB) error {
	_, err := sdb.ExecWrite(`DELETE FROM of_tasks`)
	return err
}

// ReopenBlockedTasks flips the checklist items blocked on the owner back to pending,
// returning how many it reopened. Called when the owner's answer resumes a build: an item
// blocked on the owner is exactly what the answer unblocks, and `blocked` counts as
// resolved, so left alone it would ship unbuilt without the build ever looking at it
// again. An item blocked for any other reason (a capability the platform lacks) stays
// blocked: the answer does not change it. One still blocked after the answer is simply
// blocked again.
func ReopenBlockedTasks(sdb *db.ProjectDB) (int64, error) {
	res, err := sdb.ExecWrite(`UPDATE of_tasks SET status = ?, blocked_on = '', updated_at = CURRENT_TIMESTAMP WHERE status = ? AND blocked_on = ?`,
		TaskPending, TaskBlocked, BlockedOnOwner)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- small helpers ---

func decodeStringSlice(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func encodeStringSlice(s []string) string {
	if len(s) == 0 {
		return ""
	}
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b)
}
