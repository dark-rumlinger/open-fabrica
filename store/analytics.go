package store

import (
	"log/slog"

	"openfabrica/db"
)

// AnalyticsSummary holds aggregated traffic metrics for a date range.
type AnalyticsSummary struct {
	TotalViews     int            `json:"total_views"`
	UniqueVisitors int            `json:"unique_visitors"`
	TopPages       []TopPageEntry `json:"top_pages"`
	TopReferrers   []TopRefEntry  `json:"top_referrers"`
	Daily          []DailyEntry   `json:"daily"`
	Start          string         `json:"start"`
	End            string         `json:"end"`
}

// TopPageEntry represents one row in the top-pages breakdown.
type TopPageEntry struct {
	Path  string `json:"path"`
	Views int    `json:"views"`
}

// TopRefEntry represents one row in the top-referrers breakdown.
type TopRefEntry struct {
	Referrer string `json:"referrer"`
	Count    int    `json:"count"`
}

// DailyEntry represents one day in the daily analytics breakdown.
type DailyEntry struct {
	Date           string `json:"date"`
	Views          int    `json:"views"`
	UniqueVisitors int    `json:"unique_visitors"`
}

// LoadAnalyticsSummary queries of_analytics for [start, endExclusive) and
// returns the aggregated summary.
func LoadAnalyticsSummary(pdb *db.ProjectDB, start, end, endExclusive string) AnalyticsSummary {
	summary := AnalyticsSummary{
		Start:        start,
		End:          end,
		TopPages:     []TopPageEntry{},
		TopReferrers: []TopRefEntry{},
		Daily:        []DailyEntry{},
	}

	if err := pdb.QueryRow(
		"SELECT COUNT(*), COUNT(DISTINCT visitor_hash) FROM of_analytics WHERE created_at >= ? AND created_at < ?",
		start, endExclusive,
	).Scan(&summary.TotalViews, &summary.UniqueVisitors); err != nil {
		slog.Debug("analytics: failed to fetch totals", "error", err)
	}

	if rows, err := pdb.Query(
		"SELECT page_path, COUNT(*) as views FROM of_analytics WHERE created_at >= ? AND created_at < ? GROUP BY page_path ORDER BY views DESC LIMIT 10",
		start, endExclusive,
	); err == nil {
		defer rows.Close()
		for rows.Next() {
			var p TopPageEntry
			if rows.Scan(&p.Path, &p.Views) == nil {
				summary.TopPages = append(summary.TopPages, p)
			}
		}
		_ = rows.Err()
	}

	if rows, err := pdb.Query(
		"SELECT referrer, COUNT(*) as cnt FROM of_analytics WHERE referrer IS NOT NULL AND referrer != '' AND created_at >= ? AND created_at < ? GROUP BY referrer ORDER BY cnt DESC LIMIT 10",
		start, endExclusive,
	); err == nil {
		defer rows.Close()
		for rows.Next() {
			var r TopRefEntry
			if rows.Scan(&r.Referrer, &r.Count) == nil {
				summary.TopReferrers = append(summary.TopReferrers, r)
			}
		}
		_ = rows.Err()
	}

	if rows, err := pdb.Query(
		"SELECT DATE(created_at) as date, COUNT(*) as views, COUNT(DISTINCT visitor_hash) as uniq FROM of_analytics WHERE created_at >= ? AND created_at < ? GROUP BY DATE(created_at) ORDER BY date",
		start, endExclusive,
	); err == nil {
		defer rows.Close()
		for rows.Next() {
			var d DailyEntry
			if rows.Scan(&d.Date, &d.Views, &d.UniqueVisitors) == nil {
				summary.Daily = append(summary.Daily, d)
			}
		}
		_ = rows.Err()
	}

	return summary
}
