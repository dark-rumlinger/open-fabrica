package snapshotbuild

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/store"
)

// PageRef is a single page entry in the project snapshot.
type PageRef struct {
	Path   string
	Title  string
	Assets []string // page-scoped asset filenames linked to this page (from of_files metadata.assets)
}

// TableRef is a single table entry in the project snapshot.
type TableRef struct {
	Name     string
	Columns  []string // raw column definitions ("id INTEGER", ...)
	RowCount int      // number of rows currently in the table; -1 if the count could not be read
}

// ViewRef is a single SQL view entry in the project snapshot. Views (create_view) are
// read-only derived datasets in sqlite_master, not of_dynamic_tables, and hold no rows of
// their own, so unlike TableRef there is no row count to carry.
type ViewRef struct {
	Name    string
	Columns []string // resolved output columns ("date", "revenue_mtd REAL", ...)
}

// FileRef is a saved asset/file entry (CSS, JS, image, etc).
type FileRef struct {
	Path  string
	Kind  string // "css", "js", "html", "image", "other"
	Scope string // "global" (auto-injected on every page) or "page" (linked per-page via artifact metadata.assets)
}

// LayoutRef is a saved layout HTML wrapper.
type LayoutRef struct {
	Name string
}

// ComponentRef is a saved component HTML fragment (resolved by {{include:name}}).
type ComponentRef struct {
	Name string
}

// loadPages reads project pages with their titles and their linked artifacts
// (from of_page_assets). Best-effort, like every read here: a failed query or a
// partial iteration returns the pages read so far.
func loadPages(ctx context.Context, db *sql.DB) []PageRef {
	var out []PageRef
	if rows, err := db.QueryContext(ctx,
		`SELECT path, COALESCE(json_extract(metadata, '$.title'),'')
		 FROM of_files WHERE kind = 'page' AND COALESCE(is_deleted,0) = 0 ORDER BY path`); err == nil {
		type pageRow struct {
			path, title string
		}
		var pages []pageRow
		for rows.Next() {
			var pr pageRow
			if err := rows.Scan(&pr.path, &pr.title); err == nil {
				pages = append(pages, pr)
			}
		}
		_ = rows.Err()
		rows.Close()
		// Load linked artifacts per page now that the cursor is closed.
		for _, pr := range pages {
			var assetList []string
			if aRows, aErr := db.QueryContext(ctx,
				`SELECT file_path FROM of_page_assets WHERE page_path = ? ORDER BY position, file_path`,
				pr.path); aErr == nil {
				for aRows.Next() {
					var ap string
					if aRows.Scan(&ap) == nil {
						assetList = append(assetList, ap)
					}
				}
				_ = aRows.Err()
				aRows.Close()
			}
			out = append(out, PageRef{Path: pr.path, Title: pr.title, Assets: assetList})
		}
	}
	return out
}

// loadTables reads dynamic tables with their columns and row counts. The count tells the
// model which content tables are already seeded so a build resume does not re-insert
// duplicate rows; -1 means unknown, not empty. Counts are read after the cursor closes
// because the single-connection pool would deadlock on a COUNT inside the row loop.
func loadTables(ctx context.Context, db *sql.DB) []TableRef {
	var out []TableRef
	if rows, err := db.QueryContext(ctx,
		`SELECT table_name, schema_def FROM of_dynamic_tables ORDER BY table_name`); err == nil {
		type tableRow struct {
			name, schema string
		}
		var tbls []tableRow
		for rows.Next() {
			var tr tableRow
			if err := rows.Scan(&tr.name, &tr.schema); err == nil {
				tbls = append(tbls, tr)
			}
		}
		_ = rows.Err()
		rows.Close()
		for _, tr := range tbls {
			rowCount := -1
			if physical, perr := store.SanitizedTableName(tr.name); perr == nil {
				var n int
				if cerr := db.QueryRowContext(ctx,
					fmt.Sprintf("SELECT COUNT(*) FROM %s", physical)).Scan(&n); cerr == nil {
					rowCount = n
				}
			}
			out = append(out, TableRef{Name: tr.name, Columns: parseColumnList(tr.schema), RowCount: rowCount})
		}
	}
	return out
}

// loadViews reads project views (create_view) from sqlite_master and resolves each view's
// output columns. Surfaced so the model sees a derived dataset it built and queries it by
// its real name, not only the read endpoint on top. Columns are read after the cursor closes
// (PRAGMA inside the open cursor would deadlock the single-connection pool, same as
// loadTables). The of_ prefix is reserved for internal objects.
func loadViews(ctx context.Context, db *sql.DB) []ViewRef {
	var out []ViewRef
	if rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'view' AND name NOT LIKE 'of_%' ORDER BY name`); err == nil {
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err == nil {
				names = append(names, n)
			}
		}
		_ = rows.Err()
		rows.Close()
		for _, n := range names {
			physical, perr := store.SanitizedTableName(n)
			if perr != nil {
				continue
			}
			var cols []string
			if ci, err := store.GetTableColumnInfo(db, physical); err == nil {
				for _, c := range ci {
					if c.Type != "" {
						cols = append(cols, c.Name+" "+c.Type)
					} else {
						cols = append(cols, c.Name)
					}
				}
			}
			out = append(out, ViewRef{Name: n, Columns: cols})
		}
	}
	return out
}

// loadLayouts reads layout file names.
func loadLayouts(ctx context.Context, db *sql.DB) []LayoutRef {
	var out []LayoutRef
	if rows, err := db.QueryContext(ctx,
		`SELECT path FROM of_files WHERE kind = 'layout' AND COALESCE(is_deleted,0) = 0 ORDER BY path`); err == nil {
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err == nil {
				out = append(out, LayoutRef{Name: name})
			}
		}
		_ = rows.Err()
		rows.Close()
	}
	return out
}

// loadComponents reads component file names.
func loadComponents(ctx context.Context, db *sql.DB) []ComponentRef {
	var out []ComponentRef
	if rows, err := db.QueryContext(ctx,
		`SELECT path FROM of_files WHERE kind = 'component' AND COALESCE(is_deleted,0) = 0 ORDER BY path`); err == nil {
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err == nil {
				out = append(out, ComponentRef{Name: name})
			}
		}
		_ = rows.Err()
		rows.Close()
	}
	return out
}

// loadFiles reads standalone project files with their classified kind and scope.
func loadFiles(ctx context.Context, db *sql.DB) []FileRef {
	var out []FileRef
	if rows, err := db.QueryContext(ctx,
		`SELECT path, COALESCE(content_type,''), COALESCE(scope,'') FROM of_files WHERE kind = 'file' AND is_deleted = 0 ORDER BY path`); err == nil {
		for rows.Next() {
			var name, ctype, scope string
			if err := rows.Scan(&name, &ctype, &scope); err == nil {
				out = append(out, FileRef{Path: name, Kind: classifyFile(name, ctype), Scope: scope})
			}
		}
		_ = rows.Err()
		rows.Close()
	}
	return out
}

// --- parsing helpers ---

// parseColumnList renders a schema_def as "name TYPE" entries, adding NOT NULL for a
// column the caller must send (NOT NULL with no default). The schema_def is an object
// keyed by column name, so it goes through the one shared reader in store.
func parseColumnList(schema string) []string {
	cols := store.ParseSchemaDef(schema)
	if len(cols) == 0 {
		return nil
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		col := c.Name + " " + c.Type
		if c.NotNull && !c.ServerFills {
			col += " NOT NULL"
		}
		out = append(out, col)
	}
	return out
}

func classifyFile(name, ctype string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".css"):
		return "css"
	case strings.HasSuffix(lower, ".js") || strings.HasSuffix(lower, ".mjs"):
		return "js"
	case strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm"):
		return "html"
	case strings.HasSuffix(lower, ".json"):
		return "json"
	case strings.HasSuffix(lower, ".svg"):
		return "svg"
	case strings.HasSuffix(lower, ".png"),
		strings.HasSuffix(lower, ".jpg"),
		strings.HasSuffix(lower, ".jpeg"),
		strings.HasSuffix(lower, ".webp"),
		strings.HasSuffix(lower, ".gif"):
		return "image"
	}
	switch {
	case strings.HasPrefix(ctype, "text/css"):
		return "css"
	case strings.HasPrefix(ctype, "application/javascript"), strings.HasPrefix(ctype, "text/javascript"):
		return "js"
	case strings.HasPrefix(ctype, "image/"):
		return "image"
	}
	return "other"
}
