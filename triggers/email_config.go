package triggers

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// recipientRe is one plain address: no spaces, commas, angle brackets or quotes.
var recipientRe = regexp.MustCompile(`^[^\s,;<>"']+@[^\s,;<>"']+\.[^\s,;<>"']+$`)

// escapedPayload returns the payload with every value rendered as HTML-escaped text,
// for substitution into the body of an HTML email.
func escapedPayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		out[k] = html.EscapeString(fmt.Sprintf("%v", v))
	}
	return out
}

// singleLine removes line breaks, which in a header value would start a new header.
func singleLine(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// resolveEmailConfig fills a send_email trigger's {{placeholders}} from the event.
// Event data can be visitor-supplied (a contact form's message), so it is treated
// as untrusted: values reaching the HTML body or the template variables are
// escaped (a message cannot become markup or a phishing link), the subject cannot
// carry a line break, and every recipient must be a plain address (a value cannot
// add recipients or smuggle in headers).
func resolveEmailConfig(config, payload map[string]any) error {
	html := escapedPayload(payload)
	// Pulled aside so the generic pass does not substitute into them a second time.
	aside := map[string]any{}
	for _, key := range []string{"body_html", "template_vars"} {
		if v, ok := config[key]; ok {
			aside[key] = v
			delete(config, key)
		}
	}
	resolveConfigTemplates(config, payload)
	for key, v := range aside {
		config[key] = resolveConfigTemplates(v, html)
	}

	if s, ok := config["subject"].(string); ok {
		config["subject"] = singleLine(s)
	}
	if to, ok := config["to"].(string); ok {
		for _, addr := range strings.Split(to, ",") {
			if !recipientRe.MatchString(strings.TrimSpace(addr)) {
				return fmt.Errorf("send_email: %q is not a valid recipient address", singleLine(addr))
			}
		}
	}
	return nil
}
