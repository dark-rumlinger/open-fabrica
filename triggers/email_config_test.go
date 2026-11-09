package triggers

import (
	"strings"
	"testing"
)

func TestResolveEmailConfigEscapesHTMLAndGuardsHeaders(t *testing.T) {
	payload := map[string]any{
		"email":   "ada@example.com",
		"name":    "Ada",
		"message": `<a href="https://evil.example">Reset your password</a> {{email}}`,
		"title":   "Hello\r\nBcc: attacker@evil.example",
	}
	config := map[string]any{
		"to":            "{{email}}",
		"subject":       "Re: {{title}}",
		"body_html":     "<p>{{name}} wrote:</p><blockquote>{{message}}</blockquote>",
		"template_vars": map[string]any{"msg": "{{message}}"},
	}
	if err := resolveEmailConfig(config, payload); err != nil {
		t.Fatal(err)
	}
	body := config["body_html"].(string)
	if strings.Contains(body, "<a href") || !strings.Contains(body, "&lt;a href=") {
		t.Errorf("visitor text became markup in the email body: %s", body)
	}
	if strings.Contains(body, "ada@example.com") {
		t.Errorf("a placeholder inside a substituted value was expanded again: %s", body)
	}
	if v := config["template_vars"].(map[string]any)["msg"].(string); strings.Contains(v, "<a href") {
		t.Errorf("template vars were not escaped: %s", v)
	}
	if subj := config["subject"].(string); strings.ContainsAny(subj, "\r\n") {
		t.Errorf("a line break reached the subject: %q", subj)
	}
	if config["to"] != "ada@example.com" {
		t.Errorf("to = %v", config["to"])
	}
}

func TestResolveEmailConfigRejectsBadRecipients(t *testing.T) {
	for name, to := range map[string]string{
		"adds a recipient":       "a@x.io, b@evil.io\nBcc: c@evil.io",
		"angle brackets":         "Ada <a@x.io>",
		"no address":             "not-an-address",
		"empty":                  "",
		"newline header attempt": "a@x.io\r\nBcc: e@evil.io",
	} {
		cfg := map[string]any{"to": "{{who}}"}
		if err := resolveEmailConfig(cfg, map[string]any{"who": to}); err == nil && name != "adds a recipient" {
			t.Errorf("%s: %q was accepted", name, to)
		}
	}
	// A comma separated list of plain addresses is fine.
	if err := resolveEmailConfig(map[string]any{"to": "a@x.io, b@y.io"}, nil); err != nil {
		t.Errorf("a list of plain addresses: %v", err)
	}
}
