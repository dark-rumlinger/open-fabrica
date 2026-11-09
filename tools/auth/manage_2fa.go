package auth

import (
	"openfabrica/tools"
	"openfabrica/tools/api"
)

// manage_2fa registers TOTP authenticator-app codes and email magic-link
// passwordless login on top of an existing manage_auth endpoint. Handlers
// live in manage_2fa_handlers.go because they share the auth-endpoint storage;
// the structs
// here are thin schema declarations that build an args map and delegate.
//
// One arg struct per action - factor_type branching lives in Run(), but
// the field surface is shrunk per-action (disable / list only need
// auth_path; the totp / magic_link knobs are visible only on enable).

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_2fa",
		Category: "auth",
		OneLiner: "Add TOTP codes and magic-link passwordless email login to an auth endpoint.",
		WhenToUse: `Use enable/disable/list with factor_type='totp' for authenticator-app codes,
or factor_type='magic_link' for passwordless email. Both attach to an existing
auth endpoint - create that first, then layer 2FA on top.`,
		Actions: []tools.Action{
			&TwoFactorEnable{},
			&TwoFactorDisable{},
			&TwoFactorList{},
		},
		Summarize: api.EpSummarize,
	})
}

// --- enable -----------------------------------------------------------------

type TwoFactorEnable struct {
	FactorType    string  `arg:"factor_type,required" desc:"Which second factor." enum:"totp,magic_link"`
	AuthPath      string  `arg:"auth_path,required"   desc:"Existing auth endpoint path the 2FA attaches to."`
	Issuer        string  `arg:"issuer"               desc:"totp only: issuer label shown in authenticator apps."`
	Enforced      bool    `arg:"enforced"             desc:"totp only: if true, a user who has not enrolled gets no session from /login: it answers needs_totp_setup plus a single-purpose enroll_token for /totp/setup and /totp/verify, and the verified code signs them in. If false, /login challenges only users who already enrolled."`
	EmailTemplate string  `arg:"email_template"       desc:"magic_link only: NAME of a template saved with manage_email save_template; its body_html gets the link at {{magic_url}}. Omit for a plain default email."`
	RedirectURL   string  `arg:"redirect_url"         desc:"magic_link only: path after successful verification." default:"/"`
	TTLMinutes    float64 `arg:"ttl_minutes"          desc:"magic_link only: link validity in minutes." default:"15"`
}

func (a *TwoFactorEnable) ActionName() string { return "enable" }
func (a *TwoFactorEnable) Doc() string {
	return "Enable TOTP or magic-link on the named auth endpoint. Re-running it replaces the settings: omitted options reset to defaults."
}
func (a *TwoFactorEnable) Example() any {
	return &TwoFactorEnable{FactorType: "totp", AuthPath: "auth", Issuer: "MyApp"}
}
func (a *TwoFactorEnable) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	switch a.FactorType {
	case "totp":
		return authEnableTOTP(ctx, a.AuthPath, a.Issuer, a.Enforced)
	case "magic_link":
		return authEnableMagicLink(ctx, a.AuthPath, a.EmailTemplate, a.RedirectURL, a.TTLMinutes)
	}
	return tools.Fail("factor_type is required: totp | magic_link"), nil
}

// --- disable ----------------------------------------------------------------

type TwoFactorDisable struct {
	FactorType string `arg:"factor_type,required" desc:"Which second factor to turn off." enum:"totp,magic_link"`
	AuthPath   string `arg:"auth_path,required"   desc:"Existing auth endpoint path the 2FA was attached to."`
}

func (a *TwoFactorDisable) ActionName() string { return "disable" }
func (a *TwoFactorDisable) Doc() string {
	return "Disable TOTP or magic-link on the named auth endpoint."
}
func (a *TwoFactorDisable) Example() any {
	return &TwoFactorDisable{FactorType: "totp", AuthPath: "auth"}
}
func (a *TwoFactorDisable) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	switch a.FactorType {
	case "totp":
		return authDisableTOTP(ctx, a.AuthPath)
	case "magic_link":
		return authDisableMagicLink(ctx, a.AuthPath)
	}
	return tools.Fail("factor_type is required: totp | magic_link"), nil
}

// --- list -------------------------------------------------------------------

type TwoFactorList struct {
	FactorType string `arg:"factor_type,required" desc:"Which second factor to inspect." enum:"totp,magic_link"`
	AuthPath   string `arg:"auth_path"            desc:"Auth endpoint path to scope the read to. Omit to list all endpoints with this factor."`
}

func (a *TwoFactorList) ActionName() string { return "list" }
func (a *TwoFactorList) Doc() string        { return "List 2FA configuration for an auth endpoint." }
func (a *TwoFactorList) ReadOnly() bool     { return true }
func (a *TwoFactorList) Example() any {
	return &TwoFactorList{FactorType: "totp"}
}
func (a *TwoFactorList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	switch a.FactorType {
	case "totp":
		return authListTOTP(ctx, a.AuthPath)
	case "magic_link":
		return authListMagicLink(ctx, a.AuthPath)
	}
	return tools.Fail("factor_type is required: totp | magic_link"), nil
}
