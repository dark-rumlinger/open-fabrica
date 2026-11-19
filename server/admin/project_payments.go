package admin

import (
	"net/http"

	"openfabrica/store"
)

// ProjectPaymentsHandler gives the owner a read-only view of the money a project
// has taken: payments, subscription plans and the subscriptions people hold.
type ProjectPaymentsHandler struct {
	deps *Deps
}

// Overview returns everything the Payments panel shows in one response.
func (h *ProjectPaymentsHandler) Overview(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.LoadPaymentsOverview(projectDB))
}
