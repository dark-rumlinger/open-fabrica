package auth

import (
	"openfabrica/tools"
)

// manage_tenancy is the multi-tenant SaaS surface: provisions of_tenants +
// of_tenant_members, switches auth into tenant-aware signup modes, and
// attaches `tenant_id` columns to user-facing tables and `tenant_column` to
// the matching API/upload/WS/import/export endpoints. The handlers in
// manage_tenancy_handlers.go perform the schema mutations; this file is the
// LLM-facing schema declaration.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_tenancy",
		Category: "auth",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult: 8000,
		OneLiner:  "Manage multi-tenant SaaS - tenants, memberships, invites, per-table tenant scoping.",
		WhenToUse: `Use when the brief calls for workspaces, teams, or separate customer data. The
flow is: enable on the auth endpoint -> create each table's endpoints -> add_tenant_column
for every data table (it scopes only endpoints that already exist; re-run it after adding
more) -> set signup_mode -> create_tenant for seed accounts. invite_member
issues a one-time token; set_tenant_role changes a user's role inside a
tenant.`,
		Actions: []tools.Action{
			&TenancyEnable{},
			&TenancySetSignupMode{},
			&TenancyAddTenantColumn{},
			&TenancyCreateTenant{},
			&TenancyListTenants{},
			&TenancyInviteMember{},
			&TenancySetTenantRole{},
			&TenancyListMembers{},
		},
	})
}

// TenancyEnable provisions of_tenants + of_tenant_members and attaches
// tenancy to an existing auth endpoint.
type TenancyEnable struct {
	AuthEndpointPath string `arg:"auth_endpoint_path,required" desc:"Auth endpoint to attach tenancy to."`
}

func (a *TenancyEnable) Doc() string {
	return "Turn on tenancy for an auth endpoint (keeps any signup mode already chosen; a fresh endpoint starts invite_only). Then scope each user-facing table with add_tenant_column."
}
func (a *TenancyEnable) Example() any { return &TenancyEnable{AuthEndpointPath: "auth"} }
func (a *TenancyEnable) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return tenancyEnable(ctx, a.AuthEndpointPath, "", "")
}

// TenancySetSignupMode chooses how /register handles tenants.
type TenancySetSignupMode struct {
	AuthEndpointPath  string `arg:"auth_endpoint_path,required" desc:"Auth endpoint."`
	SignupMode        string `arg:"signup_mode,required"        desc:"How register handles tenants." enum:"invite_only,signup_creates_tenant,signup_joins_default"`
	DefaultTenantSlug string `arg:"default_tenant_slug"         desc:"signup_joins_default only: new users join the FIRST tenant (lowest id); this slug creates that tenant when none exists yet."`
}

func (a *TenancySetSignupMode) Doc() string { return "Choose how register handles tenants." }
func (a *TenancySetSignupMode) Example() any {
	return &TenancySetSignupMode{AuthEndpointPath: "auth", SignupMode: "signup_creates_tenant"}
}
func (a *TenancySetSignupMode) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return tenancySetSignupMode(ctx, a.AuthEndpointPath, a.SignupMode, a.DefaultTenantSlug)
}

// TenancyAddTenantColumn adds a tenant_id column to one or more tables and
// wires the matching endpoints to enforce tenant isolation.
type TenancyAddTenantColumn struct {
	Tables       []any  `arg:"tables,required" desc:"Table names to add tenant_id INTEGER to." items:"string"`
	TenantColumn string `arg:"tenant_column"   desc:"Column name to use." default:"tenant_id"`
}

func (a *TenancyAddTenantColumn) Doc() string {
	return "Add tenant_id to named tables and wire it onto matching endpoints."
}
func (a *TenancyAddTenantColumn) Example() any {
	return &TenancyAddTenantColumn{Tables: []any{"projects", "documents"}}
}
func (a *TenancyAddTenantColumn) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return tenancyAddTenantColumn(ctx, a.Tables, a.TenantColumn)
}

// TenancyCreateTenant creates one tenant row.
type TenancyCreateTenant struct {
	Slug     string         `arg:"slug"     desc:"URL-safe tenant slug. Auto-generated from name if omitted."`
	Name     string         `arg:"name,required" desc:"Human-readable tenant name."`
	Plan     string         `arg:"plan"     desc:"Plan tag for billing/limits (free, pro, ...)." default:"free"`
	Settings map[string]any `arg:"settings" desc:"Per-tenant JSON config (branding, feature flags)."`
}

func (a *TenancyCreateTenant) Doc() string { return "Create a tenant row." }
func (a *TenancyCreateTenant) Example() any {
	return &TenancyCreateTenant{Name: "Acme Corp", Plan: "pro"}
}
func (a *TenancyCreateTenant) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return tenancyCreateTenant(ctx, a.Slug, a.Name, a.Plan, a.Settings)
}

// TenancyListTenants returns every tenant row.
type TenancyListTenants struct{}

func (a *TenancyListTenants) Doc() string    { return "List every tenant." }
func (a *TenancyListTenants) ReadOnly() bool { return true }
func (a *TenancyListTenants) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return tenancyListTenants(ctx)
}

// TenancyInviteMember issues a one-time invite token for one tenant.
type TenancyInviteMember struct {
	TenantID int    `arg:"tenant_id,required" desc:"Target tenant id."`
	Email    string `arg:"email,required"     desc:"Invitee email."`
	Role     string `arg:"role"               desc:"Per-tenant role." enum:"owner,admin,member,viewer" default:"member"`
}

func (a *TenancyInviteMember) Doc() string { return "Issue an invite token for one tenant." }
func (a *TenancyInviteMember) Example() any {
	return &TenancyInviteMember{TenantID: 1, Email: "ada@example.com", Role: "admin"}
}
func (a *TenancyInviteMember) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return tenancyInviteMember(ctx, a.TenantID, a.Email, a.Role)
}

// TenancySetTenantRole changes a user's role inside a tenant.
type TenancySetTenantRole struct {
	TenantID int    `arg:"tenant_id,required" desc:"Tenant id."`
	UserID   int    `arg:"user_id,required"   desc:"User id."`
	Role     string `arg:"role,required"      desc:"New per-tenant role." enum:"owner,admin,member,viewer"`
}

func (a *TenancySetTenantRole) Doc() string { return "Update a member's role inside a tenant." }
func (a *TenancySetTenantRole) Example() any {
	return &TenancySetTenantRole{TenantID: 1, UserID: 2, Role: "owner"}
}
func (a *TenancySetTenantRole) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return tenancySetTenantRole(ctx, a.TenantID, a.UserID, a.Role)
}

// TenancyListMembers lists members of a tenant.
type TenancyListMembers struct {
	TenantID int `arg:"tenant_id,required" desc:"Tenant id."`
}

func (a *TenancyListMembers) Doc() string    { return "List members of a tenant." }
func (a *TenancyListMembers) ReadOnly() bool { return true }
func (a *TenancyListMembers) Example() any {
	return &TenancyListMembers{TenantID: 1}
}
func (a *TenancyListMembers) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return tenancyListMembers(ctx, a.TenantID)
}
