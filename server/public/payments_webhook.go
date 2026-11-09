package public

// Provider payment webhooks, served at POST /payments/webhook.
//
// The provider's signed callback is the ONLY trustworthy confirmation that money
// moved, so fulfillment hangs off the payment.completed this raises, never off the
// success_url redirect any visitor can open without paying.
//
// The route carries no auth middleware on purpose: the provider holds no token, so
// the request's authenticity IS its signature, which billing.ReconcileWebhook
// verifies before touching a money row. The handler stays thin - HTTP in, status out
// - and owns no part of deciding whether an event is genuine.
//
// One route per project, not a named family: of_payment_config is a singleton (WHERE
// id = 1), so there is exactly one provider callback per project. The project resolves
// from the request host like every other public route.

import (
	"errors"
	"io"
	"net/http"

	"openfabrica/billing"
)

// maxWebhookBody caps what a provider callback may post. Signature verification
// hashes the whole body, so an unbounded read would let an unauthenticated caller
// spend memory before anything has authenticated it.
const maxWebhookBody = 1 << 20 // 1MB

// stripeSignatureHeader is the only provider signature scheme reconciliation
// supports today; ReconcileWebhook refuses the others with a message that says so,
// rather than failing their genuine events silently.
const stripeSignatureHeader = "Stripe-Signature"

func (h *Handler) PaymentWebhook(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	projectDB := h.openProjectDB(w, project.ID)
	if projectDB == nil {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}

	// ToolHandle is the same read/write-routed handle the engine hands tools, so a
	// reconcile behaves identically whichever caller drove it.
	out, err := billing.ReconcileWebhook(billing.WebhookEnv{
		DB:        projectDB.ToolHandle(r.Context()),
		Encryptor: h.deps.Encryptor,
		Bus:       h.deps.Bus,
		Logger:    h.deps.Logger,
		ProjectID: project.ID,
	}, string(body), r.Header.Get(stripeSignatureHeader))

	if err != nil {
		var ref *billing.WebhookRefusal
		if errors.As(err, &ref) {
			// A refusal means the caller is wrong (unsigned, unverifiable, or a
			// provider this build cannot authenticate). 400 rather than 500 so the
			// provider stops redelivering something that will never verify.
			h.deps.Logger.Warn("payment webhook refused", "project_id", project.ID, "reason", ref.Msg)
			writeError(w, http.StatusBadRequest, ref.Msg)
			return
		}
		// A real fault is ours. 500 invites the provider's retry, which is what we
		// want: the reconcile is idempotent, so a redelivery is safe and a dropped
		// settlement is not.
		h.deps.Logger.Error("payment webhook failed", "project_id", project.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "webhook processing failed")
		return
	}

	// 200 on every accepted outcome, including a duplicate: a provider that does not
	// get a 2xx keeps redelivering, and a replay it already sent is not an error.
	writeJSON(w, http.StatusOK, map[string]any{
		"received":    true,
		"type":        out.Kind,
		"external_id": out.ExternalID,
	})
}
