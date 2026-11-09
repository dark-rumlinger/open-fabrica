package public

import (
	"fmt"
	"html"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"openfabrica/billing"
	"openfabrica/internal/replayoutbox"
)

// The stub checkout a user-flow replay pays on (see billing.FlowCheckoutPrefix).
// While a replay runs, manage_payments create_checkout hands the app
// /__of/checkout/<id> instead of the provider's hosted page; a flow clicks Pay (or
// Cancel) there and lands back on the app's success (or cancel) URL with the
// payment settled through the same path a verified webhook takes. Outside a
// running replay of the project these routes answer 404.

// testCheckout resolves the checkout a stub route names, or writes a 404.
func (h *Handler) testCheckout(w http.ResponseWriter, r *http.Request) (billing.FlowCheckout, string, bool) {
	project := getProject(r)
	id := chi.URLParam(r, "id")
	c, ok := billing.LookupFlowCheckout(id)
	if project == nil || !ok || c.ProjectID != project.ID || !replayoutbox.Active(project.ID) {
		writeError(w, http.StatusNotFound, "no such checkout")
		return c, id, false
	}
	return c, id, true
}

// FlowCheckoutPage serves GET /__of/checkout/{id}.
func (h *Handler) FlowCheckoutPage(w http.ResponseWriter, r *http.Request) {
	c, id, ok := h.testCheckout(w, r)
	if !ok {
		return
	}
	amount := fmt.Sprintf("%.2f %s", float64(c.Amount)/100, c.Currency)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Test checkout</title></head>
<body style="font-family:system-ui;max-width:420px;margin:40px auto;padding:0 16px">
<h1>Test checkout</h1>
<p>This stands in for the payment page during a user-flow test. No money moves.</p>
<p id="of-checkout-item">%s</p><p id="of-checkout-amount"><b>%s</b></p>
<form method="post" action="/__of/checkout/%s/pay"><button id="of-checkout-pay" type="submit">Pay</button></form>
<form method="post" action="/__of/checkout/%s/cancel"><button id="of-checkout-cancel" type="submit">Cancel</button></form>
</body></html>`, html.EscapeString(c.Description), html.EscapeString(amount), url.PathEscape(id), url.PathEscape(id))
}

// FlowCheckoutComplete serves POST /__of/checkout/{id}/{outcome}: pay settles the
// payment (payment.completed fires), cancel fails it, then the buyer returns to
// the app.
func (h *Handler) FlowCheckoutComplete(w http.ResponseWriter, r *http.Request) {
	c, id, ok := h.testCheckout(w, r)
	if !ok {
		return
	}
	paid := chi.URLParam(r, "outcome") == "pay"
	projectDB := h.openProjectDB(w, c.ProjectID)
	if projectDB == nil {
		return
	}
	if err := billing.SettleFlowCheckout(billing.WebhookEnv{
		DB:        projectDB.ToolHandle(r.Context()),
		Encryptor: h.deps.Encryptor,
		Bus:       h.deps.Bus,
		Logger:    h.deps.Logger,
		ProjectID: c.ProjectID,
	}, id, paid); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	back := c.CancelURL
	if paid {
		back = c.SuccessURL
	}
	http.Redirect(w, r, sameSite(back), http.StatusSeeOther)
}

// sameSite keeps a return URL on the site the replay is browsing: the app names
// its public address (https://app.example/thanks), while the replay reaches it
// through the local server.
func sameSite(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return "/"
	}
	if u.IsAbs() {
		return u.RequestURI()
	}
	return raw
}
