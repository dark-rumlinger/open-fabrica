package admin

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"openfabrica/db/models"
	"openfabrica/security"
	"openfabrica/server/middleware"
)

// AuditHandler records admin-panel mutations and serves the audit log. The
// recording middleware wraps the authenticated mutation route groups; the List
// handler reads the trail back for the Activity view. See the admin_audit table
// in db/migrations/001_initial.sql for why the control plane keeps this trail.
type AuditHandler struct {
	deps *Deps
}

// auditStatusWriter captures the response status so an audit row records the
// outcome of a mutation, not merely that it was attempted. It embeds the
// ResponseWriter so Flush/Hijack and the rest pass straight through; only
// WriteHeader is observed.
type auditStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditStatusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// record is chi middleware that writes one admin_audit row per state-changing
// request (any method other than GET/HEAD) after the handler runs. Reads are not
// recorded: the control plane reads constantly and a read leaves nothing to
// answer for. The insert is best-effort, so an audit-table hiccup logs and is
// swallowed rather than turning a successful action into an error for the user.
func (h *AuditHandler) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}

		sw := &auditStatusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		var userID int
		var username string
		if claims, ok := r.Context().Value(security.ClaimsKey).(*security.Claims); ok {
			userID = claims.UserID
			username = claims.Username
		}
		// projectID is set only on the /projects/{projectID} subtree; it reads back
		// empty (and so 0) for a global action such as a user or provider change.
		projectID, _ := strconv.Atoi(chi.URLParam(r, "projectID"))

		if err := models.InsertAdminAudit(h.deps.DB.DB, models.AuditEntry{
			UserID:          userID,
			Username:        username,
			Method:          r.Method,
			Path:            r.URL.Path,
			TargetProjectID: projectID,
			Status:          sw.status,
			RequestID:       middleware.GetRequestID(r.Context()),
			IP:              middleware.ClientIP(r),
		}); err != nil {
			h.deps.Logger.Error("audit log insert failed", "error", err, "method", r.Method, "path", r.URL.Path)
		}
	})
}

// List returns audit rows newest-first with offset pagination and optional
// username/method filters, as {"items":[...],"total":N}. Admin-only (mounted in
// the admin group): the trail names who operated the control plane.
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	items, total, err := models.ListAdminAudit(
		h.deps.DB.DB,
		r.URL.Query().Get("user"),
		r.URL.Query().Get("action"),
		limit,
		offset,
	)
	if err != nil {
		h.deps.serverError(w, "failed to query audit log", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}
