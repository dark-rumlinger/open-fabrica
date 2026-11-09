package payments

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"openfabrica/tools"
	"openfabrica/tools/internal/httpclient"
	"openfabrica/tools/internal/provider"
)

// manage_subscriptions registers recurring subscription lifecycle actions:
// open hosted checkout for a plan, cancel, list, and reconcile with the
// configured provider. Renewals echo back through the platform's /payments/webhook route.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_subscriptions",
		Category: "payments",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult: 8000,
		OneLiner:  "Manage recurring subscriptions - hosted checkout, list, cancel, reconcile.",
		WhenToUse: `Use create to open a hosted checkout for an existing pricing tier (define
those first via manage_subscription_plans); list = local state, check = provider refresh;
cancel to end one IMMEDIATELY (not at period end). Renewals and cancellation echoes from
the provider arrive on the platform's POST /payments/webhook route (point the
provider's dashboard there), not through this tool.`,
		Actions: []tools.Action{
			&SubscriptionCreate{},
			&SubscriptionCancel{},
			&SubscriptionList{},
			&SubscriptionCheck{},
		},
	})
}

// SubscriptionCreate opens a hosted checkout to subscribe a user to a plan.
type SubscriptionCreate struct {
	PlanID        int            `arg:"plan_id,required"     desc:"Plan to subscribe to."`
	SuccessURL    string         `arg:"success_url,required" desc:"Redirect URL on success."`
	CancelURL     string         `arg:"cancel_url,required"  desc:"Redirect URL on cancel."`
	CustomerEmail string         `arg:"customer_email"       desc:"Email passed to the provider."`
	UserID        int            `arg:"user_id"              desc:"Local user id stored with the subscription."`
	Metadata      map[string]any `arg:"metadata"             desc:"Free-form metadata."`
}

func (a *SubscriptionCreate) Doc() string {
	return "Open a hosted checkout to subscribe a customer to a plan."
}
func (a *SubscriptionCreate) Example() any {
	return &SubscriptionCreate{PlanID: 1, SuccessURL: "https://example.com/ok", CancelURL: "https://example.com/cancel"}
}
func (a *SubscriptionCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	metadataJSON := metadataToJSON(a.Metadata)

	var planName, planExternalID, planCurrency, planInterval string
	var planAmount, planTrialDays int
	err := ctx.DB.QueryRow(
		"SELECT name, external_id, amount, currency, interval, trial_days FROM of_subscription_plans WHERE id = ? AND is_active = 1",
		a.PlanID,
	).Scan(&planName, &planExternalID, &planAmount, &planCurrency, &planInterval, &planTrialDays)
	if err != nil {
		return tools.Fail("plan not found or inactive"), nil
	}

	cfg, errR := paymentLoadConfig(ctx)
	if errR != nil {
		return errR, nil
	}

	var checkoutURL, externalID string

	switch cfg.providerType {
	case "stripe":
		creds, errR := provider.Load(ctx, cfg.providerName)
		if errR != nil {
			return errR, nil
		}
		if planExternalID == "" {
			return tools.Fail("plan has no external_id, recreate the plan with a Stripe-configured provider"), nil
		}
		reqURL := strings.TrimRight(creds.BaseURL, "/") + "/v1/checkout/sessions"
		// Form-encode every field so a success/cancel URL with its own query
		// string, or a customer email, cannot break the body or inject extra
		// Stripe parameters.
		form := url.Values{}
		form.Set("mode", "subscription")
		form.Set("line_items[0][price]", planExternalID)
		form.Set("line_items[0][quantity]", "1")
		form.Set("success_url", a.SuccessURL)
		form.Set("cancel_url", a.CancelURL)
		if a.CustomerEmail != "" {
			form.Set("customer_email", a.CustomerEmail)
		}
		if planTrialDays > 0 {
			form.Set("subscription_data[trial_period_days]", fmt.Sprintf("%d", planTrialDays))
		}
		reqBody := form.Encode()
		respData, errR := postProviderJSON(ctx, creds, reqURL, reqBody, "application/x-www-form-urlencoded")
		if errR != nil {
			return errR, nil
		}
		checkoutURL = tools.OptionalString(respData, "url", "")
		externalID = tools.OptionalString(respData, "subscription", "")
		if externalID == "" {
			externalID = tools.OptionalString(respData, "id", "")
		}
		if externalID == "" {
			return tools.Fail("Stripe response missing subscription/session id"), nil
		}
		if checkoutURL == "" {
			return tools.Fail("Stripe response missing checkout url"), nil
		}

	default:
		return tools.Fail("subscriptions require a stripe payment provider"), nil
	}

	now := time.Now()
	var userIDVal any
	if a.UserID > 0 {
		userIDVal = a.UserID
	}
	result, err := ctx.DB.Exec(
		`INSERT INTO of_subscriptions (user_id, plan_id, external_id, status, current_period_start, metadata)
		 VALUES (?, ?, ?, 'pending', ?, ?)`,
		userIDVal, a.PlanID, externalID, now, metadataJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("storing subscription: %w", err)
	}
	subscriptionID, _ := result.LastInsertId()

	return tools.Ok(map[string]any{
		"subscription_id": subscriptionID,
		"external_id":     externalID,
		"checkout_url":    checkoutURL,
		"plan_id":         a.PlanID,
		"status":          "pending",
	}), nil
}

// stripeSessionPrefix marks a checkout session id, stored until the completion
// webhook replaces it with the subscription id.
const stripeSessionPrefix = "cs_"

// closeStripeSession expires a still-open checkout session. A session that
// already expired needs nothing; one that completed has a live subscription, so
// it fails rather than orphaning it.
func closeStripeSession(ctx *tools.ToolContext, creds *provider.Creds, sessionID string) *tools.Result {
	base := strings.TrimRight(creds.BaseURL, "/") + "/v1/checkout/sessions/" + sessionID
	getReq, err := http.NewRequest("GET", base, nil)
	if err != nil {
		return tools.Fail("creating session request: %v", err)
	}
	body, _, errR := httpclient.DoWithAuth(ctx, getReq, creds, "")
	if errR != nil {
		return errR
	}
	var sess map[string]any
	if json.Unmarshal(body, &sess) != nil {
		return tools.Fail("invalid JSON from provider")
	}
	switch tools.OptionalString(sess, "status", "") {
	case "open":
		expReq, err := http.NewRequest("POST", base+"/expire", nil)
		if err != nil {
			return tools.Fail("creating expire request: %v", err)
		}
		if _, _, errR := httpclient.DoWithAuth(ctx, expReq, creds, ""); errR != nil {
			return errR
		}
	case "complete":
		return tools.Fail("checkout already completed: the subscription id arrives with its webhook, cancel again once the status is no longer pending")
	}
	return nil
}

// SubscriptionCancel terminates a subscription on the provider and marks it
// canceled locally. Idempotent: a second cancel returns "already canceled".
type SubscriptionCancel struct {
	SubscriptionID int `arg:"subscription_id,required" desc:"Local subscription id."`
}

func (a *SubscriptionCancel) Doc() string {
	return "Cancel a subscription immediately, not at period end. It is cancelled on Stripe and marked canceled locally."
}
func (a *SubscriptionCancel) Example() any { return &SubscriptionCancel{SubscriptionID: 1} }
func (a *SubscriptionCancel) Run(ctx *tools.ToolContext) (*tools.Result, error) {

	var externalID, status string
	err := ctx.DB.QueryRow(
		"SELECT external_id, status FROM of_subscriptions WHERE id = ?", a.SubscriptionID,
	).Scan(&externalID, &status)
	if err != nil {
		return tools.NotFound("subscription id", fmt.Sprint(a.SubscriptionID)), nil
	}
	if status == "canceled" {
		return tools.Fail("subscription is already canceled"), nil
	}

	cfg, errR := paymentLoadConfig(ctx)
	if errR != nil {
		return errR, nil
	}

	if externalID != "" {
		creds, errR := provider.Load(ctx, cfg.providerName)
		if errR != nil {
			return errR, nil
		}
		var req *http.Request
		var reqErr error
		switch cfg.providerType {
		case "stripe":
			if strings.HasPrefix(externalID, stripeSessionPrefix) {
				// Checkout not completed yet: there is no subscription to delete,
				// only a session to close.
				if errR := closeStripeSession(ctx, creds, externalID); errR != nil {
					return errR, nil
				}
				break
			}
			reqURL := strings.TrimRight(creds.BaseURL, "/") + "/v1/subscriptions/" + externalID
			req, reqErr = http.NewRequest("DELETE", reqURL, nil)
		}
		if reqErr != nil {
			return tools.Fail("creating cancel request: %v", reqErr), nil
		}
		if req != nil {
			if _, _, errR := httpclient.DoWithAuth(ctx, req, creds, ""); errR != nil {
				return errR, nil
			}
		}
	}

	_, err = ctx.DB.Exec(
		"UPDATE of_subscriptions SET status = 'canceled', canceled_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		a.SubscriptionID,
	)
	if err != nil {
		return nil, fmt.Errorf("updating subscription: %w", err)
	}

	return tools.Ok(map[string]any{
		"subscription_id": a.SubscriptionID,
		"status":          "canceled",
	}), nil
}

// SubscriptionList enumerates local subscriptions with optional filters and
// joins the plan row so the LLM sees pricing context without a second call.
type SubscriptionList struct {
	Status string `arg:"status" desc:"Filter by subscription status."`
	UserID int    `arg:"user_id" desc:"Filter by local user id."`
	Limit  int    `arg:"limit"  desc:"Max rows returned (maximum 500)." default:"50" min:"1" max:"500"`
	Offset int    `arg:"offset" desc:"Pagination offset."`
}

func (a *SubscriptionList) Doc() string {
	return "List local subscriptions, optionally filtered by status."
}
func (a *SubscriptionList) ReadOnly() bool { return true }
func (a *SubscriptionList) Example() any {
	return &SubscriptionList{Status: "active"}
}
func (a *SubscriptionList) Run(ctx *tools.ToolContext) (*tools.Result, error) {

	query := `SELECT s.id, s.user_id, s.plan_id, s.external_id, s.status,
		s.current_period_start, s.current_period_end, s.canceled_at, s.created_at,
		p.name, p.amount, p.currency, p.interval
		FROM of_subscriptions s
		LEFT JOIN of_subscription_plans p ON s.plan_id = p.id
		WHERE 1=1`
	var queryArgs []any
	if a.Status != "" {
		query += " AND s.status = ?"
		queryArgs = append(queryArgs, a.Status)
	}
	if a.UserID > 0 {
		query += " AND s.user_id = ?"
		queryArgs = append(queryArgs, a.UserID)
	}
	query += " ORDER BY s.created_at DESC LIMIT ? OFFSET ?"
	queryArgs = append(queryArgs, a.Limit, a.Offset)

	rows, err := ctx.DB.Query(query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("listing subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []map[string]any
	for rows.Next() {
		var id, planID int
		var userIDVal sql.NullInt64
		var externalID, status string
		var periodStart, periodEnd, canceledAt sql.NullTime
		var createdAt time.Time
		var planName sql.NullString
		var planAmount sql.NullInt64
		var planCurrency, planInterval sql.NullString
		if err := rows.Scan(&id, &userIDVal, &planID, &externalID, &status,
			&periodStart, &periodEnd, &canceledAt, &createdAt,
			&planName, &planAmount, &planCurrency, &planInterval); err != nil {
			continue
		}
		sub := map[string]any{
			"id": id, "plan_id": planID, "external_id": externalID,
			"status": status, "created_at": createdAt,
		}
		if userIDVal.Valid {
			sub["user_id"] = userIDVal.Int64
		}
		if periodStart.Valid {
			sub["current_period_start"] = periodStart.Time
		}
		if periodEnd.Valid {
			sub["current_period_end"] = periodEnd.Time
		}
		if canceledAt.Valid {
			sub["canceled_at"] = canceledAt.Time
		}
		if planName.Valid {
			sub["plan_name"] = planName.String
			sub["plan_amount"] = planAmount.Int64
			sub["plan_currency"] = planCurrency.String
			sub["plan_interval"] = planInterval.String
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing subscriptions: %w", err)
	}
	return tools.Ok(subs), nil
}

// SubscriptionCheck fetches one subscription's local state and reconciles it
// against the provider's current view when a remote id is on file.
type SubscriptionCheck struct {
	SubscriptionID int `arg:"subscription_id,required" desc:"Local subscription id."`
}

func (a *SubscriptionCheck) Doc() string {
	return "Refresh one subscription from the provider (manage_payments check_status reads local state only)."
}
func (a *SubscriptionCheck) Example() any {
	return &SubscriptionCheck{SubscriptionID: 1}
}
func (a *SubscriptionCheck) Run(ctx *tools.ToolContext) (*tools.Result, error) {

	var externalID, status string
	var periodStart, periodEnd, canceledAt sql.NullTime
	var planID int
	err := ctx.DB.QueryRow(
		"SELECT external_id, status, plan_id, current_period_start, current_period_end, canceled_at FROM of_subscriptions WHERE id = ?",
		a.SubscriptionID,
	).Scan(&externalID, &status, &planID, &periodStart, &periodEnd, &canceledAt)
	if err != nil {
		return tools.NotFound("subscription id", fmt.Sprint(a.SubscriptionID)), nil
	}

	// A stored checkout session id has no subscription behind it yet; the
	// completion webhook swaps it for the real one.
	if externalID != "" && !strings.HasPrefix(externalID, stripeSessionPrefix) {
		if cfg, errR := paymentLoadConfig(ctx); errR == nil {
			if creds, errR := provider.Load(ctx, cfg.providerName); errR == nil {
				var req *http.Request
				var reqErr error
				switch cfg.providerType {
				case "stripe":
					req, reqErr = http.NewRequest("GET", strings.TrimRight(creds.BaseURL, "/")+"/v1/subscriptions/"+externalID, nil)
				}
				if reqErr == nil && req != nil {
					respBody, _, errR := httpclient.DoWithAuth(ctx, req, creds, "")
					if errR == nil {
						var respData map[string]any
						if json.Unmarshal(respBody, &respData) == nil {
							// canceled is terminal: never let provider eventual-consistency
							// lag (still reporting active just after a cancel) resurrect a
							// locally canceled subscription.
							if newStatus := parseSubscriptionStatus(cfg.providerType, respData); newStatus != "" && newStatus != status && status != "canceled" {
								status = newStatus
								if _, err := ctx.DB.Exec(
									"UPDATE of_subscriptions SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
									status, a.SubscriptionID,
								); err != nil {
									ctx.Logger.Warn("subscription status update failed", "subscription_id", a.SubscriptionID, "error", err)
								}
							}
							pStart, pEnd := parseSubscriptionPeriod(cfg.providerType, respData)
							if !pStart.IsZero() {
								if _, err := ctx.DB.Exec(
									"UPDATE of_subscriptions SET current_period_start = ?, current_period_end = ? WHERE id = ?",
									pStart, pEnd, a.SubscriptionID,
								); err != nil {
									ctx.Logger.Warn("subscription period update failed", "subscription_id", a.SubscriptionID, "error", err)
								}
								periodStart = sql.NullTime{Time: pStart, Valid: true}
								periodEnd = sql.NullTime{Time: pEnd, Valid: true}
							}
						}
					}
				}
			}
		}
	}

	result := map[string]any{
		"subscription_id": a.SubscriptionID,
		"external_id":     externalID,
		"status":          status,
		"plan_id":         planID,
	}
	if periodStart.Valid {
		result["current_period_start"] = periodStart.Time
	}
	if periodEnd.Valid {
		result["current_period_end"] = periodEnd.Time
	}
	if canceledAt.Valid {
		result["canceled_at"] = canceledAt.Time
	}
	return tools.Ok(result), nil
}

// postProviderJSON sends a POST that returns a JSON body. Stripe flows
// in the subscription create path share this pattern; the helper returns the
// decoded map and a tools.Fail Result on any transport/HTTP-status failure.
// HTTP scaffolding (auth, send, bounded read, status check) is handled by
// tools/internal/httpclient; this wrapper only decodes the response.
func postProviderJSON(ctx *tools.ToolContext, creds *provider.Creds, url, body, contentType string) (map[string]any, *tools.Result) {
	respBody, _, errR := httpclient.PostWithAuth(ctx, creds, url, body, contentType, "")
	if errR != nil {
		return nil, errR
	}
	var respData map[string]any
	if err := json.Unmarshal(respBody, &respData); err != nil {
		return nil, tools.Fail("invalid JSON from provider: %v", err)
	}
	return respData, nil
}
