package admin

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestProjectSpendLimitsRoundTrip(t *testing.T) {
	deps := newBotTestDeps(t)
	if _, err := deps.ProjectDBManager.Create(3); err != nil {
		t.Fatal(err)
	}
	h := &ProjectSpendHandler{deps: deps}
	params := map[string]string{"projectID": "3"}

	rec := doJSON(h.Get, "GET", "/projects/3/spend-limits", "", params)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["daily_token_budget"] != nil || got["daily_usd_cap"] != nil {
		t.Fatalf("a new project has no overrides, got %v", got)
	}

	rec = doJSON(h.Update, "PUT", "/projects/3/spend-limits", `{"daily_token_budget":250000,"daily_usd_cap":4.5}`, params)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["daily_token_budget"] != float64(250000) || got["daily_usd_cap"] != 4.5 || got["effective_token_budget"] != float64(250000) {
		t.Fatalf("saved limits not reflected: %v", got)
	}

	rec = doJSON(h.Update, "PUT", "/projects/3/spend-limits", `{"daily_token_budget":null,"daily_usd_cap":null}`, params)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["daily_token_budget"] != nil || got["daily_usd_cap"] != nil {
		t.Fatalf("null must clear the overrides, got %v", got)
	}
}

func TestProjectSpendLimitsRejectNegativeValues(t *testing.T) {
	deps := newBotTestDeps(t)
	if _, err := deps.ProjectDBManager.Create(4); err != nil {
		t.Fatal(err)
	}
	h := &ProjectSpendHandler{deps: deps}
	params := map[string]string{"projectID": "4"}
	for _, body := range []string{`{"daily_token_budget":-1}`, `{"daily_usd_cap":-0.5}`, `{"daily_usd_cap":99999999}`} {
		if rec := doJSON(h.Update, "PUT", "/projects/4/spend-limits", body, params); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, rec.Code)
		}
	}
}

func TestProjectSpendLimitsUnknownProject(t *testing.T) {
	deps := newBotTestDeps(t)
	h := &ProjectSpendHandler{deps: deps}
	if rec := doJSON(h.Get, "GET", "/projects/99/spend-limits", "", map[string]string{"projectID": "99"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project: %d, want 404", rec.Code)
	}
}
