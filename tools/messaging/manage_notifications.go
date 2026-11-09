package messaging

import (
	"fmt"

	"openfabrica/events"
	"openfabrica/store"
	"openfabrica/tools"
)

// manage_notifications delivers in-app notifications to end users. For
// owner-facing messaging use manage_chat instead. The broadcast guard on
// send is intentional: a mass notify mistake is hard to recall.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_notifications",
		Category: "messaging",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult: 8000,
		OneLiner:  "Send in-app notifications (toast or inbox) to end-users.",
		WhenToUse: `Notify a specific user, every user in a role, or (with broadcast=true)
every user in the project. The broadcast guard is intentional - mass-notify
mistakes are hard to recall. To auto-send on events, create a trigger with
trigger_type='send_notification' instead.

These are in-app messages in the app's own UI (toast/inbox) - not OS-level browser
push (manage_push) or email (manage_email).
list/mark_read/count_unread/delete act on stored rows from the builder side. The
signed-in user's own page reads its inbox at runtime from platform routes (send the
auth token, e.g. through the auth helper's apiFetch): GET /__of/notifications
(?unread=1) -> {rows, count, unread}; GET /__of/notifications/count -> {unread}
(poll it for a badge); POST /__of/notifications/{id}/read; POST
/__of/notifications/read-all; DELETE /__of/notifications/{id}. A user only ever
sees rows addressed to their own user_id.`,
		Actions: []tools.Action{
			&NotifSend{},
			&NotifList{},
			&NotifMarkRead{},
			&NotifCountUnread{},
			&NotifDelete{},
		},
		Hints: func(action string, _ map[string]any, r *tools.Result) string {
			if r == nil || !r.Success {
				return ""
			}
			if action == "send" {
				return "Notification sent. To auto-send on events, create a trigger with trigger_type='send_notification'. The page shows it by reading GET /__of/notifications (see the tool notes)."
			}
			return ""
		},
	})
}

// NotifSend delivers a notification to one user, every user in a role, or
// (with broadcast=true) every user in the project.
type NotifSend struct {
	UserID    int    `arg:"user_id"   desc:"Target user id."`
	Role      string `arg:"role"      desc:"Deliver to all users with this role."`
	Broadcast bool   `arg:"broadcast" desc:"Explicit opt-in to deliver to every user."`
	Title     string `arg:"title,required" desc:"Notification title."`
	Message   string `arg:"message"   desc:"Notification body text."`
	Type      string `arg:"type"      desc:"Visual style." enum:"info,warning,success,error" default:"info"`
	ActionURL string `arg:"action_url" desc:"URL to navigate to when clicked."`
}

func (a *NotifSend) ActionName() string { return "send" }
func (a *NotifSend) Doc() string {
	return "Deliver a notification to a user, a role, or (explicitly) everyone."
}
func (a *NotifSend) Example() any {
	return &NotifSend{UserID: 1, Title: "Welcome", Message: "Your account is ready."}
}
func (a *NotifSend) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var userIDs []int
	switch {
	case a.UserID > 0:
		userIDs = append(userIDs, a.UserID)
	case a.Role != "":
		userIDs = notifFindUsersByRole(ctx, a.Role)
		if len(userIDs) == 0 {
			return tools.Fail("no users found with role '%s'", a.Role), nil
		}
	case a.Broadcast:
		userIDs = notifFindAllUsers(ctx)
		if len(userIDs) == 0 {
			// A row for user 0 is never shown to anyone (the inbox reads the signed-in
			// user's rows), so reporting it as sent would be false.
			return tools.Fail("no user accounts exist yet, so a broadcast reaches nobody; notifications go to existing accounts only"), nil
		}
	default:
		return tools.Fail("send requires user_id, role, or broadcast=true (explicit opt-in to notify ALL users)"), nil
	}

	count := 0
	for _, uid := range userIDs {
		if _, err := ctx.DB.Exec(
			"INSERT INTO of_notifications (user_id, title, message, type, action_url) VALUES (?, ?, ?, ?, ?)",
			uid, a.Title, a.Message, a.Type, a.ActionURL,
		); err == nil {
			count++
		}
	}
	if ctx.Bus != nil {
		ctx.Bus.Publish(events.NewEvent(events.EventNotificationCreated, ctx.ProjectID, map[string]any{
			"title": a.Title, "type": a.Type, "count": count,
		}))
	}
	return tools.Ok(map[string]any{"sent": count, "title": a.Title}), nil
}

// NotifList returns the most recent notifications for one user, optionally
// filtered to unread only.
type NotifList struct {
	UserID     int  `arg:"user_id,required" desc:"User whose inbox to read."`
	UnreadOnly bool `arg:"unread_only"      desc:"Only return unread notifications."`
	Limit      int  `arg:"limit"            desc:"Max rows (maximum 200)." default:"50" max:"200"`
}

func (a *NotifList) ActionName() string { return "list" }
func (a *NotifList) Doc() string        { return "List notifications for a user." }
func (a *NotifList) ReadOnly() bool     { return true }
func (a *NotifList) Example() any       { return &NotifList{UserID: 1} }
func (a *NotifList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	limit := a.Limit
	query := "SELECT id, title, message, type, is_read, action_url, created_at, read_at FROM of_notifications WHERE user_id = ?"
	qArgs := []any{a.UserID}
	if a.UnreadOnly {
		query += " AND is_read = 0"
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	qArgs = append(qArgs, limit)

	rows, err := ctx.DB.Query(query, qArgs...)
	if err != nil {
		return nil, fmt.Errorf("listing notifications: %w", err)
	}
	defer rows.Close()
	var notifs []map[string]any
	for rows.Next() {
		var id int
		var title, message, notifType string
		var isRead bool
		var actionURL, createdAt string
		var readAt *string
		if err := rows.Scan(&id, &title, &message, &notifType, &isRead, &actionURL, &createdAt, &readAt); err != nil {
			continue
		}
		n := map[string]any{
			"id": id, "title": title, "message": message,
			"type": notifType, "is_read": isRead,
			"action_url": actionURL, "created_at": createdAt,
		}
		if readAt != nil {
			n["read_at"] = *readAt
		}
		notifs = append(notifs, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing notifications: %w", err)
	}
	return tools.Ok(notifs), nil
}

// NotifMarkRead marks one notification read by id, or every unread for a
// user when only user_id is provided.
type NotifMarkRead struct {
	ID     int `arg:"id"      desc:"Single notification id to mark read."`
	UserID int `arg:"user_id" desc:"Mark all unread for this user."`
}

func (a *NotifMarkRead) ActionName() string { return "mark_read" }
func (a *NotifMarkRead) Doc() string        { return "Mark one notification (or all of a user's) as read." }
func (a *NotifMarkRead) Example() any       { return &NotifMarkRead{ID: 42} }
func (a *NotifMarkRead) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if a.ID > 0 {
		res, err := ctx.DB.Exec("UPDATE of_notifications SET is_read = 1, read_at = CURRENT_TIMESTAMP WHERE id = ?", a.ID)
		if err != nil {
			return nil, fmt.Errorf("marking notification read: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return tools.NotFound("notification id", fmt.Sprint(a.ID)), nil
		}
		return tools.Ok(map[string]any{"marked_read": a.ID}), nil
	}
	if a.UserID > 0 {
		result, err := ctx.DB.Exec("UPDATE of_notifications SET is_read = 1, read_at = CURRENT_TIMESTAMP WHERE user_id = ? AND is_read = 0", a.UserID)
		if err != nil {
			return nil, fmt.Errorf("marking notifications read: %w", err)
		}
		affected, _ := result.RowsAffected()
		return tools.Ok(map[string]any{"marked_read": affected}), nil
	}
	return tools.Fail("provide id (single) or user_id (all) to mark as read"), nil
}

// NotifCountUnread returns the unread count for one user.
type NotifCountUnread struct {
	UserID int `arg:"user_id,required" desc:"User id."`
}

func (a *NotifCountUnread) ActionName() string { return "count_unread" }
func (a *NotifCountUnread) Doc() string        { return "Count unread notifications for a user." }
func (a *NotifCountUnread) ReadOnly() bool     { return true }
func (a *NotifCountUnread) Example() any       { return &NotifCountUnread{UserID: 1} }
func (a *NotifCountUnread) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var count int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_notifications WHERE user_id = ? AND is_read = 0", a.UserID).Scan(&count); err != nil {
		return nil, fmt.Errorf("counting unread notifications: %w", err)
	}
	return tools.Ok(map[string]any{"user_id": a.UserID, "unread": count}), nil
}

// NotifDelete removes a notification row.
type NotifDelete struct {
	ID int `arg:"id,required" desc:"Notification id."`
}

func (a *NotifDelete) ActionName() string { return "delete" }
func (a *NotifDelete) Doc() string        { return "Delete one notification." }
func (a *NotifDelete) Example() any       { return &NotifDelete{ID: 42} }
func (a *NotifDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	res, err := ctx.DB.Exec("DELETE FROM of_notifications WHERE id = ?", a.ID)
	if err != nil {
		return nil, fmt.Errorf("deleting notification: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tools.NotFound("notification id", fmt.Sprint(a.ID)), nil
	}
	return tools.Ok(map[string]any{"deleted": a.ID}), nil
}

// notifAuthEndpoint is one registered auth endpoint's user table and role column.
type notifAuthEndpoint struct {
	table      string
	roleColumn string
}

// notifAuthEndpoints lists EVERY registered auth endpoint, fully materialized
// before any per-table user query runs. A project can register more than one auth
// endpoint (e.g. a separate admin table); role and broadcast sends must reach users
// in all of them, not just the first - the advertised "every user in the project".
// The cursor is drained and closed here (not held open) because ctx.DB has
// MaxOpenConns=1 and notifQueryUserIDs issues its own query on the same handle,
// which would deadlock against a still-open cursor.
func notifAuthEndpoints(ctx *tools.ToolContext) []notifAuthEndpoint {
	rows, err := ctx.DB.Query("SELECT table_name, COALESCE(role_column,'') FROM of_auth_endpoints")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []notifAuthEndpoint
	for rows.Next() {
		var ep notifAuthEndpoint
		if rows.Scan(&ep.table, &ep.roleColumn) == nil {
			out = append(out, ep)
		}
	}
	_ = rows.Err() // best-effort resolution; mirrors the silent query-error path below
	return out
}

// notifFindUsersByRole resolves a role string to the user IDs that hold it across
// every auth endpoint's table (union, deduped).
func notifFindUsersByRole(ctx *tools.ToolContext, role string) []int {
	var ids []int
	seen := map[int]bool{}
	for _, ep := range notifAuthEndpoints(ctx) {
		if !store.ValidTableName.MatchString(ep.table) || !store.ValidColumnName.MatchString(ep.roleColumn) {
			continue
		}
		for _, id := range notifQueryUserIDs(ctx, fmt.Sprintf("SELECT id FROM %s WHERE %s = ?", ep.table, ep.roleColumn), role) {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// notifFindAllUsers returns every user ID across every auth endpoint's table
// (union, deduped).
func notifFindAllUsers(ctx *tools.ToolContext) []int {
	var ids []int
	seen := map[int]bool{}
	for _, ep := range notifAuthEndpoints(ctx) {
		if !store.ValidTableName.MatchString(ep.table) {
			continue
		}
		for _, id := range notifQueryUserIDs(ctx, fmt.Sprintf("SELECT id FROM %s", ep.table)) {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func notifQueryUserIDs(ctx *tools.ToolContext, query string, queryArgs ...any) []int {
	rows, err := ctx.DB.Query(query, queryArgs...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Err() // best-effort resolution; mirrors the silent query-error path above
	return ids
}
