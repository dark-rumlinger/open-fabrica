package admin

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"
)

// ProjectAnalyticsHandler handles analytics data for admin.
type ProjectAnalyticsHandler struct {
	deps *Deps
}

type analyticsSummary struct {
	TotalViews     int            `json:"total_views"`
	UniqueVisitors int            `json:"unique_visitors"`
	TopPages       []topPageEntry `json:"top_pages"`
	TopReferrers   []topRefEntry  `json:"top_referrers"`
	Daily          []dailyEntry   `json:"daily"`
	Start          string         `json:"start"`
	End            string         `json:"end"`
}

type topPageEntry struct {
	Path  string `json:"path"`
	Views int    `json:"views"`
}

type topRefEntry struct {
	Referrer string `json:"referrer"`
	Count    int    `json:"count"`
}

type dailyEntry struct {
	Date           string `json:"date"`
	Views          int    `json:"views"`
	UniqueVisitors int    `json:"unique_visitors"`
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

	summary := analyticsSummary{Start: start, End: end}

	// Totals
	if err := projectDB.QueryRow(
		"SELECT COUNT(*), COUNT(DISTINCT visitor_hash) FROM of_analytics WHERE created_at >= ? AND created_at < ?",
		start, endExclusive,
	).Scan(&summary.TotalViews, &summary.UniqueVisitors); err != nil {
		slog.Debug("analytics: failed to fetch totals", "error", err)
	}

	// Top pages
	rows, err := projectDB.Query(
		"SELECT page_path, COUNT(*) as views FROM of_analytics WHERE created_at >= ? AND created_at < ? GROUP BY page_path ORDER BY views DESC LIMIT 10",
		start, endExclusive,
	)
	if err == nil {
		summary.TopPages = append(summary.TopPages, collectRows(rows, func(rows *sql.Rows) (topPageEntry, error) {
			var p topPageEntry
			if err := rows.Scan(&p.Path, &p.Views); err != nil {
				return p, err
			}
			return p, nil
		})...)
	}

	// Top referrers
	rows2, err := projectDB.Query(
		"SELECT referrer, COUNT(*) as cnt FROM of_analytics WHERE referrer IS NOT NULL AND referrer != '' AND created_at >= ? AND created_at < ? GROUP BY referrer ORDER BY cnt DESC LIMIT 10",
		start, endExclusive,
	)
	if err == nil {
		summary.TopReferrers = append(summary.TopReferrers, collectRows(rows2, func(rows *sql.Rows) (topRefEntry, error) {
			var r topRefEntry
			if err := rows.Scan(&r.Referrer, &r.Count); err != nil {
				return r, err
			}
			return r, nil
		})...)
	}

	// Daily breakdown
	rows3, err := projectDB.Query(
		"SELECT DATE(created_at) as date, COUNT(*) as views, COUNT(DISTINCT visitor_hash) as uniq FROM of_analytics WHERE created_at >= ? AND created_at < ? GROUP BY DATE(created_at) ORDER BY date",
		start, endExclusive,
	)
	if err == nil {
		summary.Daily = append(summary.Daily, collectRows(rows3, func(rows *sql.Rows) (dailyEntry, error) {
			var d dailyEntry
			if err := rows.Scan(&d.Date, &d.Views, &d.UniqueVisitors); err != nil {
				return d, err
			}
			return d, nil
		})...)
	}

	if summary.TopPages == nil {
		summary.TopPages = []topPageEntry{}
	}
	if summary.TopReferrers == nil {
		summary.TopReferrers = []topRefEntry{}
	}
	if summary.Daily == nil {
		summary.Daily = []dailyEntry{}
	}

	writeJSON(w, http.StatusOK, summary)
}
