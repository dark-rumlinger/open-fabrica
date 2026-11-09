package api

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// TestGenerateDocsHasNoExternalAssets locks in the external-asset fix: generate_docs must
// never emit a rendered docs *page* that pulls assets from a third-party CDN.
// The machine-readable spec at /api/docs.json is the only docs artifact; the old
// Swagger UI page loaded swagger-ui-dist from unpkg.com at runtime, which broke
// the offline / self-contained guarantee and injected a third-party script into
// every generated app. This test also proves generate_docs removes a stale
// /api/docs page left behind by a project generated before the change.
func TestGenerateDocsHasNoExternalAssets(t *testing.T) {
	t.Chdir(t.TempDir()) // sandbox WriteFileToDisk (the spec asset) to a temp dir

	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	wdb := pdb.Writer()

	// A global DB with a projects row so the spec title resolves. The tool
	// tolerates a missing table (falls back to "API"), but a real row keeps the
	// test exercising the happy path.
	gdb, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { _ = gdb.Close() })
	if _, err := gdb.Exec(`CREATE TABLE projects (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create projects table: %v", err)
	}
	if _, err := gdb.Exec(`INSERT INTO projects (id, name) VALUES (1, 'Test Project')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// One dynamic table + one API endpoint so the generated spec has content.
	// schema_def must be seeded in the shape manage_schema really stores (an OBJECT
	// keyed by column name, each value a bare type string or a constraint object) -
	// see tools/data/schema_ddl.go SchemaCreate. A fixture in any other shape tests
	// a format nothing writes, and the docs come out empty in production while the
	// test passes.
	if _, err := wdb.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('items', ?)`,
		`{"title":{"type":"TEXT","not_null":true},"note":"TEXT","secret":{"type":"PASSWORD","not_null":true},"views":{"type":"INTEGER","default":"0"}}`); err != nil {
		t.Fatalf("seed dynamic table: %v", err)
	}
	if _, err := wdb.Exec(`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth, public_read)
		VALUES ('items', 'items', '["GET","POST"]', 0, 1)`); err != nil {
		t.Fatalf("seed api endpoint: %v", err)
	}

	// A stale /api/docs page whose body pulls swagger-ui from a CDN. Insert it
	// directly: ArtifactSave rejects a cross-origin <script> in a page body
	// (validateNoCrossOriginAssets), so such a row cannot be seeded through the
	// normal path. generate_docs must delete it.
	if _, err := wdb.Exec(
		`INSERT INTO of_files (kind, path, storage, body, content_type, scope)
		 VALUES ('page', '/api/docs', 'inline', ?, 'text/html; charset=utf-8', 'global')`,
		`<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>`,
	); err != nil {
		t.Fatalf("seed stale docs page: %v", err)
	}

	ctx := &tools.ToolContext{DB: wdb, GlobalDB: gdb, ProjectID: 1}
	res, err := (&APIGenerateDocs{}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("generate_docs: err=%v res=%+v", err, res)
	}

	// (1) The docs are served as the portable spec at /api/docs.json, and the
	// tool no longer advertises a rendered docs_page.
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("result data is not a map: %T", res.Data)
	}
	if data["spec_url"] != "/api/docs.json" {
		t.Errorf("spec_url should be /api/docs.json, got %v", data["spec_url"])
	}
	if _, ok := data["docs_page"]; ok {
		t.Errorf("generate_docs must not advertise a docs_page anymore, got %v", data["docs_page"])
	}
	// The seeded table's COLUMNS must reach the spec. A schema entry is emitted per
	// endpoint whatever schema_def parsed to, so the count proves nothing - only the
	// properties do. A schema_def the reader cannot parse yields a documented
	// endpoint whose request/response body is an empty object, with the tool still
	// reporting success.
	var specDiskPath string
	if err := wdb.QueryRow(
		`SELECT disk_path FROM of_files WHERE kind = 'file' AND path = 'api-docs.json'`,
	).Scan(&specDiskPath); err != nil {
		t.Fatalf("spec asset row: %v", err)
	}
	specBytes, err := os.ReadFile(specDiskPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	spec := string(specBytes)
	for _, col := range []string{"title", "note", "views"} {
		if !strings.Contains(spec, `"`+col+`"`) {
			t.Errorf("column %q is missing from the generated spec - schema_def did not parse, so the "+
				"documented request/response bodies are empty", col)
		}
	}
	// PASSWORD columns are request-only, never in a response schema.
	if !strings.Contains(spec, `"format": "password"`) {
		t.Errorf("the PASSWORD column never reached the request schema - schema_def did not parse")
	}

	// (2) No live page artifact remains at /api/docs (the stale CDN page is gone).
	var pageCount int
	if err := wdb.QueryRow(
		`SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND path = '/api/docs' AND is_deleted = 0`,
	).Scan(&pageCount); err != nil {
		t.Fatalf("count /api/docs page: %v", err)
	}
	if pageCount != 0 {
		t.Errorf("generate_docs must delete the stale /api/docs page, but %d live page(s) remain", pageCount)
	}

	// (3) No live artifact body carries an external URL, so a CDN dependency can
	// never reappear in generated output. (The spec itself is stored on disk with
	// an empty inline body, so this scans exactly the rendered-page surface.)
	rows, err := wdb.Query(`SELECT path, COALESCE(body, '') FROM of_files WHERE is_deleted = 0`)
	if err != nil {
		t.Fatalf("scan of_files: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var path, body string
		if err := rows.Scan(&path, &body); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
			t.Errorf("artifact %q body contains an external URL (possible CDN dependency): %s", path, body)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
}

// parseTableSchemaDef reads the of_dynamic_tables.schema_def shape manage_schema
// writes. Both value forms are legal there (a bare type string, or a constraint
// object), and the OpenAPI `required` list means "the request body must carry it",
// so a column the server fills on its own is not required.
func TestParseTableSchemaDefReadsTheStoredShape(t *testing.T) {
	cols := parseTableSchemaDef(`{
		"title":  {"type":"TEXT","not_null":true},
		"note":   "TEXT",
		"views":  {"type":"INTEGER","not_null":true,"default":"0"},
		"code":   {"type":"TEXT","not_null":true,"generate":"code"}
	}`)

	got := map[string]columnDef{}
	for _, c := range cols {
		got[c.Name] = c
	}
	if len(cols) != 4 {
		t.Fatalf("parsed %d columns, want 4: %+v", len(cols), cols)
	}
	if c := got["title"]; c.Type != "TEXT" || !c.Required {
		t.Errorf("title = %+v, want TEXT required (object form, not_null with no default)", c)
	}
	if c := got["note"]; c.Type != "TEXT" || c.Required {
		t.Errorf("note = %+v, want TEXT optional (bare-string form)", c)
	}
	if c := got["views"]; c.Required {
		t.Errorf("views = %+v, want optional - a column with a DEFAULT need not be sent", c)
	}
	if c := got["code"]; c.Required {
		t.Errorf("code = %+v, want optional - a generate column is filled server-side", c)
	}
}

// Column order must not depend on Go's randomized map iteration, or the written
// spec churns between identical runs.
func TestParseTableSchemaDefIsDeterministic(t *testing.T) {
	const def = `{"zeta":"TEXT","alpha":"TEXT","mid":"INTEGER"}`
	first := parseTableSchemaDef(def)
	for i := 0; i < 20; i++ {
		next := parseTableSchemaDef(def)
		for j := range first {
			if first[j].Name != next[j].Name {
				t.Fatalf("column order varies between runs (%v vs %v) - the generated spec would churn",
					first[j].Name, next[j].Name)
			}
		}
	}
	if first[0].Name != "alpha" {
		t.Errorf("columns should be name-sorted, got %v first", first[0].Name)
	}
}
