package admin

// GuidesHandler serves the READ-ONLY platform guide catalogue. Guides ship in the
// repo (knowledge/guides/) and are not editable at runtime; anything project-
// specific lives in project memory (ProjectMemoryHandler), not guides.

import (
	"net/http"

	"openfabrica/pack"
)

// GuidesHandler exposes the read-only platform guides.
type GuidesHandler struct {
	deps *Deps
}

// List returns the platform capability guides (read-only).
func (h *GuidesHandler) List(w http.ResponseWriter, r *http.Request) {
	ps, err := pack.Embedded()
	if err != nil {
		h.deps.Logger.Error("failed to load platform guides", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load guides")
		return
	}
	writeJSON(w, http.StatusOK, pack.Capabilities(ps))
}
