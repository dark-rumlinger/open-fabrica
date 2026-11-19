package store

import (
	"database/sql"
	"time"

	"openfabrica/db"
)

// PublishedPageRow holds the rendered fields of a published page from of_files.
type PublishedPageRow struct {
	Title      sql.NullString
	Content    sql.NullString
	Metadata   sql.NullString
	LayoutName sql.NullString
}

// LoadPublishedPage fetches a published page row by exact path.
func LoadPublishedPage(reader *sql.DB, pagePath string) (*PublishedPageRow, error) {
	var p PublishedPageRow
	err := reader.QueryRow(
		"SELECT json_extract(metadata, '$.title'), body, metadata, json_extract(metadata, '$.layout') FROM of_files WHERE kind = 'page' AND path = ? AND status = 'published' AND is_deleted = 0",
		pagePath,
	).Scan(&p.Title, &p.Content, &p.Metadata, &p.LayoutName)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CountPublishedPages returns the number of published, non-deleted pages.
func CountPublishedPages(reader *sql.DB) (int, error) {
	var n int
	err := reader.QueryRow(
		"SELECT COUNT(1) FROM of_files WHERE kind = 'page' AND status = 'published' AND is_deleted = 0",
	).Scan(&n)
	return n, err
}

// LatestRunStage returns the stage of the most recent of_runs row, or "".
func LatestRunStage(reader *sql.DB) string {
	var stage string
	_ = reader.QueryRow("SELECT stage FROM of_runs ORDER BY started_at DESC, id DESC LIMIT 1").Scan(&stage)
	return stage
}

// LookupRedirect looks up a redirect rule by source path.
func LookupRedirect(reader *sql.DB, path string) (target string, statusCode int, found bool) {
	err := reader.QueryRow(
		"SELECT redirect_to, status_code FROM of_redirects WHERE redirect_from = ?",
		path,
	).Scan(&target, &statusCode)
	if err != nil {
		return "", 0, false
	}
	return target, statusCode, true
}

// ListParamPagePaths returns all published parameterized page paths (containing ':').
func ListParamPagePaths(reader *sql.DB) ([]string, error) {
	rows, err := reader.Query(
		"SELECT path FROM of_files WHERE kind = 'page' AND path LIKE '%:%' AND status = 'published' AND is_deleted = 0",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			paths = append(paths, p)
		}
	}
	return paths, rows.Err()
}

// RecordPageView records a best-effort page view in of_analytics.
func RecordPageView(pdb *db.ProjectDB, pagePath, visitorHash, referrer, ua string) {
	bestEffortExec(
		pdb.Writer(),
		"page view analytics",
		"INSERT INTO of_analytics (page_path, visitor_hash, referrer, user_agent) VALUES (?, ?, ?, ?)",
		pagePath, visitorHash, referrer, ua,
	)
}

// LoadLayoutBody returns the body of a named layout, or "".
func LoadLayoutBody(reader *sql.DB, name string) string {
	if name == "" {
		return ""
	}
	var body sql.NullString
	err := reader.QueryRow(
		"SELECT body FROM of_files WHERE kind = 'layout' AND path = ? AND is_deleted = 0",
		name,
	).Scan(&body)
	if err != nil || !body.Valid {
		return ""
	}
	return body.String
}

// LoadFallbackLayoutBody returns the most recently updated non-empty layout body, or "".
func LoadFallbackLayoutBody(reader *sql.DB) string {
	var body sql.NullString
	err := reader.QueryRow(
		`SELECT body FROM of_files
		 WHERE kind = 'layout' AND is_deleted = 0 AND body != ''
		 ORDER BY updated_at DESC LIMIT 1`,
	).Scan(&body)
	if err != nil || !body.Valid {
		return ""
	}
	return body.String
}

// FileVersionRow holds metadata used to compute an asset's cache-busting hash.
type FileVersionRow struct {
	Path      string
	ID        int64
	Size      int64
	UpdatedAt string
	Version   int64
}

// ListFileVersions returns version metadata for every live file artifact.
func ListFileVersions(reader *sql.DB) ([]FileVersionRow, error) {
	rows, err := reader.Query(
		`SELECT f.path, f.id, COALESCE(f.size,0), COALESCE(f.updated_at,''),
		        COALESCE((SELECT MAX(h.version_number) FROM of_file_history h WHERE h.kind = f.kind AND h.path = f.path), 0)
		   FROM of_files f
		  WHERE f.kind = 'file' AND f.is_deleted = 0`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FileVersionRow
	for rows.Next() {
		var r FileVersionRow
		if rows.Scan(&r.Path, &r.ID, &r.Size, &r.UpdatedAt, &r.Version) == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// ListGlobalAssetPaths returns ordered global-scoped CSS/JS/favicon file paths.
func ListGlobalAssetPaths(reader *sql.DB) ([]string, error) {
	rows, err := reader.Query(
		`SELECT path FROM of_files
		 WHERE kind = 'file' AND is_deleted = 0 AND scope = 'global'
		   AND (path LIKE '%.css' OR path LIKE '%.js' OR path LIKE '%.mjs'
		        OR path IN ('favicon.svg', 'favicon.png', 'favicon.ico', 'apple-touch-icon.png'))
		 ORDER BY path`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// ListJSModulePaths returns all live .js and .mjs file paths.
func ListJSModulePaths(reader *sql.DB) ([]string, error) {
	rows, err := reader.Query(
		`SELECT path
		   FROM of_files
		  WHERE kind = 'file' AND is_deleted = 0
		    AND (path LIKE '%.js' OR path LIKE '%.mjs')`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// LoadCurrentPlanJSON returns the plan_json of the latest run, or "".
func LoadCurrentPlanJSON(reader *sql.DB) string {
	var planJSON sql.NullString
	err := reader.QueryRow(
		`SELECT plan_json FROM of_runs
		 WHERE plan_json IS NOT NULL AND plan_json != ''
		 ORDER BY started_at DESC, id DESC LIMIT 1`,
	).Scan(&planJSON)
	if err != nil || !planJSON.Valid {
		return ""
	}
	return planJSON.String
}

// ListPageAssetPaths returns the ordered list of artifact paths linked to pagePath.
func ListPageAssetPaths(reader *sql.DB, pagePath string) []string {
	rows, err := reader.Query(
		`SELECT file_path FROM of_page_assets WHERE page_path = ? ORDER BY position, file_path`,
		pagePath,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// SEOMetaRow holds per-page SEO override fields from of_seo_meta.
type SEOMetaRow struct {
	Title          sql.NullString
	Description    sql.NullString
	OGTitle        string
	OGDescription  string
	OGImage        sql.NullString
	CanonicalURL   sql.NullString
	StructuredData sql.NullString
	TwitterCard    sql.NullString
	Locale         sql.NullString
}

// LoadSEOMeta returns per-page SEO overrides from of_seo_meta.
func LoadSEOMeta(reader *sql.DB, pagePath string) (*SEOMetaRow, error) {
	var s SEOMetaRow
	err := reader.QueryRow(
		`SELECT title, description, COALESCE(og_title, ''), COALESCE(og_description, ''), og_image, canonical_url, structured_data, twitter_card, locale
		 FROM of_seo_meta WHERE page_path = ?`, pagePath,
	).Scan(&s.Title, &s.Description, &s.OGTitle, &s.OGDescription, &s.OGImage, &s.CanonicalURL, &s.StructuredData, &s.TwitterCard, &s.Locale)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ListCSSJSAssetPaths returns all live .css and .js file paths.
func ListCSSJSAssetPaths(reader *sql.DB) ([]string, error) {
	rows, err := reader.Query("SELECT path FROM of_files WHERE kind = 'file' AND is_deleted = 0 AND (path LIKE '%.css' OR path LIKE '%.js')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// LoadComponentBody returns the body of a non-deleted component by name.
func LoadComponentBody(reader *sql.DB, name string) (string, bool) {
	var body string
	if err := reader.QueryRow("SELECT body FROM of_files WHERE kind = 'component' AND path = ? AND is_deleted = 0", name).Scan(&body); err != nil {
		return "", false
	}
	return body, true
}

// SitemapPageRow holds path and updated_at for sitemap.xml generation.
type SitemapPageRow struct {
	Path      string
	UpdatedAt time.Time
}

// ListPublishedPagesForSitemap returns all published, non-deleted pages ordered by path.
func ListPublishedPagesForSitemap(reader *sql.DB) ([]SitemapPageRow, error) {
	rows, err := reader.Query(
		"SELECT path, updated_at FROM of_files WHERE kind = 'page' AND status = 'published' AND is_deleted = 0 ORDER BY path",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SitemapPageRow
	for rows.Next() {
		var r SitemapPageRow
		if rows.Scan(&r.Path, &r.UpdatedAt) == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// PublicFileRow holds storage metadata for a live file artifact.
type PublicFileRow struct {
	Storage     sql.NullString
	Body        sql.NullString
	DiskPath    sql.NullString
	ContentType sql.NullString
}

// LoadPublicFile loads a live file artifact by path.
func LoadPublicFile(reader *sql.DB, path string) (*PublicFileRow, error) {
	var f PublicFileRow
	err := reader.QueryRow(
		"SELECT storage, body, disk_path, content_type FROM of_files WHERE kind = 'file' AND path = ? AND is_deleted = 0",
		path,
	).Scan(&f.Storage, &f.Body, &f.DiskPath, &f.ContentType)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// LoadGlobalPublicFile loads a live global-scoped file artifact by path.
func LoadGlobalPublicFile(reader *sql.DB, path string) (*PublicFileRow, error) {
	var f PublicFileRow
	err := reader.QueryRow(
		"SELECT storage, body, disk_path, content_type FROM of_files WHERE kind = 'file' AND path = ? AND scope = 'global' AND is_deleted = 0",
		path,
	).Scan(&f.Storage, &f.Body, &f.DiskPath, &f.ContentType)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// FindFaviconPath returns the highest-priority favicon path in of_files, or "".
func FindFaviconPath(reader *sql.DB) string {
	var filename string
	err := reader.QueryRow(
		`SELECT path FROM of_files
		 WHERE kind = 'file' AND is_deleted = 0
		   AND path IN ('favicon.ico', 'favicon.svg', 'favicon.png')
		 ORDER BY CASE path
		   WHEN 'favicon.ico' THEN 1
		   WHEN 'favicon.svg' THEN 2
		   ELSE 3
		 END
		 LIMIT 1`,
	).Scan(&filename)
	if err != nil {
		return ""
	}
	return filename
}
