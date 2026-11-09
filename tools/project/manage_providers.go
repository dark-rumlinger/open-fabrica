package project

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"openfabrica/internal/netguard"
	"openfabrica/tools"
	"openfabrica/tools/api"
	"openfabrica/tools/internal/provider"
)

// manage_providers registers external API providers (base URL + auth shape +
// secret reference) and dispatches authed calls against them. Provider rows
// are shared infrastructure: manage_email/push/payments all reference
// them by name.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_providers",
		Category: "project",
		OneLiner: "Register external API providers and make authed calls by name.",
		WhenToUse: `Register a provider once with add (base URL, auth type, secret_name pointing at
the secrets store) and call it later with request. Prefer this over raw HTTP when
the project will hit the same external API repeatedly - the credentials and
auth shape live in one place. To reach a device on the local network (e.g. a
Home Assistant hub), register it with allow_private_host=true; the SSRF block
stays on for every other host. Use describe to pull an unfamiliar API's OpenAPI
doc into a compact operation list before calling it. manage_guides recall
slug=connect_any_api covers the full secrets -> add -> request workflow and the
LAN/Home Assistant example.`,
		Actions: []tools.Action{
			&ProviderAdd{},
			&ProviderList{},
			&ProviderRemove{},
			&ProviderUpdate{},
			&ProviderRequest{},
			&ProviderDescribe{},
		},
		// describe can return a sizable operation list; lift the default 6000-char
		// result cap so a useful slice of a real API surface survives truncation.
		MaxResult: 12000,
	})
}

// ProviderAdd registers a new provider record.
type ProviderAdd struct {
	Name        string `arg:"name,required"     desc:"Provider name (referenced by other tools)."`
	BaseURL     string `arg:"base_url,required" desc:"Base URL of the API."`
	AuthType    string `arg:"auth_type"         desc:"Credential carrier." enum:"none,bearer,api_key_header,basic" default:"bearer"`
	AuthHeader  string `arg:"auth_header"       desc:"Header name for auth." default:"Authorization"`
	AuthPrefix  string `arg:"auth_prefix"       desc:"Auth value prefix." default:"Bearer"`
	SecretName  string `arg:"secret_name"       desc:"Secrets-store entry holding the credential."`
	Description string `arg:"description"       desc:"Human-readable description."`
	APIDocs     string `arg:"api_docs"          desc:"URL or note pointing at the API's docs."`
	// AllowPrivateHost is the scoped opt-in for LAN devices. Off by default;
	// the request action keeps the SSRF block on for every provider unless this
	// is set, and even then only the provider's own base_url host is reachable.
	AllowPrivateHost bool `arg:"allow_private_host" desc:"Permit calls to a private/LAN host (e.g. a local Home Assistant hub at 192.168.x or a .local name). Off by default; set only for a trusted device on the operator's own network. Scoped to this provider's own host."`
}

func (a *ProviderAdd) Doc() string { return "Register a new provider record." }
func (a *ProviderAdd) Example() any {
	return &ProviderAdd{Name: "stripe", BaseURL: "https://api.stripe.com", AuthType: "bearer", SecretName: "STRIPE_KEY"}
}
func (a *ProviderAdd) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	allowPriv := 0
	if a.AllowPrivateHost {
		allowPriv = 1
	}
	result, err := ctx.DB.Exec(
		`INSERT INTO of_service_providers (name, base_url, auth_type, auth_header, auth_prefix, secret_name, description, api_docs, allow_private_host)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Name, a.BaseURL, a.AuthType, a.AuthHeader, a.AuthPrefix, a.SecretName, a.Description, a.APIDocs, allowPriv,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			return tools.Fail("provider '%s' already exists for this project", a.Name), nil
		}
		return nil, fmt.Errorf("adding provider: %w", err)
	}
	id, _ := result.LastInsertId()
	return tools.Ok(map[string]any{"id": id, "name": a.Name}), nil
}

// ProviderList enumerates registered providers.
type ProviderList struct{}

func (a *ProviderList) Doc() string    { return "List registered providers." }
func (a *ProviderList) ReadOnly() bool { return true }
func (a *ProviderList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		`SELECT id, name, base_url, auth_type, secret_name, description, api_docs, is_enabled, allow_private_host, created_at
		 FROM of_service_providers ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing providers: %w", err)
	}
	defer rows.Close()
	var providers []map[string]any
	for rows.Next() {
		var id int
		var name, baseURL, authType, description, apiDocs string
		var secretName sql.NullString
		var isEnabled, allowPrivate bool
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &baseURL, &authType, &secretName, &description, &apiDocs, &isEnabled, &allowPrivate, &createdAt); err != nil {
			ctx.Logger.Warn("scan error in service providers list", "error", err)
			continue
		}
		prov := map[string]any{
			"id": id, "name": name, "base_url": baseURL,
			"auth_type": authType, "description": description,
			"api_docs": apiDocs, "is_enabled": isEnabled,
			"allow_private_host": allowPrivate,
		}
		if secretName.Valid {
			prov["secret_name"] = secretName.String
		}
		providers = append(providers, prov)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing providers: %w", err)
	}
	if providers == nil {
		providers = []map[string]any{}
	}
	return tools.Ok(providers), nil
}

// ProviderRemove deletes a provider by name or id.
type ProviderRemove struct {
	Name string `arg:"name" desc:"Provider name to remove."`
	ID   int64  `arg:"id"   desc:"Provider id to remove."`
}

func (a *ProviderRemove) Doc() string  { return "Delete a provider by name or id." }
func (a *ProviderRemove) Example() any { return &ProviderRemove{Name: "stripe"} }
func (a *ProviderRemove) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var res sql.Result
	var err error
	switch {
	case a.Name != "":
		res, err = ctx.DB.Exec("DELETE FROM of_service_providers WHERE name = ?", a.Name)
	case a.ID > 0:
		res, err = ctx.DB.Exec("DELETE FROM of_service_providers WHERE id = ?", a.ID)
	default:
		return tools.Fail("name or id is required"), nil
	}
	if err != nil {
		return nil, fmt.Errorf("removing provider: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return tools.NotFound("provider", a.Name+idSuffix(a.ID)), nil
	}
	return tools.Ok("provider removed"), nil
}

// ProviderUpdate patches one or more fields on an existing provider. Lookup
// is by name; pass only the fields you want to change.
type ProviderUpdate struct {
	Name        string `arg:"name,required" desc:"Provider name (key for lookup)."`
	Description string `arg:"description"   desc:"New description."`
	APIDocs     string `arg:"api_docs"      desc:"New docs URL/note."`
	BaseURL     string `arg:"base_url"      desc:"New base URL."`
	AuthType    string `arg:"auth_type"     desc:"New credential carrier."`
	SecretName  string `arg:"secret_name"   desc:"New secrets-store entry name."`
}

func (a *ProviderUpdate) Doc() string {
	return "Update provider fields. Only supplied fields are changed."
}
func (a *ProviderUpdate) Example() any {
	return &ProviderUpdate{Name: "stripe", Description: "Stripe live API"}
}
func (a *ProviderUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	setClauses := []string{}
	values := []any{}
	if a.Description != "" {
		setClauses = append(setClauses, "description = ?")
		values = append(values, a.Description)
	}
	if a.APIDocs != "" {
		setClauses = append(setClauses, "api_docs = ?")
		values = append(values, a.APIDocs)
	}
	if a.BaseURL != "" {
		setClauses = append(setClauses, "base_url = ?")
		values = append(values, a.BaseURL)
	}
	if a.AuthType != "" {
		setClauses = append(setClauses, "auth_type = ?")
		values = append(values, a.AuthType)
	}
	if a.SecretName != "" {
		setClauses = append(setClauses, "secret_name = ?")
		values = append(values, a.SecretName)
	}
	if len(setClauses) == 0 {
		return tools.Fail("no fields to update"), nil
	}
	query := fmt.Sprintf("UPDATE of_service_providers SET %s WHERE name = ?", strings.Join(setClauses, ", "))
	values = append(values, a.Name)
	res, err := ctx.DB.Exec(query, values...)
	if err != nil {
		return nil, fmt.Errorf("updating provider: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return tools.Fail("provider '%s' not found", a.Name), nil
	}
	return tools.Ok("provider updated"), nil
}

// ProviderRequest dispatches an authed HTTP call against a registered
// provider. The provider's auth shape (bearer/api_key_header/basic) is
// applied automatically from the stored secret.
type ProviderRequest struct {
	Provider    string         `arg:"provider,required" desc:"Provider name."`
	Method      string         `arg:"method"            desc:"HTTP method." enum:"GET,HEAD,POST,PUT,DELETE,PATCH" default:"POST"`
	Path        string         `arg:"path"              desc:"API path to append to base URL."`
	RequestBody string         `arg:"request_body"      desc:"Request body."`
	Headers     map[string]any `arg:"headers"           desc:"Extra request headers."`
}

func (a *ProviderRequest) Doc() string {
	return "Make an authed HTTP call against a registered provider. The body in the result is capped at 8 KB (body_truncated:true when trimmed)."
}
func (a *ProviderRequest) ReadOnly() bool { return false }
func (a *ProviderRequest) Example() any {
	return &ProviderRequest{Provider: "stripe", Method: "GET", Path: "/charges"}
}
func (a *ProviderRequest) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	creds, errR := provider.Load(ctx, a.Provider)
	if errR != nil {
		return errR, nil
	}

	fullURL := strings.TrimRight(creds.BaseURL, "/")
	if a.Path != "" {
		path := a.Path
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		fullURL += path
	}

	// SSRF guard: external calls may not resolve to a private/internal IP. A
	// provider explicitly registered with allow_private_host (a trusted LAN
	// device, e.g. a local Home Assistant hub) lifts that block, but only for its
	// own configured host - every other internal address stays blocked, here and
	// on any redirect.
	allowHost := ""
	if providerAllowsPrivateHost(ctx, a.Provider) {
		if pu, err := neturl.Parse(creds.BaseURL); err == nil {
			allowHost = pu.Hostname()
		}
	}
	if err := netguard.ValidateExternalURLAllowingHost(fullURL, allowHost); err != nil {
		return tools.Fail("blocked URL: %v", err), nil
	}

	var bodyReader io.Reader
	if a.RequestBody != "" {
		bodyReader = strings.NewReader(a.RequestBody)
	}
	req, err := http.NewRequest(a.Method, fullURL, bodyReader)
	if err != nil {
		return tools.Fail("creating request: %v", err), nil
	}
	req.Header.Set("Content-Type", "application/json")

	// Centralized auth injection from the stored secret (bearer/api_key_header/
	// basic/none). basic credentials are base64-encoded here, the same path
	// messaging and payments use, so this verb-agnostic call cannot drift.
	if errR := provider.InjectAuth(ctx, req, creds, ""); errR != nil {
		return errR, nil
	}

	for k, v := range a.Headers {
		if vs, ok := v.(string); ok {
			req.Header.Set(k, vs)
		}
	}

	client := netguard.SSRFSafeClientAllowingHost(allowHost)
	client.Timeout = 60 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return tools.Fail("executing request: %v", err), nil
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, api.HTTPBodyCap+1))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	data := map[string]any{
		"status_code": resp.StatusCode,
		"status":      resp.Status,
		"headers":     flattenHeaders(resp.Header),
	}
	body, cut := api.ClipResultBody(string(respBody))
	if cut || len(respBody) > api.HTTPBodyCap {
		data["body_truncated"] = true
	}
	data["body"] = body
	r := &tools.Result{Success: resp.StatusCode >= 200 && resp.StatusCode < 300, Data: data}
	if !r.Success {
		r.Error = fmt.Sprintf("the provider answered %s; its response body is in data.body", resp.Status)
	}
	return r, nil
}

// providerAllowsPrivateHost reports whether the named provider was registered
// with the allow_private_host opt-in. Checks the project-local connector first,
// then a globally-enabled one. A scan failure (column absent on an older schema,
// or a missing row) defaults to false - the most restrictive answer, so the SSRF
// block stays on whenever the flag can't be positively confirmed.
func providerAllowsPrivateHost(ctx *tools.ToolContext, name string) bool {
	var allow bool
	if err := ctx.DB.QueryRow(
		"SELECT allow_private_host FROM of_service_providers WHERE name = ?", name,
	).Scan(&allow); err == nil {
		return allow
	}
	if ctx.GlobalDB == nil {
		return false
	}
	var en int
	if err := ctx.DB.QueryRow(
		"SELECT COUNT(*) FROM of_enabled_connectors WHERE global_name = ?", name,
	).Scan(&en); err != nil || en == 0 {
		return false
	}
	if err := ctx.GlobalDB.QueryRow(
		"SELECT allow_private_host FROM global_connectors WHERE name = ?", name,
	).Scan(&allow); err != nil {
		return false
	}
	return allow
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// idSuffix renders a numeric id lookup for a not-found message ("" when unused).
func idSuffix(id int64) string {
	if id <= 0 {
		return ""
	}
	return fmt.Sprintf("#%d", id)
}
