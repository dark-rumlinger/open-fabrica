package payments

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"openfabrica/tools"
	"openfabrica/tools/internal/httpclient"
	"openfabrica/tools/internal/provider"
)

// manage_subscription_plans registers subscription pricing tiers backed by the
// configured payments provider. Tiers are reused by manage_subscriptions when
// opening a hosted checkout - create once, subscribe many.
//
// Named "_subscription_plans" rather than "_plans" because a bare "manage_plans"
// reads as project/build planning (the build checklist is manage_tasks), a
// different concept LLMs empirically confused it with.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_subscription_plans",
		Category: "payments",
		OneLiner: "Manage subscription pricing tiers backed by the configured payment provider.",
		WhenToUse: `Create a plan once, then subscribe users to it via manage_subscriptions. Plans
are tied to the provider configured on manage_payments. For one-shot payments
(no recurrence), a function handler calls ctx.checkout with an amount instead of a plan_id.`,
		Actions: []tools.Action{
			&PlanCreate{},
			&PlanList{},
		},
		Hints: func(action string, _ map[string]any, r *tools.Result) string {
			if action == "create" && r != nil && r.Success {
				return "Subscription flow: create plan -> manage_subscriptions create (returns checkout_url). Lifecycle events (renewals, cancellations) arrive on the platform's POST /payments/webhook route, which the provider's dashboard must point at."
			}
			return ""
		},
	})
}

// PlanCreate creates a recurring price on the configured provider and stores
// it in of_subscription_plans for later use by manage_subscriptions.
type PlanCreate struct {
	Name      string         `arg:"name,required"     desc:"Plan name."`
	Amount    int            `arg:"amount,required"   desc:"Recurring amount in cents."`
	Currency  string         `arg:"currency"          desc:"ISO code; defaults to the payments configuration."`
	Interval  string         `arg:"interval"          desc:"Billing interval." enum:"month,year,week" default:"month"`
	TrialDays int            `arg:"trial_days"        desc:"Free trial length in days."`
	Metadata  map[string]any `arg:"metadata"          desc:"Free-form metadata."`
}

func (a *PlanCreate) ActionName() string { return "create" }
func (a *PlanCreate) Doc() string        { return "Create a recurring price." }
func (a *PlanCreate) Example() any {
	return &PlanCreate{Name: "Pro Monthly", Amount: 1999, Interval: "month"}
}
func (a *PlanCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if a.Amount <= 0 {
		return tools.Fail("amount (in cents) is required and must be positive"), nil
	}
	cfg, errR := paymentLoadConfig(ctx)
	if errR != nil {
		return errR, nil
	}
	currency := a.Currency
	if currency == "" {
		currency = cfg.currency
	}
	metadataJSON := metadataToJSON(a.Metadata)

	var externalID string

	switch cfg.providerType {
	case "stripe":
		creds, errR := provider.Load(ctx, cfg.providerName)
		if errR != nil {
			return errR, nil
		}
		reqURL := strings.TrimRight(creds.BaseURL, "/") + "/v1/prices"
		// url.Values encodes each value: a raw Sprintf body sent "Pro Monthly" with a
		// bare space and let an "&" in a name inject extra form fields.
		form := url.Values{}
		form.Set("unit_amount", strconv.Itoa(a.Amount))
		form.Set("currency", currency)
		form.Set("recurring[interval]", a.Interval)
		form.Set("product_data[name]", a.Name)
		if a.TrialDays > 0 {
			form.Set("recurring[trial_period_days]", strconv.Itoa(a.TrialDays))
		}
		id, errR := providerPostJSONID(ctx, creds, reqURL, form.Encode(), "application/x-www-form-urlencoded")
		if errR != nil {
			return errR, nil
		}
		externalID = id

	default:
		// Only Stripe subscription events are reconciled, so a plan on any other provider
		// would sell a subscription whose fulfilment never fires.
		return tools.FailCode(tools.CodeBadArg, "subscription plans need a stripe payment provider (configured: %s); one-off payments work with the others", cfg.providerType), nil
	}

	result, err := ctx.DB.Exec(
		`INSERT INTO of_subscription_plans (name, external_id, amount, currency, interval, trial_days, metadata)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.Name, externalID, a.Amount, currency, a.Interval, a.TrialDays, metadataJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("storing subscription plan: %w", err)
	}
	planID, _ := result.LastInsertId()

	return tools.Ok(map[string]any{
		"plan_id":     planID,
		"name":        a.Name,
		"external_id": externalID,
		"amount":      a.Amount,
		"currency":    currency,
		"interval":    a.Interval,
		"trial_days":  a.TrialDays,
	}), nil
}

// PlanList returns every active plan ordered by creation time descending.
type PlanList struct{}

func (a *PlanList) ActionName() string { return "list" }
func (a *PlanList) Doc() string        { return "List every defined plan." }
func (a *PlanList) ReadOnly() bool     { return true }
func (a *PlanList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		"SELECT id, name, external_id, amount, currency, interval, trial_days, is_active, created_at FROM of_subscription_plans WHERE is_active = 1 ORDER BY created_at DESC",
	)
	if err != nil {
		return nil, fmt.Errorf("listing plans: %w", err)
	}
	defer rows.Close()
	var plans []map[string]any
	for rows.Next() {
		var id, amount, trialDays int
		var isActive bool
		var name, externalID, currency, interval string
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &externalID, &amount, &currency, &interval, &trialDays, &isActive, &createdAt); err != nil {
			continue
		}
		plans = append(plans, map[string]any{
			"id": id, "name": name, "external_id": externalID,
			"amount": amount, "currency": currency, "interval": interval,
			"trial_days": trialDays, "is_active": isActive, "created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing plans: %w", err)
	}
	return tools.Ok(plans), nil
}

// providerPostJSONID is the thin payments-side wrapper around
// httpclient.PostWithAuth that decodes the response, pulls a required string
// "id" field, and returns it. Stripe/PayPal plan and subscription creation
// flows share this pattern.
func providerPostJSONID(ctx *tools.ToolContext, creds *provider.Creds, url, body, contentType string) (string, *tools.Result) {
	respBody, _, errR := httpclient.PostWithAuth(ctx, creds, url, body, contentType, "")
	if errR != nil {
		return "", errR
	}
	var respData map[string]any
	if err := json.Unmarshal(respBody, &respData); err != nil {
		return "", tools.Fail("invalid JSON from provider: %v", err)
	}
	id, idErr := requireStringField(respData, "id", "Provider")
	if idErr != nil {
		return "", tools.Fail("%s", idErr.Error())
	}
	return id, nil
}
