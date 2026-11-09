package admin

import "net/http"

// BuildQueue returns the project's current unified-loop work queue (of_tasks)
// as a snapshot: whole-queue status counts plus a position-ordered task list.
// The chat view fetches this on mount to seed the build-queue baseline, since
// the engine.tasks SSE stream (state.select) does not replay on load. The payload
// shape mirrors engine.BuildTaskQueuePayload so the chat narration behaves
// identically whether seeded here or updated live.
func (h *ProjectTasksHandler) BuildQueue(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	rows, err := projectDB.Query(`SELECT id, title, status, COALESCE(origin,'')
		FROM of_tasks ORDER BY position, id`)
	if err != nil {
		// No of_tasks table yet (project never built) -> empty queue, not an error.
		writeJSON(w, http.StatusOK, emptyBuildQueue())
		return
	}
	defer rows.Close()

	const maxTasks = 60
	var total, done, active, pending, blocked int
	list := make([]map[string]any, 0)
	for rows.Next() {
		var id, title, status, origin string
		if err := rows.Scan(&id, &title, &status, &origin); err != nil {
			continue
		}
		total++
		switch status {
		case "done":
			done++
		case "active":
			active++
		case "blocked":
			blocked++
		default:
			pending++
		}
		if len(list) < maxTasks {
			list = append(list, map[string]any{
				"id": id, "title": title, "status": status, "origin": origin,
			})
		}
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusOK, emptyBuildQueue())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total": total, "done": done, "active": active,
		"pending": pending, "blocked": blocked, "tasks": list,
	})
}

func emptyBuildQueue() map[string]any {
	return map[string]any{
		"total": 0, "done": 0, "active": 0, "pending": 0, "blocked": 0,
		"tasks": []map[string]any{},
	}
}
