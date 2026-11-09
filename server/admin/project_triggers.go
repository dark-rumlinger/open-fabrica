package admin

import (
	"database/sql"
	"net/http"
	"time"
)

// ProjectTriggersHandler is the operator window onto of_triggers, the
// server-side actions that fire when events are published (the inbound half of
// "event hooks"; outbound HTTP webhooks live in ProjectWebhooksHandler). The
// engine authors triggers via the manage_triggers tool; this lets an operator
// see, disable, and remove them without going through chat.
type ProjectTriggersHandler struct {
	deps *Deps
}

// List returns every trigger with its event_type, action type, and config.
func (h *ProjectTriggersHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	rows, err := projectDB.Query(
		`SELECT id, name, event_type, event_filter, trigger_type, trigger_config, timing, is_enabled, created_at
		 FROM of_triggers ORDER BY created_at DESC`)
	if err != nil {
		writeJSON(w, http.StatusOK, []map[string]any{})
		return
	}
	defer rows.Close()
	triggers := []map[string]any{}
	for rows.Next() {
		var id int
		var name, eventType, triggerType, triggerConfig string
		var eventFilter, timing sql.NullString
		var isEnabled bool
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &eventType, &eventFilter, &triggerType, &triggerConfig, &timing, &isEnabled, &createdAt); err != nil {
			continue
		}
		trig := map[string]any{
			"id": id, "name": name, "event_type": eventType,
			"trigger_type": triggerType, "trigger_config": triggerConfig,
			"is_enabled": isEnabled, "created_at": createdAt,
		}
		if eventFilter.Valid && eventFilter.String != "" {
			trig["event_filter"] = eventFilter.String
		}
		if timing.Valid {
			trig["timing"] = timing.String
		}
		triggers = append(triggers, trig)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusOK, []map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, triggers)
}

// Toggle flips a trigger's enabled flag.
func (h *ProjectTriggersHandler) Toggle(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	triggerID, ok := requireIntParam(w, r, "triggerID", "invalid trigger ID")
	if !ok {
		return
	}
	if _, err := projectDB.ExecWrite("UPDATE of_triggers SET is_enabled = NOT is_enabled WHERE id = ?", triggerID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to toggle trigger")
		return
	}
	var isEnabled bool
	if err := projectDB.QueryRow("SELECT is_enabled FROM of_triggers WHERE id = ?", triggerID).Scan(&isEnabled); err != nil {
		writeError(w, http.StatusNotFound, "trigger not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": triggerID, "is_enabled": isEnabled})
}

// Delete removes a trigger.
func (h *ProjectTriggersHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	triggerID, ok := requireIntParam(w, r, "triggerID", "invalid trigger ID")
	if !ok {
		return
	}
	res, err := projectDB.ExecWrite("DELETE FROM of_triggers WHERE id = ?", triggerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete trigger")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "trigger not found")
		return
	}
	writeStatus(w, "deleted")
}
