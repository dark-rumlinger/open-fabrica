package observability

import (
	"fmt"
	"time"

	"openfabrica/tools"
)

// manage_analytics surfaces two read-only data sources the public server
// captures automatically: the visitor-row store (page views) and the LLM
// proxy's per-endpoint daily token spend.

func init() {
	tools.Define(tools.Tool{
		Name: "manage_analytics",
		// Sized to the tool's own output: the 3000-byte default cut it mid-JSON.
		MaxResult: 8000,
		Category:  "observability",
		OneLiner:  "Query page-view analytics or LLM proxy daily token spend.",
		WhenToUse: `Use to answer "what pages are popular last week", "where do visitors come from", or
"how many tokens did the LLM proxy burn this month". All data is captured
automatically by the public server; this tool only reads it. query returns raw
visitor rows; summary returns aggregated rollups; llm_usage rolls up the
per-endpoint per-day proxy token totals archived when each UTC-day window rolls
over.`,
		Actions: []tools.Action{
			&AnalyticsQuery{},
			&AnalyticsSummary{},
			&AnalyticsLLMUsage{},
		},
	})
}

// AnalyticsQuery returns raw visitor rows in a date window, optionally
// filtered to one page path. Capped at 1000 rows to bound the payload.
type AnalyticsQuery struct {
	StartDate string `arg:"start_date,required" desc:"Start date in YYYY-MM-DD format."`
	EndDate   string `arg:"end_date,required"   desc:"End date in YYYY-MM-DD format."`
	PagePath  string `arg:"page_path"           desc:"Filter by a specific page path."`
}

func (a *AnalyticsQuery) Doc() string {
	return "Return raw visitor rows in a date range, optionally filtered by page_path."
}
func (a *AnalyticsQuery) ReadOnly() bool { return true }
func (a *AnalyticsQuery) Example() any {
	return &AnalyticsQuery{StartDate: "2026-05-01", EndDate: "2026-05-21"}
}
func (a *AnalyticsQuery) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	query := `SELECT page_path, visitor_hash, referrer, user_agent, country, created_at
		FROM of_analytics WHERE created_at >= ? AND created_at <= ?`
	queryArgs := []any{a.StartDate, a.EndDate + " 23:59:59"}
	if a.PagePath != "" {
		query += " AND page_path = ?"
		queryArgs = append(queryArgs, a.PagePath)
	}
	query += " ORDER BY created_at DESC LIMIT 1000"

	rows, err := ctx.DB.Query(query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying analytics: %w", err)
	}
	defer rows.Close()

	var records []map[string]any
	for rows.Next() {
		var pagePath, createdAt string
		var visitorHash, referrer, userAgent, country *string
		if err := rows.Scan(&pagePath, &visitorHash, &referrer, &userAgent, &country, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning analytics: %w", err)
		}
		record := map[string]any{"page_path": pagePath, "created_at": createdAt}
		if visitorHash != nil {
			record["visitor_hash"] = *visitorHash
		}
		if referrer != nil {
			record["referrer"] = *referrer
		}
		if userAgent != nil {
			record["user_agent"] = *userAgent
		}
		if country != nil {
			record["country"] = *country
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("querying analytics: %w", err)
	}
	return tools.Ok(records), nil
}

// AnalyticsSummary returns total views, unique visitors, top pages, and top
// referrers over a date window.
type AnalyticsSummary struct {
	StartDate string `arg:"start_date,required" desc:"Start date in YYYY-MM-DD format."`
	EndDate   string `arg:"end_date,required"   desc:"End date in YYYY-MM-DD format."`
}

func (a *AnalyticsSummary) Doc() string    { return "Return aggregated visitor stats over a date range." }
func (a *AnalyticsSummary) ReadOnly() bool { return true }
func (a *AnalyticsSummary) Example() any {
	return &AnalyticsSummary{StartDate: "2026-05-01", EndDate: "2026-05-21"}
}
func (a *AnalyticsSummary) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	dateFilter := " AND created_at >= ? AND created_at <= ?"
	dateArgs := []any{a.StartDate, a.EndDate + " 23:59:59"}

	var totalViews int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_analytics WHERE 1=1"+dateFilter, dateArgs...).Scan(&totalViews); err != nil {
		return nil, fmt.Errorf("counting views: %w", err)
	}

	var uniqueVisitors int
	if err := ctx.DB.QueryRow("SELECT COUNT(DISTINCT visitor_hash) FROM of_analytics WHERE 1=1"+dateFilter, dateArgs...).Scan(&uniqueVisitors); err != nil {
		return nil, fmt.Errorf("counting unique visitors: %w", err)
	}

	pageRows, err := ctx.DB.Query(
		"SELECT page_path, COUNT(*) as views FROM of_analytics WHERE 1=1"+dateFilter+
			" GROUP BY page_path ORDER BY views DESC LIMIT 10", dateArgs...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying top pages: %w", err)
	}
	defer pageRows.Close()
	var topPages []map[string]any
	for pageRows.Next() {
		var path string
		var views int
		if err := pageRows.Scan(&path, &views); err != nil {
			return nil, fmt.Errorf("scanning top page: %w", err)
		}
		topPages = append(topPages, map[string]any{"page_path": path, "views": views})
	}
	if err := pageRows.Err(); err != nil {
		return nil, fmt.Errorf("querying top pages: %w", err)
	}

	refRows, err := ctx.DB.Query(
		"SELECT referrer, COUNT(*) as count FROM of_analytics WHERE referrer IS NOT NULL AND referrer != ''"+
			dateFilter+" GROUP BY referrer ORDER BY count DESC LIMIT 10", dateArgs...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying top referrers: %w", err)
	}
	defer refRows.Close()
	var topReferrers []map[string]any
	for refRows.Next() {
		var referrer string
		var count int
		if err := refRows.Scan(&referrer, &count); err != nil {
			return nil, fmt.Errorf("scanning referrer: %w", err)
		}
		topReferrers = append(topReferrers, map[string]any{"referrer": referrer, "count": count})
	}
	if err := refRows.Err(); err != nil {
		return nil, fmt.Errorf("querying top referrers: %w", err)
	}

	return tools.Ok(map[string]any{
		"total_views":     totalViews,
		"unique_visitors": uniqueVisitors,
		"top_pages":       topPages,
		"top_referrers":   topReferrers,
		"start_date":      a.StartDate,
		"end_date":        a.EndDate,
	}), nil
}

// AnalyticsLLMUsage rolls up per-day token totals for LLM proxy endpoints
// over a date window. Reads of_llm_usage_daily (archived previous days) and
// folds today's running total from of_llm_endpoints if the window includes
// today, so the result always reflects the current state without a gap.
type AnalyticsLLMUsage struct {
	StartDate    string `arg:"start_date,required" desc:"Start date YYYY-MM-DD (UTC)."`
	EndDate      string `arg:"end_date,required"   desc:"End date YYYY-MM-DD (UTC), inclusive."`
	EndpointPath string `arg:"endpoint_path"       desc:"Filter to one endpoint path. Empty = every endpoint."`
}

func (a *AnalyticsLLMUsage) ActionName() string { return "llm_usage" }
func (a *AnalyticsLLMUsage) Doc() string {
	return "Per-day token spend for LLM proxy endpoints, with rollup totals."
}
func (a *AnalyticsLLMUsage) ReadOnly() bool { return true }
func (a *AnalyticsLLMUsage) Example() any {
	return &AnalyticsLLMUsage{StartDate: "2026-05-01", EndDate: "2026-05-31"}
}
func (a *AnalyticsLLMUsage) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	// Historical rows from the daily archive.
	histQuery := "SELECT path, day, tokens_used FROM of_llm_usage_daily WHERE day >= ? AND day <= ?"
	histArgs := []any{a.StartDate, a.EndDate}
	if a.EndpointPath != "" {
		histQuery += " AND path = ?"
		histArgs = append(histArgs, a.EndpointPath)
	}
	histQuery += " ORDER BY day ASC, path ASC"
	histRows, err := ctx.DB.Query(histQuery, histArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying llm usage history: %w", err)
	}
	defer histRows.Close()

	type dayRow struct {
		Path       string `json:"path"`
		Day        string `json:"day"`
		TokensUsed int    `json:"tokens_used"`
	}
	rows := []dayRow{}
	byEndpoint := map[string]int{}
	var grandTotal int
	for histRows.Next() {
		var r dayRow
		if err := histRows.Scan(&r.Path, &r.Day, &r.TokensUsed); err != nil {
			return nil, fmt.Errorf("scanning llm usage: %w", err)
		}
		rows = append(rows, r)
		byEndpoint[r.Path] += r.TokensUsed
		grandTotal += r.TokensUsed
	}
	if err := histRows.Err(); err != nil {
		return nil, fmt.Errorf("querying llm usage history: %w", err)
	}

	// Fold today's live counter from of_llm_endpoints when the window
	// includes today - otherwise the latest day is silently absent until the
	// next UTC-roll archives it.
	todayUTC := time.Now().UTC().Format("2006-01-02")
	if todayUTC >= a.StartDate && todayUTC <= a.EndDate {
		liveQuery := "SELECT path, daily_tokens_used, day_window_start FROM of_llm_endpoints WHERE daily_tokens_used > 0 AND day_window_start = ?"
		liveArgs := []any{todayUTC}
		if a.EndpointPath != "" {
			liveQuery += " AND path = ?"
			liveArgs = append(liveArgs, a.EndpointPath)
		}
		liveRows, err := ctx.DB.Query(liveQuery, liveArgs...)
		if err != nil {
			return nil, fmt.Errorf("querying live llm usage: %w", err)
		}
		defer liveRows.Close()
		for liveRows.Next() {
			var r dayRow
			if err := liveRows.Scan(&r.Path, &r.TokensUsed, &r.Day); err != nil {
				return nil, fmt.Errorf("scanning live llm usage: %w", err)
			}
			rows = append(rows, r)
			byEndpoint[r.Path] += r.TokensUsed
			grandTotal += r.TokensUsed
		}
		if err := liveRows.Err(); err != nil {
			return nil, fmt.Errorf("querying live llm usage: %w", err)
		}
	}

	perEndpoint := make([]map[string]any, 0, len(byEndpoint))
	for path, total := range byEndpoint {
		perEndpoint = append(perEndpoint, map[string]any{
			"path":         path,
			"tokens_total": total,
		})
	}

	return tools.Ok(map[string]any{
		"start_date":         a.StartDate,
		"end_date":           a.EndDate,
		"daily":              rows,
		"per_endpoint_total": perEndpoint,
		"grand_total":        grandTotal,
	}), nil
}
