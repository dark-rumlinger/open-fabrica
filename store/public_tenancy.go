package store

import (
	"database/sql"
	"fmt"

	"openfabrica/db"
)

// PublicTenantMembership represents an active tenant membership for a user.
type PublicTenantMembership struct {
	TenantID int
	Slug     string
	Name     string
	Role     string
	Status   string
}

// LoadActiveTenantMemberships returns the user's active tenants ordered by joined_at.
func LoadActiveTenantMemberships(reader *sql.DB, userID int) ([]PublicTenantMembership, error) {
	rows, err := reader.Query(
		`SELECT t.id, t.slug, t.name, m.role, m.status
		 FROM of_tenant_members m
		 JOIN of_tenants t ON t.id = m.tenant_id AND t.is_deleted = 0
		 WHERE m.user_id = ? AND m.status = 'active'
		 ORDER BY m.joined_at ASC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PublicTenantMembership
	for rows.Next() {
		var m PublicTenantMembership
		if err := rows.Scan(&m.TenantID, &m.Slug, &m.Name, &m.Role, &m.Status); err == nil {
			out = append(out, m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// TenantInviteUsable reports whether an unexpired invite token exists for email (or open invite).
func TenantInviteUsable(reader *sql.DB, token, email string) bool {
	if token == "" {
		return false
	}
	var n int
	err := reader.QueryRow(
		`SELECT COUNT(*) FROM of_tenant_invites
		 WHERE token = ? AND (email = ? OR email = '')
		   AND (expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP)`,
		token, email,
	).Scan(&n)
	return err == nil && n > 0
}

// ConsumeTenantInviteToken atomically validates an invite token and deletes it.
func ConsumeTenantInviteToken(writeDB *sql.DB, token, email string) (tenantID int, role string, ok bool) {
	err := db.RunInTx(writeDB, func(tx *sql.Tx) error {
		if err := tx.QueryRow(
			`SELECT tenant_id, role FROM of_tenant_invites
			 WHERE token = ? AND (email = ? OR email = '')
			   AND (expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP)`,
			token, email,
		).Scan(&tenantID, &role); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM of_tenant_invites WHERE token = ?", token)
		return err
	})
	if err != nil {
		return 0, "", false
	}
	if role == "" {
		role = "member"
	}
	return tenantID, role, true
}

// InsertTenantMember adds an active membership row for userID in tenantID.
func InsertTenantMember(writeDB *sql.DB, tenantID, userID int, role string) error {
	_, err := writeDB.Exec(
		"INSERT INTO of_tenant_members (tenant_id, user_id, role, status) VALUES (?, ?, ?, 'active')",
		tenantID, userID, role,
	)
	return err
}

// TenantSlugExists reports whether a tenant with slug already exists.
func TenantSlugExists(db *sql.DB, slug string) bool {
	var exists int
	_ = db.QueryRow("SELECT COUNT(*) FROM of_tenants WHERE slug = ?", slug).Scan(&exists)
	return exists > 0
}

// CreateTenantWithOwner creates a new free-plan tenant with a unique slug derived
// from baseSlug and adds userID as its owner.
func CreateTenantWithOwner(writeDB *sql.DB, baseSlug string, userID int) (tenantID int, slug string, err error) {
	slug = baseSlug
	for i := 2; i < 100; i++ {
		if !TenantSlugExists(writeDB, slug) {
			break
		}
		slug = fmt.Sprintf("%s-%d", baseSlug, i)
	}
	res, err := writeDB.Exec(
		"INSERT INTO of_tenants (slug, name, plan, settings) VALUES (?, ?, 'free', '{}')",
		slug, slug,
	)
	if err != nil {
		return 0, "", err
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, "", err
	}
	if err := InsertTenantMember(writeDB, int(newID), userID, "owner"); err != nil {
		return 0, "", err
	}
	return int(newID), slug, nil
}

// DefaultTenantID returns the lowest-id active tenant.
func DefaultTenantID(db *sql.DB) (int, error) {
	var defID int
	err := db.QueryRow("SELECT id FROM of_tenants WHERE is_deleted = 0 ORDER BY id ASC LIMIT 1").Scan(&defID)
	return defID, err
}
