package public

// In-app notifications, read side. manage_notifications and the send_notification
// trigger write of_notifications rows; these routes are how the signed-in user's
// own page reads and clears them. They live under /__of/ so they never shadow a
// page the project builds at /notifications.
//
//	GET    /__of/notifications            list the caller's notifications (?unread=1&limit=&offset=)
//	GET    /__of/notifications/count      {unread: n}
//	POST   /__of/notifications/read-all   mark every one of the caller's read
//	POST   /__of/notifications/{id}/read  mark one read
//	DELETE /__of/notifications/{id}       delete one
//
// Every route is scoped to the user in the bearer token: another user's
// notification id answers 404, never its contents.

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

const (
	notificationsDefaultLimit = 50
	notificationsMaxLimit     = 200
	notificationsRateLimit    = 240 // requests/min per user, poll-friendly
)

// notificationUser authenticates the caller and applies the per-user rate limit.
// It writes the error response and returns ok=false on failure.
func (h *Handler) notificationUser(w http.ResponseWriter, r *http.Request) (projectID, userID int, ok bool) {
	project := getProject(r)
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return 0, 0, false
	}
	uid, _, ok := h.requireJWTClaims(w, r, project.ID)
	if !ok {
		return 0, 0, false
	}
	if uid <= 0 {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return 0, 0, false
	}
	if !checkEndpointRateLimit(fmt.Sprintf("notif:%d:%d", project.ID, uid), notificationsRateLimit) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return 0, 0, false
	}
	return project.ID, uid, true
}

// NotificationsList handles GET /__of/notifications.
func (h *Handler) NotificationsList(w http.ResponseWriter, r *http.Request) {
	projectID, userID, ok := h.notificationUser(w, r)
	if !ok {
		return
	}
	projectDB := h.openProjectDB(w, projectID)
	if projectDB == nil {
		return
	}
	limit, offset := notificationsDefaultLimit, 0
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, notificationsMaxLimit)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = n
	}
	where := "user_id = ?"
	if r.URL.Query().Get("unread") == "1" {
		where += " AND is_read = 0"
	}
	rows, err := projectDB.Reader().Query(
		`SELECT id, title, message, type, is_read, action_url, created_at, read_at
		 FROM of_notifications WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		userID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load notifications")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int
		var title, message, typ, actionURL sql.NullString
		var isRead bool
		var createdAt, readAt sql.NullTime
		if err := rows.Scan(&id, &title, &message, &typ, &isRead, &actionURL, &createdAt, &readAt); err != nil {
			continue
		}
		row := map[string]any{
			"id": id, "title": title.String, "message": message.String, "type": typ.String,
			"is_read": isRead, "action_url": actionURL.String,
		}
		if createdAt.Valid {
			row["created_at"] = createdAt.Time
		}
		if readAt.Valid {
			row["read_at"] = readAt.Time
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not load notifications")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows": out, "count": len(out), "limit": limit, "offset": offset,
		"unread": countUnread(projectDB.Reader(), userID),
	})
}

func countUnread(db *sql.DB, userID int) int {
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM of_notifications WHERE user_id = ? AND is_read = 0", userID).Scan(&n)
	return n
}

// NotificationsCount handles GET /__of/notifications/count.
func (h *Handler) NotificationsCount(w http.ResponseWriter, r *http.Request) {
	projectID, userID, ok := h.notificationUser(w, r)
	if !ok {
		return
	}
	projectDB := h.openProjectDB(w, projectID)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unread": countUnread(projectDB.Reader(), userID)})
}

// NotificationsMarkRead handles POST /__of/notifications/{id}/read.
func (h *Handler) NotificationsMarkRead(w http.ResponseWriter, r *http.Request) {
	projectID, userID, ok := h.notificationUser(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	projectDB := h.openProjectDB(w, projectID)
	if projectDB == nil {
		return
	}
	res, err := projectDB.ExecWrite(
		"UPDATE of_notifications SET is_read = 1, read_at = COALESCE(read_at, CURRENT_TIMESTAMP) WHERE id = ? AND user_id = ?",
		id, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update notification")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "notification not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "unread": countUnread(projectDB.Reader(), userID)})
}

// NotificationsReadAll handles POST /__of/notifications/read-all.
func (h *Handler) NotificationsReadAll(w http.ResponseWriter, r *http.Request) {
	projectID, userID, ok := h.notificationUser(w, r)
	if !ok {
		return
	}
	projectDB := h.openProjectDB(w, projectID)
	if projectDB == nil {
		return
	}
	res, err := projectDB.ExecWrite(
		"UPDATE of_notifications SET is_read = 1, read_at = CURRENT_TIMESTAMP WHERE user_id = ? AND is_read = 0", userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update notifications")
		return
	}
	n, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "marked": n, "unread": 0})
}

// NotificationsDelete handles DELETE /__of/notifications/{id}.
func (h *Handler) NotificationsDelete(w http.ResponseWriter, r *http.Request) {
	projectID, userID, ok := h.notificationUser(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	projectDB := h.openProjectDB(w, projectID)
	if projectDB == nil {
		return
	}
	res, err := projectDB.ExecWrite("DELETE FROM of_notifications WHERE id = ? AND user_id = ?", id, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete notification")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "notification not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "unread": countUnread(projectDB.Reader(), userID)})
}
