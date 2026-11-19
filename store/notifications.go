package store

import (
	"database/sql"

	"openfabrica/db"
)

// ListNotifications returns paginated notification rows for userID, newest first.
func ListNotifications(pdb *db.ProjectDB, userID int, unreadOnly bool, limit, offset int) ([]map[string]any, error) {
	where := "user_id = ?"
	if unreadOnly {
		where += " AND is_read = 0"
	}
	rows, err := pdb.Reader().Query(
		`SELECT id, title, message, type, is_read, action_url, created_at, read_at
		 FROM of_notifications WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		userID, limit, offset,
	)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	return out, nil
}

// CountUnreadNotifications returns the unread notification count for userID.
func CountUnreadNotifications(pdb *db.ProjectDB, userID int) int {
	var n int
	_ = pdb.Reader().QueryRow("SELECT COUNT(*) FROM of_notifications WHERE user_id = ? AND is_read = 0", userID).Scan(&n)
	return n
}

// MarkNotificationRead marks a single notification as read and reports whether it existed.
func MarkNotificationRead(pdb *db.ProjectDB, id, userID int) (bool, error) {
	res, err := pdb.ExecWrite(
		"UPDATE of_notifications SET is_read = 1, read_at = COALESCE(read_at, CURRENT_TIMESTAMP) WHERE id = ? AND user_id = ?",
		id, userID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkAllNotificationsRead marks all unread notifications for userID as read.
func MarkAllNotificationsRead(pdb *db.ProjectDB, userID int) (int64, error) {
	res, err := pdb.ExecWrite(
		"UPDATE of_notifications SET is_read = 1, read_at = CURRENT_TIMESTAMP WHERE user_id = ? AND is_read = 0",
		userID,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteNotification removes a single notification owned by userID and reports whether it existed.
func DeleteNotification(pdb *db.ProjectDB, id, userID int) (bool, error) {
	res, err := pdb.ExecWrite("DELETE FROM of_notifications WHERE id = ? AND user_id = ?", id, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
