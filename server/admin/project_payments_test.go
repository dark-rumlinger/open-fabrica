package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The Payments view lists what a project has taken, newest first, for the owner
// only, and never shows the buyer-supplied metadata or the hosted checkout link.
func TestPaymentsOverviewThroughTheRouter(t *testing.T) {
	f := newAdminFlow(t)
	admin := f.login("owner", flowPassword)
	viewer := f.login("viewer", flowPassword)
	pdb, err := f.pm.Create(7)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO of_payment_config (id, provider_name, provider_type, currency) VALUES (1, 'stripe', 'stripe', 'eur')`,
		`INSERT INTO of_payments (external_id, amount, currency, status, description, metadata, checkout_url) VALUES
			('cs_1', 1999, 'eur', 'paid', 'Pro plan', '{"email":"buyer@example.com"}', 'https://pay.example/c/secret-link'),
			('cs_2', 500, 'eur', 'pending', 'Donation', '{}', ''),
			('cs_3', 2500, 'eur', 'paid', 'Credits', '{}', '')`,
		`INSERT INTO of_subscription_plans (name, amount, currency, interval, trial_days) VALUES ('Pro', 1999, 'eur', 'month', 7)`,
		`INSERT INTO of_subscriptions (user_id, plan_id, external_id, status) VALUES (4, 1, 'sub_1', 'Active')`,
	} {
		if _, err := pdb.ExecWrite(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const path = "/admin/api/projects/7/payments"

	if rec := f.do(http.MethodGet, path, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d, want 401", rec.Code)
	}
	if rec := f.do(http.MethodGet, path, viewer, ""); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin: %d, want 403", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/admin/api/projects/99/payments", admin, ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing project: %d, want 404", rec.Code)
	}

	rec := f.do(http.MethodGet, path, admin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, hidden := range []string{"buyer@example.com", "secret-link", "checkout"} {
		if strings.Contains(body, hidden) {
			t.Errorf("the response carries %q: %s", hidden, body)
		}
	}
	var got struct {
		Configured   bool           `json:"configured"`
		ProviderType string         `json:"provider_type"`
		Totals       []paymentTotal `json:"totals"`
		Payments     []paymentRow   `json:"payments"`
		Plans        []planRow      `json:"plans"`
		Subs         []struct {
			Plan   string `json:"plan"`
			Status string `json:"status"`
			UserID *int64 `json:"user_id"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Configured || got.ProviderType != "stripe" {
		t.Errorf("config = %v %q", got.Configured, got.ProviderType)
	}
	if len(got.Payments) != 3 || got.Payments[0].ExternalID != "cs_3" {
		t.Errorf("payments = %+v, want 3 with the newest (cs_3) first", got.Payments)
	}
	paid := int64(0)
	for _, tot := range got.Totals {
		if tot.Status == "paid" {
			paid += tot.Amount
			if tot.Count != 2 {
				t.Errorf("paid count = %d, want 2", tot.Count)
			}
		}
	}
	if paid != 4499 {
		t.Errorf("paid total = %d, want 4499", paid)
	}
	if len(got.Plans) != 1 || got.Plans[0].Name != "Pro" || got.Plans[0].TrialDays != 7 {
		t.Errorf("plans = %+v", got.Plans)
	}
	if len(got.Subs) != 1 || got.Subs[0].Plan != "Pro" || got.Subs[0].Status != "active" || got.Subs[0].UserID == nil || *got.Subs[0].UserID != 4 {
		t.Errorf("subscriptions = %+v", got.Subs)
	}

	// A project that sells nothing has empty lists, not nulls the page would trip on.
	if _, err := f.pm.Create(8); err != nil {
		t.Fatal(err)
	}
	rec = f.do(http.MethodGet, "/admin/api/projects/8/payments", admin, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"payments":[]`) || !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Errorf("empty project: %d %s", rec.Code, rec.Body.String())
	}
}
