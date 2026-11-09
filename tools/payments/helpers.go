package payments

import (
	"encoding/json"
	"fmt"
	"time"

	"openfabrica/tools"
)

// Shared infrastructure for the payments package: table bootstrapping, payment
// config loading, provider-specific webhook signature verification, and the
// response parsers each tool action calls into. Provider-row loading and HTTP
// auth injection now live in tools/internal/provider; the POST-with-auth path
// lives in tools/internal/httpclient. Tool-facing schema and dispatch live in
// the manage_*.go files.

// paymentConfig holds the loaded payment configuration.
type paymentConfig struct {
	providerName      string
	providerType      string
	currency          string
	webhookSecretName string
}

// paymentLoadConfig loads the singleton of_payment_config row. Returns an error
// Result when payments has not been configured yet - handlers surface it to
// the LLM unchanged so the user sees a clear "configure first" prompt.
func paymentLoadConfig(ctx *tools.ToolContext) (*paymentConfig, *tools.Result) {
	var cfg paymentConfig
	err := ctx.DB.QueryRow(
		"SELECT provider_name, provider_type, currency, webhook_secret_name FROM of_payment_config WHERE id = 1",
	).Scan(&cfg.providerName, &cfg.providerType, &cfg.currency, &cfg.webhookSecretName)
	if err != nil {
		return nil, tools.Fail("payments not configured, use manage_payments(action='configure') first")
	}
	return &cfg, nil
}

// requireStringField extracts a string field from a provider response, failing
// loudly when missing. Used after JSON-decoding provider replies where the
// field is contractually required (external IDs, checkout URLs).
func requireStringField(m map[string]any, key, provider string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("%s returned empty response body", provider)
	}
	v, ok := m[key].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("%s response missing required field %q", provider, key)
	}
	return v, nil
}

// parseCheckoutResponse extracts the checkout redirect URL and the provider's
// external session id from a fresh "create checkout" reply.
func parseCheckoutResponse(providerType string, data map[string]any) (checkoutURL, externalID string) {
	switch providerType {
	case "stripe":
		checkoutURL = tools.OptionalString(data, "url", "")
		externalID = tools.OptionalString(data, "id", "")
	case "paypal":
		externalID = tools.OptionalString(data, "id", "")
		if links, ok := data["links"].([]any); ok {
			for _, l := range links {
				link, ok := l.(map[string]any)
				if !ok {
					continue
				}
				if tools.OptionalString(link, "rel", "") == "approve" {
					checkoutURL = tools.OptionalString(link, "href", "")
				}
			}
		}
	case "mollie":
		externalID = tools.OptionalString(data, "id", "")
		if links, ok := data["_links"].(map[string]any); ok {
			if checkout, ok := links["checkout"].(map[string]any); ok {
				checkoutURL = tools.OptionalString(checkout, "href", "")
			}
		}
	default:
		checkoutURL = tools.OptionalString(data, "checkout_url", "")
		if checkoutURL == "" {
			checkoutURL = tools.OptionalString(data, "url", "")
		}
		externalID = tools.OptionalString(data, "id", "")
		if externalID == "" {
			externalID = tools.OptionalString(data, "payment_id", "")
		}
	}
	return
}

func parseSubscriptionStatus(providerType string, data map[string]any) string {
	s, _ := data["status"].(string)
	if s == "" {
		return ""
	}
	switch providerType {
	case "stripe":
		switch s {
		case "active", "past_due", "canceled", "trialing":
			return s
		}
		return s
	}
	return ""
}

func parseSubscriptionPeriod(providerType string, data map[string]any) (start, end time.Time) {
	switch providerType {
	case "stripe":
		if ps, ok := data["current_period_start"].(float64); ok {
			start = time.Unix(int64(ps), 0)
		}
		if pe, ok := data["current_period_end"].(float64); ok {
			end = time.Unix(int64(pe), 0)
		}
	}
	return
}

// metadataToJSON serializes a free-form metadata map into a JSON string with a
// default empty-object fallback. Every payments action accepts a metadata arg
// and persists it as text - centralized here so the encoding stays consistent.
func metadataToJSON(m map[string]any) string {
	if m == nil {
		return "{}"
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(data)
}
