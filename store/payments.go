package store

import (
	"database/sql"
	"strings"
	"time"

	"openfabrica/db"
)

const maxPaymentOverviewRows = 200

// PaymentRow represents a row in of_payments.
type PaymentRow struct {
	ID          int       `json:"id"`
	ExternalID  string    `json:"external_id"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// PlanRow represents a row in of_subscription_plans.
type PlanRow struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Interval  string `json:"interval"`
	TrialDays int    `json:"trial_days"`
	IsActive  bool   `json:"is_active"`
}

// SubscriptionRow represents a row in of_subscriptions joined with its plan name.
type SubscriptionRow struct {
	ID               int        `json:"id"`
	UserIDOut        *int64     `json:"user_id"`
	PlanName         string     `json:"plan"`
	ExternalID       string     `json:"external_id"`
	Status           string     `json:"status"`
	CurrentPeriodEnd *time.Time `json:"current_period_end"`
	CanceledAt       *time.Time `json:"canceled_at"`
	CreatedAt        time.Time  `json:"created_at"`
}

// PaymentTotal represents an aggregated status+currency row from of_payments.
type PaymentTotal struct {
	Status   string `json:"status"`
	Currency string `json:"currency"`
	Count    int    `json:"count"`
	Amount   int64  `json:"amount"`
}

// LoadPaymentsOverview queries payment config, totals, payments, plans, and subscriptions.
func LoadPaymentsOverview(pdb *db.ProjectDB) map[string]any {
	totals := []PaymentTotal{}
	payments := []PaymentRow{}
	plans := []PlanRow{}
	subs := []SubscriptionRow{}

	configured := false
	var providerType, currency string
	if err := pdb.QueryRow(`SELECT provider_type, currency FROM of_payment_config WHERE id = 1`).Scan(&providerType, &currency); err == nil {
		configured = true
	}

	if rows, err := pdb.Query(`SELECT status, currency, COUNT(*), COALESCE(SUM(amount), 0) FROM of_payments GROUP BY status, currency ORDER BY status, currency`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var t PaymentTotal
			if rows.Scan(&t.Status, &t.Currency, &t.Count, &t.Amount) == nil {
				totals = append(totals, t)
			}
		}
		_ = rows.Err()
	}

	if rows, err := pdb.Query(
		`SELECT id, COALESCE(external_id,''), amount, currency, status, COALESCE(description,''), created_at
		 FROM of_payments ORDER BY id DESC LIMIT ?`, maxPaymentOverviewRows,
	); err == nil {
		defer rows.Close()
		for rows.Next() {
			var p PaymentRow
			if rows.Scan(&p.ID, &p.ExternalID, &p.Amount, &p.Currency, &p.Status, &p.Description, &p.CreatedAt) == nil {
				payments = append(payments, p)
			}
		}
		_ = rows.Err()
	}

	if rows, err := pdb.Query(
		`SELECT id, name, amount, currency, interval, COALESCE(trial_days,0), COALESCE(is_active,1)
		 FROM of_subscription_plans ORDER BY id`,
	); err == nil {
		defer rows.Close()
		for rows.Next() {
			var p PlanRow
			if rows.Scan(&p.ID, &p.Name, &p.Amount, &p.Currency, &p.Interval, &p.TrialDays, &p.IsActive) == nil {
				plans = append(plans, p)
			}
		}
		_ = rows.Err()
	}

	if rows, err := pdb.Query(
		`SELECT s.id, s.user_id, COALESCE(p.name, ''), COALESCE(s.external_id,''), s.status,
		        s.current_period_end, s.canceled_at, s.created_at
		 FROM of_subscriptions s LEFT JOIN of_subscription_plans p ON p.id = s.plan_id
		 ORDER BY s.id DESC LIMIT ?`, maxPaymentOverviewRows,
	); err == nil {
		defer rows.Close()
		for rows.Next() {
			var s SubscriptionRow
			var userID sql.NullInt64
			var periodEnd, canceled sql.NullTime
			if rows.Scan(&s.ID, &userID, &s.PlanName, &s.ExternalID, &s.Status, &periodEnd, &canceled, &s.CreatedAt) != nil {
				continue
			}
			if userID.Valid {
				id := userID.Int64
				s.UserIDOut = &id
			}
			if periodEnd.Valid {
				s.CurrentPeriodEnd = &periodEnd.Time
			}
			if canceled.Valid {
				s.CanceledAt = &canceled.Time
			}
			s.Status = strings.ToLower(s.Status)
			subs = append(subs, s)
		}
		_ = rows.Err()
	}

	return map[string]any{
		"configured":    configured,
		"provider_type": providerType,
		"currency":      currency,
		"totals":        totals,
		"payments":      payments,
		"plans":         plans,
		"subscriptions": subs,
	}
}
