package content

import (
	"fmt"
	"os"
	"path/filepath"

	"openfabrica/store"
	"openfabrica/tools"
)

// SEO handler functions

// SEO actions are registered under manage_seo in manage_seo.go. Handler functions remain here.

// seoMetaSpec carries one page's meta fields. A struct, not a positional list: the
// row has eleven columns and the flat signature was three names past the point where
// a caller can keep ogTitle/ogDesc/ogImage/canonical in the right order by eye (and
// past the argument-limit rule in .golangci.yml, which exists to stop exactly that).
type seoMetaSpec struct {
	PagePath      string
	Title         string
	Description   string
	OGTitle       string
	OGDescription string
	OGImage       string
	Canonical     string
	Structured    string
	TwitterCard   string
	Locale        string
}

// seoSetMeta upserts per-page meta. Structured, TwitterCard and Locale are all
// consumed by the renderer (loadSEOMeta), so they are real fields rather than being
// pinned empty - JSON-LD and og:locale were otherwise built-and-unreachable. robots
// stays empty: no reader exists for it anywhere, so a field would promise nothing.
func seoSetMeta(ctx *tools.ToolContext, m seoMetaSpec) (*tools.Result, error) {
	robots := ""

	_, err := ctx.DB.Exec(
		`INSERT INTO of_seo_meta (page_path, title, description, og_title, og_description, og_image, canonical_url, robots, structured_data, twitter_card, locale, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(page_path) DO UPDATE SET
		   title = excluded.title,
		   description = excluded.description,
		   og_title = excluded.og_title,
		   og_description = excluded.og_description,
		   og_image = excluded.og_image,
		   canonical_url = excluded.canonical_url,
		   robots = excluded.robots,
		   structured_data = excluded.structured_data,
		   twitter_card = excluded.twitter_card,
		   locale = excluded.locale,
		   updated_at = CURRENT_TIMESTAMP`,
		m.PagePath, m.Title, m.Description, m.OGTitle, m.OGDescription, m.OGImage,
		m.Canonical, robots, m.Structured, m.TwitterCard, m.Locale,
	)
	if err != nil {
		return nil, fmt.Errorf("setting meta: %w", err)
	}

	return tools.Ok(map[string]any{
		"page_path": m.PagePath,
		"title":     m.Title,
	}), nil
}

func seoGetMeta(ctx *tools.ToolContext, pagePath string) (*tools.Result, error) {
	var title, desc, ogTitle, ogDesc, ogImage, canonical, robots, structured, twitterCard, locale string
	err := ctx.DB.QueryRow(
		`SELECT title, description, og_title, og_description, og_image, canonical_url, robots, structured_data, twitter_card, locale
		 FROM of_seo_meta WHERE page_path = ?`, pagePath,
	).Scan(&title, &desc, &ogTitle, &ogDesc, &ogImage, &canonical, &robots, &structured, &twitterCard, &locale)
	if err != nil {
		return tools.Fail("no meta found for page '%s'", pagePath), nil
	}

	return tools.Ok(map[string]any{
		"page_path":       pagePath,
		"title":           title,
		"description":     desc,
		"og_title":        ogTitle,
		"og_description":  ogDesc,
		"og_image":        ogImage,
		"canonical_url":   canonical,
		"robots":          robots,
		"structured_data": structured,
		"twitter_card":    twitterCard,
		"locale":          locale,
	}), nil
}

// seoGenerateSitemap reports what /sitemap.xml lists. The sitemap itself is
// served live from the published pages on every request (server/public Sitemap),
// so there is nothing to write; a file saved here would never be read.
func seoGenerateSitemap(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		"SELECT path FROM of_files WHERE kind = 'page' AND status = 'published' AND is_deleted = 0 ORDER BY path",
	)
	if err != nil {
		return nil, fmt.Errorf("querying pages: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		if rows.Scan(&path) == nil {
			paths = append(paths, path)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("querying pages: %w", err)
	}

	return tools.Ok(map[string]any{
		"pages": len(paths),
		"paths": paths,
		"url":   "/sitemap.xml",
		"note":  "/sitemap.xml is generated live from the published pages on every request; nothing was written and nothing needs regenerating.",
	}), nil
}

func seoSetRobots(ctx *tools.ToolContext, content string) (*tools.Result, error) {
	dir, _ := store.StorageDir(ctx.DataDirOrDefault(), ctx.ProjectID, "files")
	_ = os.MkdirAll(dir, 0755) // os.WriteFile below surfaces any failure
	robotsPath := filepath.Join(dir, "robots.txt")
	if err := os.WriteFile(robotsPath, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("writing robots.txt: %w", err)
	}

	return tools.Ok(map[string]any{
		"url": "/robots.txt",
	}), nil
}

func seoValidate(ctx *tools.ToolContext) (*tools.Result, error) {
	type pageEntry struct {
		path  string
		title string
	}

	rows, err := ctx.DB.Query(
		"SELECT path, COALESCE(json_extract(metadata, '$.title'),'') FROM of_files WHERE kind = 'page' AND is_deleted = 0 ORDER BY path",
	)
	if err != nil {
		return nil, fmt.Errorf("querying pages: %w", err)
	}
	var collected []pageEntry
	for rows.Next() {
		var p pageEntry
		if rows.Scan(&p.path, &p.title) != nil {
			continue
		}
		collected = append(collected, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("querying pages: %w", err)
	}

	// Now that the cursor is closed, audit each page's meta. ctx.DB has
	// MaxOpenConns=1, so the per-page query would deadlock if we ran it inside
	// the rows.Next() loop above.
	var issues []map[string]any
	for _, p := range collected {
		var pageIssues []string

		var title, desc, ogTitle, ogDesc string
		err := ctx.DB.QueryRow(
			"SELECT title, description, og_title, og_description FROM of_seo_meta WHERE page_path = ?",
			p.path,
		).Scan(&title, &desc, &ogTitle, &ogDesc)
		if err != nil {
			pageIssues = append(pageIssues, "no SEO meta set")
		} else {
			if title == "" && p.title == "" {
				pageIssues = append(pageIssues, "missing title")
			}
			if desc == "" {
				pageIssues = append(pageIssues, "missing description")
			}
			if ogTitle == "" && title == "" {
				pageIssues = append(pageIssues, "missing og:title")
			}
			if ogDesc == "" && desc == "" {
				pageIssues = append(pageIssues, "missing og:description")
			}
		}

		if len(pageIssues) > 0 {
			issues = append(issues, map[string]any{
				"page":   p.path,
				"issues": pageIssues,
			})
		}
	}

	return tools.Ok(map[string]any{
		"total_issues": len(issues),
		"pages":        issues,
	}), nil
}
