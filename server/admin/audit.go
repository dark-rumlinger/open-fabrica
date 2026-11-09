package admin

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

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

		if _, err := h.deps.DB.ExecWrite(
			`INSERT INTO admin_audit (user_id, username, method, path, target_project_id, status, request_id, ip)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			userID, username, r.Method, r.URL.Path, projectID, sw.status,
			middleware.GetRequestID(r.Context()), middleware.ClientIP(r),
		); err != nil {
			h.deps.Logger.Error("audit log insert failed", "error", err, "method", r.Method, "path", r.URL.Path)
		}
	})
}

type auditEntry struct {
	ID              int64     `json:"id"`
	UserID          int       `json:"user_id"`
	Username        string    `json:"username"`
	Method          string    `json:"method"`
	Path            string    `json:"path"`
	TargetProjectID int       `json:"target_project_id"`
	Status          int       `json:"status"`
	RequestID       string    `json:"request_id"`
	IP              string    `json:"ip"`
	CreatedAt       time.Time `json:"created_at"`
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

	// Filters are bound parameters, never interpolated. "user" is a substring
	// match on the actor; "action" matches the HTTP method exactly.
	where := " WHERE 1=1"
	var args []any
	if user := strings.TrimSpace(r.URL.Query().Get("user")); user != "" {
		where += " AND username LIKE ?"
		args = append(args, "%"+user+"%")
	}
	if action := strings.TrimSpace(r.URL.Query().Get("action")); action != "" {
		where += " AND method = ?"
		args = append(args, strings.ToUpper(action))
	}

	var total int
	if err := h.deps.DB.DB.QueryRow("SELECT COUNT(*) FROM admin_audit"+where, args...).Scan(&total); err != nil {
		h.deps.serverError(w, "failed to query audit log", err)
		return
	}

	queryArgs := append(append([]any{}, args...), limit, offset)
	rows, err := h.deps.DB.DB.Query(
		"SELECT id, user_id, username, method, path, target_project_id, status, request_id, ip, created_at "+
			"FROM admin_audit"+where+" ORDER BY id DESC LIMIT ? OFFSET ?",
		queryArgs...,
	)
	if err != nil {
		h.deps.serverError(w, "failed to query audit log", err)
		return
	}
	defer rows.Close()

	items := collectRows(rows, func(rs *sql.Rows) (auditEntry, error) {
		var e auditEntry
		err := rs.Scan(&e.ID, &e.UserID, &e.Username, &e.Method, &e.Path,
			&e.TargetProjectID, &e.Status, &e.RequestID, &e.IP, &e.CreatedAt)
		return e, err
	})
	if items == nil {
		items = []auditEntry{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}
