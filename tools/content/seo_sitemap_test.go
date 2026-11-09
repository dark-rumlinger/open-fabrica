package content

import (
	"path/filepath"
	"testing"

	"openfabrica/tools/tooltest"
)

// /sitemap.xml is served live, so generate_sitemap must report the published
// pages without writing a file nobody reads.
func TestGenerateSitemapReportsAndWritesNothing(t *testing.T) {
	h := tooltest.New(t)
	for _, q := range []string{
		`INSERT INTO of_files (kind, path, status, is_deleted) VALUES ('page', '/', 'published', 0)`,
		`INSERT INTO of_files (kind, path, status, is_deleted) VALUES ('page', '/about', 'published', 0)`,
		`INSERT INTO of_files (kind, path, status, is_deleted) VALUES ('page', '/draft', 'draft', 0)`,
	} {
		if _, err := h.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	res := h.MustOK("manage_seo", map[string]any{"action": "generate_sitemap"})
	data, _ := res.Data.(map[string]any)
	if data["pages"] != 2 {
		t.Errorf("pages = %v, want the 2 published pages", data["pages"])
	}
	matches, _ := filepath.Glob(filepath.Join(h.Env.DataDir, "projects", "*", "files", "sitemap.xml"))
	if len(matches) != 0 {
		t.Errorf("a sitemap file was written: %v", matches)
	}
}
