package admin

import (
	"net/http"
	"time"

	"openfabrica/store"
)

// ProjectAnalyticsHandler handles analytics data for admin.
type ProjectAnalyticsHandler struct {
	deps *Deps
}

// Summary returns analytics data for a date range.
func (h *ProjectAnalyticsHandler) Summary(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	// Parse date range (defaults to last 7 days)
	end := r.URL.Query().Get("end")
	start := r.URL.Query().Get("start")
	if end == "" {
		end = time.Now().Format("2006-01-02")
	}
	if start == "" {
		start = time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	}

	// End date is inclusive, add one day for the query
	endExclusive := start // fallback
	if t, err := time.Parse("2006-01-02", end); err == nil {
		endExclusive = t.AddDate(0, 0, 1).Format("2006-01-02")
	}

	writeJSON(w, http.StatusOK, store.LoadAnalyticsSummary(projectDB, start, end, endExclusive))
}
