package admin

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

// ProjectPaymentsHandler gives the owner a read-only view of the money a project
// has taken: payments, subscription plans and the subscriptions people hold. These
// tables are written by the platform's payment flow and its provider webhooks, never
// by hand, and have no other window in the admin (they are not dynamic tables, so
// the Data Tables tab does not list them).
type ProjectPaymentsHandler struct {
	deps *Deps
}

// maxPaymentRows caps each list; the newest rows are the ones an owner looks for.
const maxPaymentRows = 200

type paymentRow struct {
	ID          int       `json:"id"`
	ExternalID  string    `json:"external_id"`
	Amount      int64     `json:"amount"` // minor units (cents)
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type planRow struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Interval  string `json:"interval"`
	TrialDays int    `json:"trial_days"`
	IsActive  bool   `json:"is_active"`
}

type subscriptionRow struct {
	ID               int           `json:"id"`
	UserID           sql.NullInt64 `json:"-"`
	UserIDOut        *int64        `json:"user_id"`
	PlanName         string        `json:"plan"`
	ExternalID       string        `json:"external_id"`
	Status           string        `json:"status"`
	CurrentPeriodEnd *time.Time    `json:"current_period_end"`
	CanceledAt       *time.Time    `json:"canceled_at"`
	CreatedAt        time.Time     `json:"created_at"`
}

type paymentTotal struct {
	Status   string `json:"status"`
	Currency string `json:"currency"`
	Count    int    `json:"count"`
	Amount   int64  `json:"amount"`
}

// Overview returns everything the Payments panel shows in one response.
//
//	GET /admin/api/projects/{projectID}/payments
func (h *ProjectPaymentsHandler) Overview(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	out := map[string]any{
		"configured":    false,
		"provider_type": "",
		"currency":      "",
		"totals":        []paymentTotal{},
		"payments":      []paymentRow{},
		"plans":         []planRow{},
		"subscriptions": []subscriptionRow{},
	}

	var providerType, currency string
	if err := projectDB.QueryRow(`SELECT provider_type, currency FROM of_payment_config WHERE id = 1`).Scan(&providerType, &currency); err == nil {
		out["configured"] = true
		out["provider_type"] = providerType
		out["currency"] = currency
	}

	if rows, err := projectDB.Query(`SELECT status, currency, COUNT(*), COALESCE(SUM(amount), 0) FROM of_payments GROUP BY status, currency ORDER BY status, currency`); err == nil {
		out["totals"] = collectRows(rows, func(rows *sql.Rows) (paymentTotal, error) {
			var t paymentTotal
			err := rows.Scan(&t.Status, &t.Currency, &t.Count, &t.Amount)
			return t, err
		})
	}

	if rows, err := projectDB.Query(
		`SELECT id, COALESCE(external_id,''), amount, currency, status, COALESCE(description,''), created_at
		 FROM of_payments ORDER BY id DESC LIMIT ?`, maxPaymentRows); err == nil {
		out["payments"] = collectRows(rows, func(rows *sql.Rows) (paymentRow, error) {
			var p paymentRow
			err := rows.Scan(&p.ID, &p.ExternalID, &p.Amount, &p.Currency, &p.Status, &p.Description, &p.CreatedAt)
			return p, err
		})
	}

	if rows, err := projectDB.Query(
		`SELECT id, name, amount, currency, interval, COALESCE(trial_days,0), COALESCE(is_active,1)
		 FROM of_subscription_plans ORDER BY id`); err == nil {
		out["plans"] = collectRows(rows, func(rows *sql.Rows) (planRow, error) {
			var p planRow
			err := rows.Scan(&p.ID, &p.Name, &p.Amount, &p.Currency, &p.Interval, &p.TrialDays, &p.IsActive)
			return p, err
		})
	}

	if rows, err := projectDB.Query(
		`SELECT s.id, s.user_id, COALESCE(p.name, ''), COALESCE(s.external_id,''), s.status,
		        s.current_period_end, s.canceled_at, s.created_at
		 FROM of_subscriptions s LEFT JOIN of_subscription_plans p ON p.id = s.plan_id
		 ORDER BY s.id DESC LIMIT ?`, maxPaymentRows); err == nil {
		out["subscriptions"] = collectRows(rows, func(rows *sql.Rows) (subscriptionRow, error) {
			var s subscriptionRow
			var periodEnd, canceled sql.NullTime
			if err := rows.Scan(&s.ID, &s.UserID, &s.PlanName, &s.ExternalID, &s.Status, &periodEnd, &canceled, &s.CreatedAt); err != nil {
				return s, err
			}
			if s.UserID.Valid {
				id := s.UserID.Int64
				s.UserIDOut = &id
			}
			if periodEnd.Valid {
				s.CurrentPeriodEnd = &periodEnd.Time
			}
			if canceled.Valid {
				s.CanceledAt = &canceled.Time
			}
			s.Status = strings.ToLower(s.Status)
			return s, nil
		})
	}

	// collectRows answers nil for no rows, which would serialize as null; the page
	// should never have to tell null from an empty list.
	out["totals"] = orEmpty[paymentTotal](out["totals"])
	out["payments"] = orEmpty[paymentRow](out["payments"])
	out["plans"] = orEmpty[planRow](out["plans"])
	out["subscriptions"] = orEmpty[subscriptionRow](out["subscriptions"])

	writeJSON(w, http.StatusOK, out)
}

// orEmpty returns v as a []T, or an empty (non-nil) slice when it is nil or another type.
func orEmpty[T any](v any) []T {
	if s, ok := v.([]T); ok && s != nil {
		return s
	}
	return []T{}
}
