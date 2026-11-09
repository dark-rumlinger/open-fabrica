package auth

import (
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/security"
	"openfabrica/store"
	"openfabrica/tools"
)

// manage_roles implements role-based access control: define roles with
// wildcard permissions, assign them to users, check access. Endpoints set
// `required_role` to gate access; the JWT carries the role so checks at
// request time are O(1) - no DB lookup.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_roles",
		Category: "auth",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult: 8000,
		OneLiner:  "Manage RBAC - define roles with wildcard permissions, assign to users, check access.",
		WhenToUse: `Use when the plan distinguishes user types (admin, editor, viewer, member).
Requires an existing auth endpoint - create it with manage_auth first; the role
rides in the JWT that auth issues. Define each role's permissions with dot
notation ('content.read') and wildcards ('content.*' or '*'); check_access
answers whether a role's permissions cover one. Permissions are a catalogue for
that check: a request is gated by role NAME (an endpoint's required_role, matched
exactly), never by permission string, so give each tier its own role.
admin is built in by name (passes every required_role gate, bypasses owner-column
RLS, assignable without create_role). Create any other role first; a tier that must
not be all-powerful needs its own name.`,
		Actions: []tools.Action{
			&RoleCreate{},
			&RoleList{},
			&RoleUpdate{},
			&RoleDelete{},
			&RoleAssign{},
			&RoleCheckAccess{},
		},
		Hints: func(action string, _ map[string]any, r *tools.Result) string {
			if r == nil || !r.Success {
				return ""
			}
			switch action {
			case "create_role":
				return "Role created. Use assign_role to assign it to users, and gate an API endpoint on it with manage_api auth_model=role required_role=<name> (admin passes every role gate)."
			case "assign_role":
				return "Role assigned. The user's JWT will contain this role on next login."
			}
			return ""
		},
	})
}

// builtinAdminRole is the role name the platform's gates treat as all-powerful
// (required_role checks and owner-column RLS bypass); it needs no of_roles row.
const builtinAdminRole = "admin"

// RoleCreate defines a role with its permission bundle.
type RoleCreate struct {
	Name        string `arg:"name,required" desc:"Role name (alphanumeric + underscores)."`
	Description string `arg:"description"   desc:"Human-readable role description."`
	Permissions []any  `arg:"permissions"   desc:"Permission strings: \"content.read\", \"users.*\", \"*\"." items:"string"`
}

func (a *RoleCreate) ActionName() string { return "create_role" }
func (a *RoleCreate) Doc() string        { return "Define a role with its permission bundle." }
func (a *RoleCreate) Example() any {
	return &RoleCreate{Name: "editor", Permissions: []any{"content.*", "media.read"}}
}
func (a *RoleCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	name := strings.ToLower(strings.TrimSpace(a.Name))
	if !store.ValidTableName.MatchString(name) {
		return tools.Fail("role name must be alphanumeric with underscores"), nil
	}
	perms := "[]"
	if len(a.Permissions) > 0 {
		b, _ := json.Marshal(a.Permissions)
		perms = string(b)
	}
	if _, err := ctx.DB.Exec(
		"INSERT INTO of_roles (name, description, permissions) VALUES (?, ?, ?)",
		name, a.Description, perms,
	); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return tools.Fail("role '%s' already exists", name), nil
		}
		return nil, fmt.Errorf("creating role: %w", err)
	}
	permList := a.Permissions
	if permList == nil {
		permList = []any{}
	}
	return tools.Ok(map[string]any{
		"name": name, "description": a.Description, "permissions": permList,
	}), nil
}

// RoleList returns every defined role with its permissions.
type RoleList struct{}

func (a *RoleList) ActionName() string { return "list_roles" }
func (a *RoleList) Doc() string        { return "List every defined role with its permissions." }
func (a *RoleList) ReadOnly() bool     { return true }
func (a *RoleList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query("SELECT name, description, permissions, is_system FROM of_roles ORDER BY is_system DESC, name")
	if err != nil {
		return nil, fmt.Errorf("listing roles: %w", err)
	}
	defer rows.Close()
	var roles []map[string]any
	for rows.Next() {
		var name, desc, perms string
		var isSystem bool
		if err := rows.Scan(&name, &desc, &perms, &isSystem); err != nil {
			continue
		}
		var permList []any
		_ = json.Unmarshal([]byte(perms), &permList)
		roles = append(roles, map[string]any{
			"name": name, "description": desc,
			"permissions": permList, "is_system": isSystem,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing roles: %w", err)
	}
	return tools.Ok(roles), nil
}

// RoleUpdate patches a role's description or permissions. System roles can
// have their description updated but not their permission bundle.
type RoleUpdate struct {
	Name        string `arg:"name,required" desc:"Role name (key for lookup)."`
	Description string `arg:"description"   desc:"New description."`
	Permissions []any  `arg:"permissions"   desc:"New permission bundle (rejected on system roles)." items:"string"`
}

func (a *RoleUpdate) ActionName() string { return "update_role" }
func (a *RoleUpdate) Doc() string        { return "Replace a role's description or permissions." }
func (a *RoleUpdate) Example() any {
	return &RoleUpdate{Name: "editor", Permissions: []any{"content.*"}}
}
func (a *RoleUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	a.Name = strings.ToLower(strings.TrimSpace(a.Name)) // create_role stores names lowercased
	var isSystem bool
	if err := ctx.DB.QueryRow("SELECT is_system FROM of_roles WHERE name = ?", a.Name).Scan(&isSystem); err != nil {
		return tools.Fail("role '%s' not found", a.Name), nil
	}
	sets := []string{}
	vals := []any{}
	if a.Description != "" {
		sets = append(sets, "description = ?")
		vals = append(vals, a.Description)
	}
	if len(a.Permissions) > 0 {
		if isSystem {
			return tools.Fail("cannot modify permissions of system roles"), nil
		}
		b, _ := json.Marshal(a.Permissions)
		sets = append(sets, "permissions = ?")
		vals = append(vals, string(b))
	}
	if len(sets) == 0 {
		return tools.Fail("nothing to update, provide description or permissions"), nil
	}
	vals = append(vals, a.Name)
	if _, err := ctx.DB.Exec(
		fmt.Sprintf("UPDATE of_roles SET %s WHERE name = ?", strings.Join(sets, ", ")),
		vals...,
	); err != nil {
		return nil, fmt.Errorf("updating role: %w", err)
	}
	return tools.Ok(map[string]any{"name": a.Name, "updated": sets}), nil
}

// RoleDelete removes a non-system role.
type RoleDelete struct {
	Name string `arg:"name,required" desc:"Role name to delete."`
}

func (a *RoleDelete) ActionName() string { return "delete_role" }
func (a *RoleDelete) Doc() string {
	return "Delete a role (a system-flagged role cannot be deleted)."
}
func (a *RoleDelete) Example() any { return &RoleDelete{Name: "editor"} }
func (a *RoleDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	a.Name = strings.ToLower(strings.TrimSpace(a.Name)) // create_role stores names lowercased
	var isSystem bool
	if err := ctx.DB.QueryRow("SELECT is_system FROM of_roles WHERE name = ?", a.Name).Scan(&isSystem); err != nil {
		return tools.Fail("role '%s' not found", a.Name), nil
	}
	if isSystem {
		return tools.Fail("cannot delete system role '%s'", a.Name), nil
	}
	if _, err := ctx.DB.Exec("DELETE FROM of_roles WHERE name = ?", a.Name); err != nil {
		return nil, fmt.Errorf("deleting role %q: %w", a.Name, err)
	}
	return tools.Ok(map[string]any{"deleted": a.Name}), nil
}

// RoleAssign updates a user row's role_column to a defined role.
type RoleAssign struct {
	Table      string `arg:"table,required"   desc:"User table name."`
	Role       string `arg:"role,required"    desc:"Role to assign."`
	UserID     int    `arg:"user_id,required" desc:"User id."`
	RoleColumn string `arg:"role_column"      desc:"User table's role column (default: the auth endpoint's)." default:"role"`
}

func (a *RoleAssign) ActionName() string { return "assign_role" }
func (a *RoleAssign) Doc() string        { return "Set a user's role on the user table." }
func (a *RoleAssign) Example() any {
	return &RoleAssign{Table: "users", UserID: 1, Role: "admin"}
}
func (a *RoleAssign) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	a.Role = strings.ToLower(strings.TrimSpace(a.Role)) // create_role stores names lowercased
	var exists int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_roles WHERE name = ?", a.Role).Scan(&exists)
	// admin is built in by name (the gates and RLS bypass key on it), so it needs no row.
	if exists == 0 && a.Role != builtinAdminRole {
		return tools.Fail("role '%s' does not exist, create it first (only admin is built in)", a.Role), nil
	}
	if strings.HasPrefix(strings.ToLower(a.Table), "of_") {
		return tools.FailCode(tools.CodeBadArg, "%s is a platform table; assign_role writes the role column of your own user table (the one manage_auth uses)", a.Table), nil
	}
	// The default column name is only a default: the auth endpoint over this table
	// names the real one (manage_auth role_column), and writing a different column
	// would leave the user's role unchanged.
	if a.RoleColumn == "" || a.RoleColumn == "role" {
		var configured string
		if err := ctx.DB.QueryRow("SELECT COALESCE(role_column,'') FROM of_auth_endpoints WHERE table_name = ? LIMIT 1", a.Table).Scan(&configured); err == nil && configured != "" {
			a.RoleColumn = configured
		}
	}
	if !store.ValidTableName.MatchString(a.Table) || !store.ValidColumnName.MatchString(a.RoleColumn) {
		return tools.Fail("invalid table or column name"), nil
	}
	result, err := ctx.DB.Exec(
		fmt.Sprintf("UPDATE %s SET %s = ? WHERE id = ?", security.QuoteIdent(a.Table), security.QuoteIdent(a.RoleColumn)),
		a.Role, a.UserID,
	)
	if err != nil {
		return tools.Fail("failed to assign role: %s", err.Error()), nil
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return tools.Fail("user %d not found in table %s", a.UserID, a.Table), nil
	}
	return tools.Ok(map[string]any{
		"user_id": a.UserID, "role": a.Role, "table": a.Table,
	}), nil
}

// RoleCheckAccess returns whether a given role's permissions match a
// required permission string (with wildcard expansion).
type RoleCheckAccess struct {
	Role       string `arg:"role,required"       desc:"Role to check."`
	Permission string `arg:"permission,required" desc:"Permission to test against the role."`
}

func (a *RoleCheckAccess) Doc() string    { return "Read-only wildcard permission check for one role." }
func (a *RoleCheckAccess) ReadOnly() bool { return true }
func (a *RoleCheckAccess) Example() any {
	return &RoleCheckAccess{Role: "editor", Permission: "content.update"}
}
func (a *RoleCheckAccess) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	a.Role = strings.ToLower(strings.TrimSpace(a.Role)) // create_role stores names lowercased
	var permsJSON string
	if err := ctx.DB.QueryRow("SELECT permissions FROM of_roles WHERE name = ?", a.Role).Scan(&permsJSON); err != nil {
		return tools.Fail("role '%s' not found", a.Role), nil
	}
	var perms []string
	_ = json.Unmarshal([]byte(permsJSON), &perms)
	return tools.Ok(map[string]any{
		"role": a.Role, "permission": a.Permission,
		"granted": PermissionMatches(perms, a.Permission),
	}), nil
}

// PermissionMatches checks whether the granted-permissions list satisfies
// the required permission. Supports "*" for everything and "prefix.*" for
// prefix wildcards. Exported because other tools (api endpoint guards) may
// want to perform the same check without re-parsing.
func PermissionMatches(granted []string, required string) bool {
	for _, p := range granted {
		if p == "*" || p == required {
			return true
		}
		if strings.HasSuffix(p, ".*") {
			prefix := strings.TrimSuffix(p, ".*")
			if strings.HasPrefix(required, prefix+".") {
				return true
			}
		}
	}
	return false
}
