package messaging

import (
	"encoding/json"
	"fmt"
	neturl "net/url"
	"strings"
	"time"

	"openfabrica/internal/replayoutbox"
	"openfabrica/tools"
	"openfabrica/tools/internal/httpclient"
	"openfabrica/tools/internal/provider"
)

// manage_email registers transactional email send via SendGrid, Mailgun,
// Resend, or a generic JSON endpoint. configure once with a registered
// provider, then send inline or via named templates with {{key}} placeholders.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_email",
		Category: "messaging",
		OneLiner: "Send transactional email (inline body or named templates; provider-agnostic).",
		WhenToUse: `Use to send transactional email - signup confirmations, order
receipts, notifications. Password-reset and magic-link emails are sent by the
platform through this same configuration. The provider must be registered via manage_providers and configured
here once before send works. For repeated copy, save_template once and pass
template_name + template_vars to send.`,
		Actions: []tools.Action{
			&EmailConfigure{},
			&EmailSend{},
			&EmailSaveTemplate{},
			&EmailListTemplates{},
		},
	})
}

// EmailConfigure binds the email tool to a registered provider and a default
// sender pair. Stored as a singleton row in of_email_config.
type EmailConfigure struct {
	ProviderName string `arg:"provider_name,required" desc:"Must match a registered provider name."`
	ProviderType string `arg:"provider_type"          desc:"Provider family." enum:"sendgrid,mailgun,resend,generic" default:"generic"`
	FromAddress  string `arg:"from_address,required"  desc:"Default sender email."`
	FromName     string `arg:"from_name"              desc:"Default sender display name."`
}

func (a *EmailConfigure) Doc() string { return "Bind the email tool to a registered provider." }
func (a *EmailConfigure) Example() any {
	return &EmailConfigure{ProviderName: "sendgrid", ProviderType: "sendgrid", FromAddress: "no-reply@example.com"}
}
func (a *EmailConfigure) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if r := provider.Require(ctx, a.ProviderName); r != nil {
		return r, nil
	}
	if _, err := ctx.DB.Exec(
		`INSERT INTO of_email_config (id, provider_name, provider_type, from_address, from_name)
		 VALUES (1, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   provider_name = excluded.provider_name,
		   provider_type = excluded.provider_type,
		   from_address = excluded.from_address,
		   from_name = excluded.from_name`,
		a.ProviderName, a.ProviderType, a.FromAddress, a.FromName,
	); err != nil {
		return nil, fmt.Errorf("configuring email: %w", err)
	}
	return tools.Ok(map[string]any{
		"provider":     a.ProviderName,
		"type":         a.ProviderType,
		"from_address": a.FromAddress,
		"from_name":    a.FromName,
	}), nil
}

// EmailSend sends one email - inline subject/body OR a saved template
// expanded with template_vars. Provider-specific request shapes are built
// per provider_type.
type EmailSend struct {
	To           string         `arg:"to,required" desc:"Recipient email address."`
	Subject      string         `arg:"subject"     desc:"Subject line (required if not using a template)."`
	BodyHTML     string         `arg:"body_html"   desc:"HTML body."`
	BodyText     string         `arg:"body_text"   desc:"Plain-text body."`
	TemplateName string         `arg:"template_name" desc:"Saved template to use."`
	TemplateVars map[string]any `arg:"template_vars" desc:"Values for {{key}} placeholders in the template."`
}

func (a *EmailSend) Doc() string { return "Send one email, inline or via a saved template." }
func (a *EmailSend) Example() any {
	return &EmailSend{To: "a@b.com", Subject: "Welcome", BodyHTML: "<p>Hello.</p>"}
}
func (a *EmailSend) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	subject, bodyHTML, bodyText := a.Subject, a.BodyHTML, a.BodyText

	if a.TemplateName != "" {
		var tmplSubject, tmplHTML, tmplText string
		err := ctx.DB.QueryRow(
			"SELECT subject, body_html, body_text FROM of_email_templates WHERE name = ?",
			a.TemplateName,
		).Scan(&tmplSubject, &tmplHTML, &tmplText)
		if err != nil {
			return tools.Fail("template '%s' not found", a.TemplateName), nil
		}
		if subject == "" {
			subject = tmplSubject
		}
		if bodyHTML == "" {
			bodyHTML = tmplHTML
		}
		if bodyText == "" {
			bodyText = tmplText
		}
	}
	subject = tools.ResolveTemplateVars(subject, a.TemplateVars)
	bodyHTML = tools.ResolveTemplateVars(bodyHTML, a.TemplateVars)
	bodyText = tools.ResolveTemplateVars(bodyText, a.TemplateVars)
	if a.To == "" || subject == "" || (bodyHTML == "" && bodyText == "") {
		return tools.Fail("to, subject, and body_html (or body_text) are required"), nil
	}

	var providerName, providerType, fromAddress, fromName string
	if err := ctx.DB.QueryRow(
		"SELECT provider_name, provider_type, from_address, from_name FROM of_email_config WHERE id = 1",
	).Scan(&providerName, &providerType, &fromAddress, &fromName); err != nil {
		return tools.Fail("email is not configured yet; configure an email provider before sending"), nil
	}
	if !emailTypeSupported(providerType) {
		return tools.Fail("configured provider_type %q is not supported; reconfigure with one of sendgrid, mailgun, resend, or generic", providerType), nil
	}
	creds, errR := provider.Load(ctx, providerName)
	if errR != nil {
		return errR, nil
	}

	// A user-flow replay keeps the mail instead of sending it (see replayoutbox).
	if replayoutbox.Capture(ctx.ProjectID, replayoutbox.Message{Kind: "email", To: a.To, Subject: subject, Body: strings.TrimSpace(bodyText + "\n" + bodyHTML)}) {
		return tools.Ok(map[string]any{"to": a.To, "subject": subject, "status": 0, "captured": "a user-flow replay is running: the email was kept for it, not delivered"}), nil
	}
	url, body, contentType := buildEmailRequest(providerType, creds.BaseURL, a.To, fromAddress, fromName, subject, bodyHTML, bodyText)
	_, status, errR := httpclient.PostWithAuth(ctx, creds, url, body, contentType, "")
	if errR != nil {
		return errR, nil
	}
	return tools.Ok(map[string]any{"to": a.To, "subject": subject, "status": status}), nil
}

// emailTypeSupported reports whether buildEmailRequest implements providerType
// natively. "generic" is supported via the default JSON shape; any other value
// (e.g. a stale config row from a provider type that was never implemented)
// is rejected by the send path rather than silently posting the generic body.
func emailTypeSupported(providerType string) bool {
	switch providerType {
	case "sendgrid", "mailgun", "resend", "generic":
		return true
	}
	return false
}

// buildEmailRequest emits the URL/body/content-type triple per provider type.
// body_text, when present, is included alongside body_html for every provider
// that supports a plain-text part so a text-only email is not dropped.
func buildEmailRequest(providerType, baseURL, to, fromAddress, fromName, subject, bodyHTML, bodyText string) (url, body, contentType string) {
	switch providerType {
	case "sendgrid":
		url = strings.TrimRight(baseURL, "/") + "/mail/send"
		contentType = "application/json"
		content := []map[string]string{}
		if bodyText != "" {
			content = append(content, map[string]string{"type": "text/plain", "value": bodyText})
		}
		if bodyHTML != "" {
			content = append(content, map[string]string{"type": "text/html", "value": bodyHTML})
		}
		payload := map[string]any{
			"personalizations": []map[string]any{
				{"to": []map[string]string{{"email": to}}},
			},
			"from":    map[string]string{"email": fromAddress, "name": fromName},
			"subject": subject,
			"content": content,
		}
		data, _ := json.Marshal(payload)
		body = string(data)
	case "mailgun":
		url = strings.TrimRight(baseURL, "/") + "/messages"
		contentType = "application/x-www-form-urlencoded"
		form := neturl.Values{}
		form.Set("from", fmt.Sprintf("%s <%s>", fromName, fromAddress))
		form.Set("to", to)
		form.Set("subject", subject)
		if bodyHTML != "" {
			form.Set("html", bodyHTML)
		}
		if bodyText != "" {
			form.Set("text", bodyText)
		}
		body = form.Encode()
	case "resend":
		url = strings.TrimRight(baseURL, "/") + "/emails"
		contentType = "application/json"
		payload := map[string]any{
			"from": fmt.Sprintf("%s <%s>", fromName, fromAddress),
			"to":   []string{to}, "subject": subject,
		}
		if bodyHTML != "" {
			payload["html"] = bodyHTML
		}
		if bodyText != "" {
			payload["text"] = bodyText
		}
		data, _ := json.Marshal(payload)
		body = string(data)
	default:
		url = strings.TrimRight(baseURL, "/") + "/send"
		contentType = "application/json"
		payload := map[string]any{
			"from": fromAddress, "to": to, "subject": subject,
			"html": bodyHTML, "text": bodyText,
		}
		data, _ := json.Marshal(payload)
		body = string(data)
	}
	return
}

// EmailSaveTemplate persists a named template for reuse via EmailSend.
type EmailSaveTemplate struct {
	TemplateName string `arg:"template_name,required" desc:"Template name (unique)."`
	Subject      string `arg:"subject,required"       desc:"Subject line; supports {{key}} placeholders."`
	BodyHTML     string `arg:"body_html,required"     desc:"HTML body; supports {{key}} placeholders."`
	BodyText     string `arg:"body_text"              desc:"Plain-text body."`
}

func (a *EmailSaveTemplate) Doc() string { return "Persist a named template for reuse." }
func (a *EmailSaveTemplate) Example() any {
	return &EmailSaveTemplate{TemplateName: "welcome", Subject: "Welcome, {{name}}", BodyHTML: "<p>Hi {{name}}.</p>"}
}
func (a *EmailSaveTemplate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if _, err := ctx.DB.Exec(
		`INSERT INTO of_email_templates (name, subject, body_html, body_text)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   subject = excluded.subject,
		   body_html = excluded.body_html,
		   body_text = excluded.body_text`,
		a.TemplateName, a.Subject, a.BodyHTML, a.BodyText,
	); err != nil {
		return nil, fmt.Errorf("saving template: %w", err)
	}
	return tools.Ok(map[string]any{"name": a.TemplateName, "subject": a.Subject}), nil
}

// EmailListTemplates returns all saved templates ordered by name.
type EmailListTemplates struct{}

func (a *EmailListTemplates) Doc() string    { return "List every saved email template." }
func (a *EmailListTemplates) ReadOnly() bool { return true }
func (a *EmailListTemplates) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query("SELECT name, subject, created_at FROM of_email_templates ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("listing templates: %w", err)
	}
	defer rows.Close()
	var templates []map[string]any
	for rows.Next() {
		var name, subject string
		var createdAt time.Time
		if err := rows.Scan(&name, &subject, &createdAt); err != nil {
			continue
		}
		templates = append(templates, map[string]any{
			"name": name, "subject": subject, "created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing templates: %w", err)
	}
	return tools.Ok(templates), nil
}
