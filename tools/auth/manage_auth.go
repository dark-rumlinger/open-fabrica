package auth

import (
	"fmt"

	"openfabrica/tools"
	"openfabrica/tools/api"
)

// manage_auth is the JWT-auth endpoint surface: scaffolds /register, /login,
// /me, /refresh on top of an existing user table. Password verification and
// the endpoint CRUD handlers live in password.go; this file is the schema
// declaration whose Action structs delegate straight to those typed handlers.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_auth",
		Category:           "auth",
		FullResultInEvents: true,
		OneLiner:           "Define JWT auth endpoints (register/login/me/refresh) over an existing user table.",
		WhenToUse: `Use create once per auth surface (typically endpoint_path='auth') after the
user table exists. This tool implements the plan's create_auth capability. JWT is
opaque - never decode client-side. The platform auto-emits js/auth.js; build the browser
side from the auth law on your prompt, which names its helpers - do not write your own
auth bootstrap.`,
		Actions: []tools.Action{
			&AuthCreate{},
			&AuthUpdate{},
			&AuthList{},
			&AuthDelete{},
			&AuthVerifyPassword{},
		},
		Hints: func(action string, args map[string]any, r *tools.Result) string {
			if r == nil || !r.Success {
				return ""
			}
			if m, ok := r.Data.(map[string]any); ok && m["already_existed"] == true {
				return ""
			}
			rawPath, _ := args["endpoint_path"].(string)
			path := tools.NormalizeEndpointPath(rawPath)
			if path == "" {
				return ""
			}
			if action != "create" {
				return ""
			}
			usernameCol, _ := args["username_column"].(string)
			// Only the per-endpoint facts: the paths and this table's actual column names.
			// The response shape, the token key, and the browser helper module are the auth
			// law, already transcluded into every prompt.
			return fmt.Sprintf(`Auth /api/%s ready. POST /register and /login take {%q:val, "password":val} (the wire field is always "password", whatever password_column is named); /register auto-logs the user in. GET /me and POST /refresh live under the same path. Build the browser side from the auth law already on your prompt rather than restating it; the `+"`auth_contract`"+` guide covers the step-up flows.`, path, usernameCol)
		},
		MaxResult: 8000,
		Summarize: api.EpSummarize,
	})
}

// AuthCreate scaffolds /register, /login, /me, /refresh over a user table.
type AuthCreate struct {
	EndpointPath   string   `arg:"endpoint_path,required"  desc:"Auth endpoint path suffix, no /api/ prefix, no leading slash."`
	TableName      string   `arg:"table_name,required"     desc:"Existing user table."`
	PublicColumns  []any    `arg:"public_columns"          desc:"Column NAMES surfaced by /me; REPLACES the default set (id, username, role), so list id and the username column too plus any extras (display_name, avatar_url). PASSWORD is always hidden; the role column is always added so requireRole works." items:"string"`
	SignupColumns  []string `arg:"signup_columns"        desc:"Extra columns /register may set from the request body (e.g. display_name). When set, every other body field is dropped. Default: any column except privilege-like ones (is_admin, verified, credits, plan, ...)."`
	UsernameColumn string   `arg:"username_column,required" desc:"Unique login column (e.g. 'email')."`
	PasswordColumn string   `arg:"password_column"         desc:"PASSWORD storage column. Wire field stays 'password'." default:"password"`
	RoleColumn     string   `arg:"role_column"             desc:"Role column." default:"role"`
	DefaultRole    string   `arg:"default_role"            desc:"Role assigned on register." default:"user"`
	RateLimit      int      `arg:"rate_limit"              desc:"Req/min per IP." default:"60"`
	JWTExpiryHours int      `arg:"jwt_expiry_hours"        desc:"How long an issued token stays valid, in hours (default 24). Raise it for a stay-signed-in app (720 = 30 days), lower it for a sensitive one. Sessions outlive this only if the client calls /refresh." default:"24"`
}

func (a *AuthCreate) Doc() string {
	return "Scaffold /api/<path>/{register,login,me,refresh} over a user table."
}
func (a *AuthCreate) Example() any {
	return &AuthCreate{
		EndpointPath:   "auth",
		TableName:      "users",
		UsernameColumn: "email",
		PublicColumns:  []any{"id", "email", "role"},
	}
}
func (a *AuthCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epCreateAuth(ctx, a)
}

// AuthUpdate patches an existing auth endpoint's columns or rate limit.
type AuthUpdate struct {
	EndpointPath   string   `arg:"endpoint_path,required" desc:"Auth endpoint path."`
	PublicColumns  []any    `arg:"public_columns"         desc:"Replacement /me column set." items:"string"`
	SignupColumns  []string `arg:"signup_columns"       desc:"Replace the columns /register may set. Pass [\"*\"] to restore the default filter."`
	UsernameColumn string   `arg:"username_column"        desc:"New username column."`
	PasswordColumn string   `arg:"password_column"        desc:"New password column."`
	RoleColumn     string   `arg:"role_column"            desc:"New role column."`
	DefaultRole    string   `arg:"default_role"           desc:"New default role for register."`
	RateLimit      int      `arg:"rate_limit"             desc:"New rate limit (req/min per IP)."`
	JWTExpiryHours int      `arg:"jwt_expiry_hours"       desc:"New token lifetime in hours; applies to tokens issued from now on."`
}

func (a *AuthUpdate) Doc() string { return "Update an existing auth endpoint's columns or rate limit." }
func (a *AuthUpdate) Example() any {
	return &AuthUpdate{EndpointPath: "auth", RateLimit: 30}
}
func (a *AuthUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epUpdateAuth(ctx, a)
}

// AuthList enumerates registered auth endpoints.
type AuthList struct{}

func (a *AuthList) Doc() string    { return "List every auth endpoint in the project." }
func (a *AuthList) ReadOnly() bool { return true }
func (a *AuthList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epListAuth(ctx)
}

// AuthDelete removes an auth endpoint and its routes.
type AuthDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Auth endpoint path to delete."`
}

func (a *AuthDelete) Doc() string  { return "Delete an auth endpoint and its routes." }
func (a *AuthDelete) Example() any { return &AuthDelete{EndpointPath: "auth"} }
func (a *AuthDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epDeleteAuth(ctx, a)
}

// AuthVerifyPassword confirms plaintext matches a stored bcrypt hash.
type AuthVerifyPassword struct {
	TableName string `arg:"table_name,required" desc:"User table."`
	UserID    int    `arg:"user_id,required"    desc:"Row id of the user."`
	Password  string `arg:"password,required"   desc:"Plaintext to verify."`
}

func (a *AuthVerifyPassword) Doc() string {
	return "Confirm a plaintext password matches a stored bcrypt hash."
}
func (a *AuthVerifyPassword) ReadOnly() bool { return true }
func (a *AuthVerifyPassword) Example() any {
	return &AuthVerifyPassword{TableName: "users", UserID: 1, Password: "hunter2"}
}
func (a *AuthVerifyPassword) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epVerifyPassword(ctx, a)
}
