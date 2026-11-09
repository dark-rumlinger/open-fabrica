package auth

import (
	"openfabrica/tools"
	"openfabrica/tools/api"
)

// manage_oauth layers OAuth providers (Google, GitHub, ...) on top of an
// existing manage_auth endpoint. Handlers live in oauth.go since they share
// the JWT issuance plumbing; this file is the schema declaration.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_oauth",
		Category: "auth",
		OneLiner: "Add OAuth providers (Google, GitHub, ...) on top of an existing auth endpoint.",
		WhenToUse: `Use when the plan asks for 'sign in with X' - this tool implements the plan's
create_oauth capability. An auth endpoint must already exist; create attaches one
provider. Secrets are encrypted at rest. Browser flow:
button -> /api/<path>/oauth/<provider> -> provider -> redirect back with token.

Do NOT call create during a fresh build: client_id/client_secret are real
credentials the owner obtains from the provider's console - you never have them.
Placeholders ship a row that fails with invalid_client and silence the go-live
prompt. At BUILD time scaffold the sign-in button only and leave the provider
unconfigured (it returns 'provider not configured' until set up); call create
later in owner chat once the owner pastes their real credentials (update replaces the
credentials of a provider already registered).`,
		Actions: []tools.Action{
			&OAuthCreate{},
			&OAuthUpdate{},
			&OAuthList{},
			&OAuthDelete{},
		},
		Summarize: api.EpSummarize,
	})
}

// OAuthCreate registers an OAuth provider against an existing auth endpoint.
type OAuthCreate struct {
	ProviderName  string `arg:"provider_name,required" desc:"OAuth provider id (e.g. 'google', 'github')."`
	DisplayName   string `arg:"display_name"           desc:"OAuth button label."`
	ClientID      string `arg:"client_id,required"     desc:"OAuth client id."`
	ClientSecret  string `arg:"client_secret,required" desc:"OAuth client secret (encrypted at rest)."`
	AuthorizeURL  string `arg:"authorize_url,required" desc:"Provider authorize URL."`
	TokenURL      string `arg:"token_url,required"     desc:"Provider token URL."`
	UserinfoURL   string `arg:"userinfo_url,required"  desc:"Provider userinfo URL."`
	Scopes        string `arg:"scopes"                 desc:"Space-separated scopes." default:"openid email profile"`
	UsernameField string `arg:"username_field"         desc:"Field from OAuth userinfo used as username." default:"email"`
	AuthPath      string `arg:"auth_path,required"     desc:"Existing auth endpoint path this OAuth attaches to."`
}

func (a *OAuthCreate) Doc() string { return "Register an OAuth provider against an auth endpoint." }
func (a *OAuthCreate) Example() any {
	return &OAuthCreate{
		AuthPath: "auth", ProviderName: "google",
		ClientID: "...", ClientSecret: "...",
		AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     "https://oauth2.googleapis.com/token",
		UserinfoURL:  "https://openidconnect.googleapis.com/v1/userinfo",
	}
}
func (a *OAuthCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epCreateOAuth(ctx, oauthProviderConfig{
		ProviderName:  a.ProviderName,
		DisplayName:   a.DisplayName,
		ClientID:      a.ClientID,
		ClientSecret:  a.ClientSecret,
		AuthorizeURL:  a.AuthorizeURL,
		TokenURL:      a.TokenURL,
		UserinfoURL:   a.UserinfoURL,
		Scopes:        a.Scopes,
		UsernameField: a.UsernameField,
		AuthPath:      a.AuthPath,
	})
}

// OAuthList lists every OAuth provider attached to any auth endpoint.
type OAuthList struct{}

func (a *OAuthList) Doc() string    { return "List every OAuth provider attached to any auth endpoint." }
func (a *OAuthList) ReadOnly() bool { return true }
func (a *OAuthList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epListOAuth(ctx)
}

// OAuthDelete detaches an OAuth provider from an auth endpoint.
type OAuthDelete struct {
	AuthPath     string `arg:"auth_path,required"     desc:"Auth endpoint path the OAuth is attached to."`
	ProviderName string `arg:"provider_name,required" desc:"OAuth provider to remove."`
}

func (a *OAuthDelete) Doc() string { return "Remove an OAuth provider from an auth endpoint." }
func (a *OAuthDelete) Example() any {
	return &OAuthDelete{AuthPath: "auth", ProviderName: "google"}
}
func (a *OAuthDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epDeleteOAuth(ctx, a.ProviderName, a.AuthPath)
}

// OAuthUpdate replaces the settings of a provider already registered. Without it a
// provider created with wrong credentials could only be deleted and re-created.
type OAuthUpdate struct {
	ProviderName  string `arg:"provider_name,required" desc:"OAuth provider id to change."`
	DisplayName   string `arg:"display_name"           desc:"New button label."`
	ClientID      string `arg:"client_id"              desc:"New OAuth client id."`
	ClientSecret  string `arg:"client_secret"          desc:"New OAuth client secret (encrypted at rest)."`
	AuthorizeURL  string `arg:"authorize_url"          desc:"New provider authorize URL."`
	TokenURL      string `arg:"token_url"              desc:"New provider token URL."`
	UserinfoURL   string `arg:"userinfo_url"           desc:"New provider userinfo URL."`
	Scopes        string `arg:"scopes"                 desc:"New space-separated scopes."`
	UsernameField string `arg:"username_field"         desc:"New userinfo field used as the username."`
}

func (a *OAuthUpdate) Doc() string {
	return "Change a registered OAuth provider's credentials or URLs; an omitted arg keeps its stored value."
}
func (a *OAuthUpdate) Example() any {
	return &OAuthUpdate{ProviderName: "google", ClientID: "...", ClientSecret: "..."}
}
func (a *OAuthUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epUpdateOAuth(ctx, a)
}
