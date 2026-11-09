package messaging

import (
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools/tooltest"
)

// A project can register more than one auth endpoint (e.g. a separate admin user
// table). Role and broadcast notification sends must reach users in EVERY auth
// table, not just the first one (the old LIMIT 1 silently missed the rest).
func TestManageNotifications_BroadcastSpansAllAuthEndpoints(t *testing.T) {
	h := tooltest.New(t)
	db := h.DB()

	for _, ddl := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, role TEXT)`,
		`CREATE TABLE admins (id INTEGER PRIMARY KEY, email TEXT, role TEXT)`,
		`INSERT INTO users (id, email, role) VALUES (1,'a@x','member'),(2,'b@x','member')`,
		`INSERT INTO admins (id, email, role) VALUES (5,'c@x','admin')`,
		`INSERT INTO of_auth_endpoints (table_name, path, role_column) VALUES ('users','auth','role')`,
		`INSERT INTO of_auth_endpoints (table_name, path, role_column) VALUES ('admins','admin','role')`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("seed %q: %v", ddl, err)
		}
	}

	// Broadcast reaches all three accounts across both tables.
	h.MustOK("manage_notifications", map[string]any{
		"action": "send", "broadcast": true, "title": "Hi", "type": "info",
	})
	if got := notifUserIDs(t, db); !sameSet(got, []int{1, 2, 5}) {
		t.Errorf("broadcast reached user_ids %v, want {1,2,5}", got)
	}

	// Role targeting also spans tables: role 'admin' lives only in the admins table.
	if _, err := db.Exec(`DELETE FROM of_notifications`); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	h.MustOK("manage_notifications", map[string]any{
		"action": "send", "role": "admin", "title": "Ping", "type": "info",
	})
	if got := notifUserIDs(t, db); !sameSet(got, []int{5}) {
		t.Errorf("role=admin reached user_ids %v, want {5}", got)
	}
}

func notifUserIDs(t *testing.T, db dbh.Handle) []int {
	t.Helper()
	rows, err := db.Query(`SELECT DISTINCT user_id FROM of_notifications ORDER BY user_id`)
	if err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return ids
}

func sameSet(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[int]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}
