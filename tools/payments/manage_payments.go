package payments

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"openfabrica/billing"
	"openfabrica/internal/replayoutbox"
	"openfabrica/tools"
	"openfabrica/tools/internal/httpclient"
	"openfabrica/tools/internal/provider"
)

// manage_payments wires up the payments provider, opens hosted checkouts,
// and reconciles inbound provider webhooks. Card data never touches the app:
// the provider hosts the form, we just store ids and statuses.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_payments",
		Category: "payments",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult: 8000,
		OneLiner:  "Configure payment providers and open hosted checkout - no card data stored locally.",
		WhenToUse: `Use when the plan calls for taking money. Build to the payment-flow contract
(carried by the payments guide, auto-loaded with this tool) - it owns the
checkout-and-fulfil flow. In short: configure the provider; the app's buyers open
checkout through a manage_function handler's ctx.checkout (create_checkout is the
build-time twin, for a test session). The platform already serves the provider's callback at
POST /payments/webhook (point the provider's dashboard there; do NOT build your
own webhook route): it verifies the signature, updates the local row, and raises
payment.completed / payment.failed for a manage_triggers hook to fulfil on.
handle_webhook is the same verify-and-reconcile step for a payload you hold.
Only Stripe events are reconciled; PayPal and Mollie checkouts open but stay
pending.`,
		Actions: []tools.Action{
			&PaymentConfigure{},
			&PaymentCreateCheckout{},
			&PaymentCheckStatus{},
			&PaymentList{},
			&PaymentHandleWebhook{},
		},
		Hints: func(action string, _ map[string]any, r *tools.Result) string {
			if action == "configure" && r != nil && r.Success {
				return "Client flow: form submit -> POST a manage_function endpoint whose handler calls ctx.checkout -> redirect to checkout_url -> provider handles payment -> success_url. Fire side effects on the payment.completed event."
			}
			return ""
		},
	})
}

// PaymentConfigure binds payments to a registered service provider, currency,
// and optional webhook secret. Creates/updates the singleton of_payment_config row.
type PaymentConfigure struct {
	ProviderName      string `arg:"provider_name,required" desc:"Must match a registered provider name."`
	ProviderType      string `arg:"provider_type"          desc:"Provider family. Only 'stripe' is reconciled automatically (paypal, mollie and generic payments stay pending)." enum:"stripe,paypal,mollie,generic" default:"stripe"`
	Currency          string `arg:"currency"               desc:"ISO code, e.g. 'usd', 'eur'." default:"usd"`
	WebhookSecretName string `arg:"webhook_secret_name,required" desc:"Required. Secrets-store name holding the provider's webhook signing key (create it with manage_secrets first). Inbound webhooks are rejected without it."`
}

func (a *PaymentConfigure) Doc() string {
	return "Bind payments to a registered provider, currency, and webhook secret."
}
func (a *PaymentConfigure) Example() any {
	return &PaymentConfigure{ProviderName: "stripe", ProviderType: "stripe", Currency: "usd", WebhookSecretName: "stripe_webhook_secret"}
}
func (a *PaymentConfigure) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if r := provider.Require(ctx, a.ProviderName); r != nil {
		return r, nil
	}

	// A webhook secret is mandatory: without it, inbound provider webhooks
	// cannot be authenticated and handle_webhook will refuse to reconcile. Bind
	// it here so payments can never be left in an unverifiable state. The secret
	// must already exist in the secrets store.
	a.WebhookSecretName = strings.TrimSpace(a.WebhookSecretName)
	if a.WebhookSecretName == "" {
		return tools.Fail("webhook_secret_name is required: create the provider's webhook signing secret with manage_secrets first, then pass its name here"), nil
	}
	var secretExists int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_secrets WHERE name = ?", a.WebhookSecretName).Scan(&secretExists); err != nil || secretExists == 0 {
		return tools.Fail("webhook secret %q not found in the secrets store; create it with manage_secrets before configuring payments", a.WebhookSecretName), nil
	}

	_, err := ctx.DB.Exec(
		`INSERT INTO of_payment_config (id, provider_name, provider_type, currency, webhook_secret_name)
		 VALUES (1, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   provider_name = excluded.provider_name,
		   provider_type = excluded.provider_type,
		   currency = excluded.currency,
		   webhook_secret_name = excluded.webhook_secret_name`,
		a.ProviderName, a.ProviderType, a.Currency, a.WebhookSecretName,
	)
	if err != nil {
		return nil, fmt.Errorf("configuring payments: %w", err)
	}
	// The webhook URL rides the configure result because this is the moment it
	// becomes actionable, and nothing else surfaces it: the path is fixed by the
	// platform, so it is not discoverable from the project's own artifacts, and a
	// provider pointed nowhere reconciles nothing while every gate stays green.
	return tools.Ok(map[string]any{
		"provider":    a.ProviderName,
		"type":        a.ProviderType,
		"currency":    a.Currency,
		"webhook_url": billing.WebhookPath,
		"next":        "Register " + billing.WebhookPath + " as the webhook endpoint in the provider's dashboard, signed with the secret named " + a.WebhookSecretName + ". Until that is done the provider sends nothing, no payment.completed is raised, and fulfillment never runs.",
	}), nil
}

// PaymentCreateCheckout opens a one-shot hosted-checkout session on the
// configured provider and stores the local payment row for reconciliation.
type PaymentCreateCheckout struct {
	Amount      int            `arg:"amount,required"      desc:"Amount in cents (must be positive)."`
	Description string         `arg:"description"          desc:"Line item description."`
	SuccessURL  string         `arg:"success_url,required" desc:"Redirect URL on success."`
	CancelURL   string         `arg:"cancel_url,required"  desc:"Redirect URL on cancel."`
	Currency    string         `arg:"currency"             desc:"Override the configured currency for this checkout. Omit to use the configured one."`
	Metadata    map[string]any `arg:"metadata"             desc:"Free-form metadata stored with the payment."`
}

func (a *PaymentCreateCheckout) Doc() string {
	return "Create a hosted-checkout session and get back its URL."
}
func (a *PaymentCreateCheckout) Example() any {
	return &PaymentCreateCheckout{Amount: 1999, Description: "Annual plan", SuccessURL: "https://example.com/ok", CancelURL: "https://example.com/cancel"}
}
func (a *PaymentCreateCheckout) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if a.Amount <= 0 {
		return tools.Fail("amount (in cents) is required and must be positive"), nil
	}
	cfg, errR := paymentLoadConfig(ctx)
	if errR != nil {
		return errR, nil
	}
	if !checkoutTypeSupported(cfg.providerType) {
		return tools.Fail("configured provider_type %q is not supported; reconfigure with one of stripe, paypal, mollie, or generic", cfg.providerType), nil
	}
	currency := a.Currency
	if currency == "" {
		currency = cfg.currency
	}
	metadataJSON := metadataToJSON(a.Metadata)

	creds, errR := provider.Load(ctx, cfg.providerName)
	if errR != nil {
		return errR, nil
	}

	var checkoutURL, externalID string
	if replayoutbox.Active(ctx.ProjectID) {
		// A user-flow replay pays on the platform's stub checkout page, which
		// settles through the same path a verified webhook takes (see
		// billing.SettleFlowCheckout): no real session, no page on another domain.
		externalID = billing.FlowCheckoutPrefix + randomHex(12)
		checkoutURL = "/__of/checkout/" + externalID
		billing.RememberFlowCheckout(externalID, billing.FlowCheckout{ProjectID: ctx.ProjectID, Amount: a.Amount,
			Currency: currency, Description: a.Description, SuccessURL: a.SuccessURL, CancelURL: a.CancelURL})
	} else {
		reqURL, reqBody, contentType := buildCheckoutRequest(cfg.providerType, creds.BaseURL, currency, a.Amount, a.Description, a.SuccessURL, a.CancelURL, a.Metadata)

		respBody, _, errR := httpclient.PostWithAuth(ctx, creds, reqURL, reqBody, contentType, "")
		if errR != nil {
			return errR, nil
		}

		var respData map[string]any
		if err := json.Unmarshal(respBody, &respData); err != nil {
			return tools.Fail("invalid JSON response from payment provider: %v", err), nil
		}
		checkoutURL, externalID = parseCheckoutResponse(cfg.providerType, respData)
	}

	result, err := ctx.DB.Exec(
		`INSERT INTO of_payments (external_id, amount, currency, status, description, metadata, checkout_url)
		 VALUES (?, ?, ?, 'pending', ?, ?, ?)`,
		externalID, a.Amount, currency, a.Description, metadataJSON, checkoutURL,
	)
	if err != nil {
		return nil, fmt.Errorf("storing payment: %w", err)
	}
	paymentID, _ := result.LastInsertId()

	return tools.Ok(map[string]any{
		"payment_id":   paymentID,
		"external_id":  externalID,
		"checkout_url": checkoutURL,
		"amount":       a.Amount,
		"currency":     currency,
	}), nil
}

// checkoutTypeSupported reports whether buildCheckoutRequest implements
// providerType natively. "generic" is supported via the default JSON shape;
// any other value (e.g. a stale config row) is rejected by the checkout path
// rather than silently posting the generic body under a provider's name.
func checkoutTypeSupported(providerType string) bool {
	switch providerType {
	case "stripe", "paypal", "mollie", "generic":
		return true
	}
	return false
}

// buildCheckoutRequest assembles the URL/body/content-type tuple for a
// hosted-checkout POST. Per-provider quirks (Stripe form-encoded, PayPal JSON
// with billing cycles, Mollie's redirectUrl alias) are encapsulated here to
// keep PaymentCreateCheckout.Run readable.
func buildCheckoutRequest(providerType, baseURL, currency string, amount int, description, successURL, cancelURL string, metadata map[string]any) (reqURL, reqBody, contentType string) {
	switch providerType {
	case "stripe":
		reqURL = strings.TrimRight(baseURL, "/") + "/v1/checkout/sessions"
		contentType = "application/x-www-form-urlencoded"
		// Form-encode every field. A success/cancel URL carrying its own query
		// string, or a description containing & or =, must not break the body
		// or inject additional Stripe parameters.
		form := url.Values{}
		form.Set("mode", "payment")
		form.Set("success_url", successURL)
		form.Set("cancel_url", cancelURL)
		form.Set("line_items[0][price_data][currency]", currency)
		form.Set("line_items[0][price_data][unit_amount]", fmt.Sprintf("%d", amount))
		form.Set("line_items[0][price_data][product_data][name]", description)
		form.Set("line_items[0][quantity]", "1")
		reqBody = form.Encode()
	case "paypal":
		reqURL = strings.TrimRight(baseURL, "/") + "/v2/checkout/orders"
		contentType = "application/json"
		amountStr := fmt.Sprintf("%.2f", float64(amount)/100.0)
		payload := map[string]any{
			"intent": "CAPTURE",
			"purchase_units": []map[string]any{{
				"amount": map[string]string{
					"currency_code": strings.ToUpper(currency),
					"value":         amountStr,
				},
				"description": description,
			}},
			"application_context": map[string]string{
				"return_url": successURL,
				"cancel_url": cancelURL,
			},
		}
		data, _ := json.Marshal(payload)
		reqBody = string(data)
	case "mollie":
		reqURL = strings.TrimRight(baseURL, "/") + "/v2/payments"
		contentType = "application/json"
		amountStr := fmt.Sprintf("%.2f", float64(amount)/100.0)
		payload := map[string]any{
			"amount":      map[string]string{"currency": strings.ToUpper(currency), "value": amountStr},
			"description": description,
			"redirectUrl": successURL,
			"cancelUrl":   cancelURL,
		}
		data, _ := json.Marshal(payload)
		reqBody = string(data)
	default:
		reqURL = strings.TrimRight(baseURL, "/") + "/payments"
		contentType = "application/json"
		payload := map[string]any{
			"amount":      amount,
			"currency":    currency,
			"description": description,
			"success_url": successURL,
			"cancel_url":  cancelURL,
			"metadata":    metadata,
		}
		data, _ := json.Marshal(payload)
		reqBody = string(data)
	}
	return
}

// PaymentCheckStatus reads a single payment's state from the local table. Provide
// either payment_id (local) or external_id (provider's id).
type PaymentCheckStatus struct {
	PaymentID  int64  `arg:"payment_id"  desc:"Local payment row id. One of payment_id or external_id is required."`
	ExternalID string `arg:"external_id" desc:"Provider's session/payment id. One of payment_id or external_id is required."`
}

func (a *PaymentCheckStatus) Doc() string {
	return "Read a payment's status from the local payments row (it does not ask the provider). Build-time only: the running app reads its own order rows instead."
}
func (a *PaymentCheckStatus) ReadOnly() bool { return true }
func (a *PaymentCheckStatus) Example() any {
	return &PaymentCheckStatus{PaymentID: 1}
}
func (a *PaymentCheckStatus) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var status, externalID, currency, description string
	var amount int
	var paymentID = a.PaymentID
	switch {
	case a.PaymentID > 0:
		err := ctx.DB.QueryRow(
			"SELECT status, external_id, amount, currency, description FROM of_payments WHERE id = ?",
			a.PaymentID,
		).Scan(&status, &externalID, &amount, &currency, &description)
		if err != nil {
			return tools.NotFound("payment id", fmt.Sprint(a.PaymentID)), nil
		}
	case a.ExternalID != "":
		externalID = a.ExternalID
		err := ctx.DB.QueryRow(
			"SELECT id, status, amount, currency, description FROM of_payments WHERE external_id = ?",
			a.ExternalID,
		).Scan(&paymentID, &status, &amount, &currency, &description)
		if err != nil {
			return tools.NotFound("payment with external_id", a.ExternalID), nil
		}
	default:
		return tools.Fail("payment_id or external_id is required"), nil
	}
	return tools.Ok(map[string]any{
		"payment_id":  paymentID,
		"external_id": externalID,
		"status":      status,
		"amount":      amount,
		"currency":    currency,
		"description": description,
	}), nil
}

// PaymentList returns recent payment rows, optionally filtered by status.
type PaymentList struct {
	Status string `arg:"status" desc:"Filter by payment status (e.g. pending, paid, failed, expired)."`
	Limit  int    `arg:"limit"  desc:"Max rows returned (maximum 500)." default:"50" max:"500"`
	Offset int    `arg:"offset" desc:"Pagination offset."`
}

func (a *PaymentList) Doc() string    { return "List local payment rows, optionally filtered by status." }
func (a *PaymentList) ReadOnly() bool { return true }
func (a *PaymentList) Example() any {
	return &PaymentList{Status: "paid"}
}
func (a *PaymentList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	limit := a.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := a.Offset
	if offset < 0 {
		offset = 0
	}

	query := "SELECT id, external_id, amount, currency, status, description, created_at FROM of_payments"
	var queryArgs []any
	if a.Status != "" {
		query += " WHERE status = ?"
		queryArgs = append(queryArgs, a.Status)
	}
	query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	queryArgs = append(queryArgs, limit, offset)

	rows, err := ctx.DB.Query(query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("listing payments: %w", err)
	}
	defer rows.Close()

	var payments []map[string]any
	for rows.Next() {
		var id, amount int
		var externalID, currency, status, description string
		var createdAt time.Time
		if err := rows.Scan(&id, &externalID, &amount, &currency, &status, &description, &createdAt); err != nil {
			continue
		}
		payments = append(payments, map[string]any{
			"id": id, "external_id": externalID, "amount": amount,
			"currency": currency, "status": status, "description": description,
			"created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing payments: %w", err)
	}
	return tools.Ok(payments), nil
}

// PaymentHandleWebhook verifies a provider webhook signature and reconciles
// the local payment or subscription row implied by the event.
type PaymentHandleWebhook struct {
	WebhookBody      string `arg:"webhook_body,required" desc:"Raw webhook request body."`
	WebhookSignature string `arg:"webhook_signature,required" desc:"Provider's signature header value (the payload is refused unverified without it)."`
}

func (a *PaymentHandleWebhook) Doc() string {
	return "Verify a provider webhook signature and reconcile the local row."
}
func (a *PaymentHandleWebhook) Example() any {
	return &PaymentHandleWebhook{WebhookBody: "{...}", WebhookSignature: "t=...,v1=..."}
}
func (a *PaymentHandleWebhook) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	out, err := billing.ReconcileWebhook(billing.WebhookEnv{
		DB: ctx.DB, Encryptor: ctx.Encryptor, Bus: ctx.Bus,
		Logger: ctx.Logger, ProjectID: ctx.ProjectID,
	}, a.WebhookBody, a.WebhookSignature)
	if err != nil {
		// A refusal is the sender's problem (unconfigured, unsigned, unverifiable),
		// so it is a reportable result rather than a tool failure.
		var ref *billing.WebhookRefusal
		if errors.As(err, &ref) {
			return tools.Fail("%s", ref.Msg), nil
		}
		return nil, err
	}
	switch out.Kind {
	case billing.WebhookDuplicate:
		return tools.Ok(map[string]any{"status": "already_processed", "event_key": out.EventKey}), nil
	case billing.WebhookIgnored:
		return tools.Ok(map[string]any{"status": "ignored", "external_id": out.ExternalID}), nil
	case billing.WebhookSubscription:
		return tools.Ok(map[string]any{"type": "subscription", "external_id": out.ExternalID, "status": out.Status}), nil
	}
	return tools.Ok(map[string]any{"type": "payment", "external_id": out.ExternalID, "status": out.Status}), nil
}

// randomHex returns n random bytes as hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
