package models

import (
	"database/sql"
	"strings"
	"time"
)

// AuditEntry represents a single row in the admin_audit table.
type AuditEntry struct {
	ID              int64     `json:"id"`
	UserID          int       `json:"user_id"`
	Username        string    `json:"username"`
	Method          string    `json:"method"`
	Path            string    `json:"path"`
	TargetProjectID int       `json:"target_project_id"`
	Status          int       `json:"status"`
	RequestID       string    `json:"request_id"`
	IP              string    `json:"ip"`
	CreatedAt       time.Time `json:"created_at"`
}

// InsertAdminAudit records a single admin mutation into admin_audit.
func InsertAdminAudit(exec Executor, e AuditEntry) error {
	_, err := exec.Exec(
		`INSERT INTO admin_audit (user_id, username, method, path, target_project_id, status, request_id, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.UserID, e.Username, e.Method, e.Path, e.TargetProjectID, e.Status, e.RequestID, e.IP,
	)
	return err
}

// ListAdminAudit returns paginated admin_audit rows newest-first and the total count.
func ListAdminAudit(db *sql.DB, userFilter, methodFilter string, limit, offset int) ([]AuditEntry, int, error) {
	where := " WHERE 1=1"
	var args []any
	if user := strings.TrimSpace(userFilter); user != "" {
		where += " AND username LIKE ?"
		args = append(args, "%"+user+"%")
	}
	if action := strings.TrimSpace(methodFilter); action != "" {
		where += " AND method = ?"
		args = append(args, strings.ToUpper(action))
	}

	var total int
	if err := db.QueryRow("SELECT COUNT(*) FROM admin_audit"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	queryArgs := append(append([]any{}, args...), limit, offset)
	rows, err := db.Query(
		"SELECT id, user_id, username, method, path, target_project_id, status, request_id, ip, created_at "+
			"FROM admin_audit"+where+" ORDER BY id DESC LIMIT ? OFFSET ?",
		queryArgs...,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var items []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.UserID, &e.Username, &e.Method, &e.Path,
			&e.TargetProjectID, &e.Status, &e.RequestID, &e.IP, &e.CreatedAt); err != nil {
			continue
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []AuditEntry{}
	}
	return items, total, nil
}
