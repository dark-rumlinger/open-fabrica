// Package provider centralizes connector resolution and Authorization-header
// injection used by messaging (email), payments (Stripe/PayPal/etc), and the
// generic manage_providers tool. It is the single source of truth for every
// connector call path. Resolution tries the project-local of_service_providers
// first and falls back to a globally-registered connector the project has
// enabled (of_enabled_connectors -> global_connectors in the global DB), so a
// project-local name always shadows a global one.
package provider

import (
	"database/sql"
	"encoding/base64"
	"net/http"
	"strings"

	"openfabrica/tools"
)

// Creds is the loaded shape of one connector. The credential is resolved lazily
// inside InjectAuth so callers never see the encryptor surface. A project-local
// connector carries SecretName (-> of_secrets); a global connector carries
// InlineSecret (the already-encrypted global credential). At most one is set.
type Creds struct {
	BaseURL      string
	AuthType     string
	AuthHeader   string
	AuthPrefix   string
	SecretName   sql.NullString
	InlineSecret sql.NullString
}

// Require reports that a connector exists (project-local or globally enabled for
// the project), returning nil when it does or a *tools.Result describing the
// failure when it does not. Used by the configure actions of email/payments,
// which only need to confirm the connector is registered before binding to it.
func Require(ctx *tools.ToolContext, name string) *tools.Result {
	var exists int
	if err := ctx.DB.QueryRow(
		"SELECT COUNT(*) FROM of_service_providers WHERE name = ?", name,
	).Scan(&exists); err == nil && exists > 0 {
		return nil
	}
	if globalConnectorEnabled(ctx, name) {
		return nil
	}
	return tools.Fail("provider '%s' is not registered yet; register the provider before binding to it", name)
}

// Load fetches the (enabled) connector by name, trying project-local first and
// then a globally-enabled connector. Returns a *tools.Result describing the
// failure so the calling action handler can pass it straight back to the LLM.
func Load(ctx *tools.ToolContext, name string) (*Creds, *tools.Result) {
	c, err := loadProjectConnector(ctx, name)
	if err == nil {
		return c, nil
	}
	if err != sql.ErrNoRows {
		// A real query error (not "no such row") - surface it as not-found, the
		// same shape callers handled before.
		return nil, tools.Fail("provider '%s' not found or disabled", name)
	}
	if c, gerr := loadGlobalConnector(ctx, name); gerr == nil {
		return c, nil
	}
	return nil, tools.Fail("provider '%s' not found or disabled", name)
}

func loadProjectConnector(ctx *tools.ToolContext, name string) (*Creds, error) {
	var c Creds
	err := ctx.DB.QueryRow(
		`SELECT base_url, auth_type, auth_header, auth_prefix, secret_name
		 FROM of_service_providers WHERE name = ? AND is_enabled = 1`,
		name,
	).Scan(&c.BaseURL, &c.AuthType, &c.AuthHeader, &c.AuthPrefix, &c.SecretName)
	if err != nil {
		return nil, err // includes sql.ErrNoRows
	}
	return &c, nil
}

func loadGlobalConnector(ctx *tools.ToolContext, name string) (*Creds, error) {
	if ctx.GlobalDB == nil || !globalConnectorEnabled(ctx, name) {
		return nil, sql.ErrNoRows
	}
	var c Creds
	err := ctx.GlobalDB.QueryRow(
		`SELECT base_url, auth_type, auth_header, auth_prefix, api_key_encrypted
		 FROM global_connectors WHERE name = ? AND is_enabled = 1`,
		name,
	).Scan(&c.BaseURL, &c.AuthType, &c.AuthHeader, &c.AuthPrefix, &c.InlineSecret)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// globalConnectorEnabled reports whether the project has opted into the named
// global connector. A missing of_enabled_connectors table (older project schema)
// or any error is treated as "not enabled" so resolution degrades safely.
func globalConnectorEnabled(ctx *tools.ToolContext, name string) bool {
	var en int
	if err := ctx.DB.QueryRow(
		"SELECT COUNT(*) FROM of_enabled_connectors WHERE global_name = ?", name,
	).Scan(&en); err != nil {
		return false
	}
	return en > 0
}

// InjectAuth decrypts the connector's credential and sets the matching
// Authorization-style header on req. Returns nil when the header was applied or
// when there is no credential (auth_type="none", or no secret configured);
// returns a *tools.Result on any secret-store or decryption failure.
//
// basicBase, when non-empty, is concatenated with the secret as
// "<basicBase>:<secret>" before base64-encoding for HTTP Basic auth (Twilio's
// account SID + auth token shape). Pass "" for connectors that put the secret
// alone in the Basic credential pair.
func InjectAuth(ctx *tools.ToolContext, req *http.Request, c *Creds, basicBase string) *tools.Result {
	if c.AuthType == "none" {
		return nil
	}
	secret, r := resolveSecret(ctx, c)
	if r != nil {
		return r
	}
	if secret == "" {
		return nil // no credential configured; send the request unauthenticated
	}
	switch c.AuthType {
	case "bearer":
		// An empty prefix sends the bare secret, not " <secret>".
		req.Header.Set(c.AuthHeader, strings.TrimSpace(c.AuthPrefix+" "+secret))
	case "api_key_header":
		req.Header.Set(c.AuthHeader, secret)
	case "basic":
		raw := secret
		if basicBase != "" {
			raw = basicBase + ":" + secret
		}
		req.Header.Set(c.AuthHeader, "Basic "+base64.StdEncoding.EncodeToString([]byte(raw)))
	}
	return nil
}

// resolveSecret returns the decrypted credential for a connector: the inline
// global credential when present, else the named project secret. Returns ("",
// nil) when no credential is configured.
func resolveSecret(ctx *tools.ToolContext, c *Creds) (string, *tools.Result) {
	var enc string
	switch {
	case c.InlineSecret.Valid && c.InlineSecret.String != "":
		enc = c.InlineSecret.String
	case c.SecretName.Valid && c.SecretName.String != "":
		if err := ctx.DB.QueryRow("SELECT value_encrypted FROM of_secrets WHERE name = ?", c.SecretName.String).Scan(&enc); err != nil {
			return "", tools.Fail("secret '%s' not found", c.SecretName.String)
		}
	default:
		return "", nil
	}
	if ctx.Encryptor == nil {
		return "", tools.Fail("encryption not available")
	}
	dec, err := ctx.Encryptor.Decrypt(enc)
	if err != nil {
		return "", tools.Fail("decrypt secret failed")
	}
	return dec, nil
}
