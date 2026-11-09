package admin

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	"openfabrica/db"
	"openfabrica/db/models"
	sdeps "openfabrica/server/deps"
)

// newTransferTestDeps builds a Deps over a real global DB (migrated) and a real
// ProjectDBManager rooted at dataDir, seeded with one selectable LLM model.
func newTransferTestDeps(t *testing.T, dataDir string) *Deps {
	t.Helper()
	gdb, err := db.Open(filepath.Join(dataDir, "openfabrica.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { _ = gdb.Close() })

	mustExec(t, gdb.DB, `INSERT INTO providers (id, name, vendor, transport, requires_api_key, kind, is_enabled)
	                     VALUES (1, 'Test Provider', 'testvendor', 'openai-chat', 0, 'llm', 1)`)
	mustExec(t, gdb.DB, `INSERT INTO llm_models (id, provider_id, model_id, display_name, is_default, is_enabled)
	                     VALUES (1, 1, 'test-model-x', 'Test Model', 1, 1)`)

	// Registered after t.TempDir's own cleanup so the project DB handles are
	// closed before the directory is removed (Windows refuses to unlink open files).
	pdbm := db.NewProjectDBManager(dataDir)
	t.Cleanup(pdbm.CloseAll)

	return &Deps{Core: sdeps.Core{
		DB:               gdb,
		ProjectDBManager: pdbm,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
}

// TestProjectExportImportRoundTrip drives the real Export and Import handlers
// end to end: a live project with an inline page, a disk-backed asset, a secret,
// chat history and file history is exported to a .of archive, then imported as a
// new project on the same instance.
func TestProjectExportImportRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	deps := newTransferTestDeps(t, dataDir)
	h := &ProjectTransferHandler{deps: deps}

	desc := "a demo project"
	project, err := models.CreateProject(deps.DB.DB, "My Cool App", nil, &desc, 1, nil, "medium")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if project.ID != 1 {
		t.Fatalf("expected first project id 1, got %d", project.ID)
	}

	pdb, err := deps.ProjectDBManager.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}

	// Authored content that must survive the round trip.
	mustExec(t, pdb.Writer(), `INSERT INTO of_files (kind, path, storage, body, scope)
	                           VALUES ('page', '/', 'inline', '<h1>hello</h1>', 'global')`)
	// A disk-backed artifact: DB row plus the physical bytes under the project dir.
	diskPath := filepath.Join("data", "projects", "1", "files", "logo.png")
	mustExec(t, pdb.Writer(), `INSERT INTO of_files (kind, path, storage, disk_path, content_type, scope)
	                           VALUES ('file', 'logo.png', 'disk', ?, 'image/png', 'global')`, diskPath)
	assetDir := filepath.Join(dataDir, "projects", "1", "files")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, "logo.png"), []byte("PNG-BYTES"), 0o644); err != nil {
		t.Fatalf("write asset: %v", err)
	}
	// State that must NOT survive: secret values, chat, file history.
	mustExec(t, pdb.Writer(), `INSERT INTO of_secrets (name, value_encrypted) VALUES ('STRIPE_KEY', 'cipher-blob')`)
	mustExec(t, pdb.Writer(), `INSERT INTO of_chat_messages (session_id, role, content) VALUES ('s1', 'user', 'build me a thing')`)
	mustExec(t, pdb.Writer(), `INSERT INTO of_file_history (file_id, kind, path, version_number, changed_by)
	                           VALUES (1, 'page', '/', 1, 'brain')`)

	// Export refuses a project that has not gone live.
	if code, _ := doExport(t, h, "1"); code != http.StatusConflict {
		t.Fatalf("export of a building project: got %d, want 409", code)
	}
	if err := models.UpdateProjectMode(deps.DB.DB, 1, "live"); err != nil {
		t.Fatalf("set live: %v", err)
	}
	if err := models.SetProjectUserFlows(deps.DB.DB, 1, false); err != nil {
		t.Fatalf("turn user-flow tests off: %v", err)
	}

	code, res := doExport(t, h, "1")
	if code != http.StatusOK {
		t.Fatalf("export: got %d, want 200 (body %q)", code, res.Body.String())
	}
	if got := res.Header().Get("Content-Disposition"); got != `attachment; filename="my-cool-app.of"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	archive := res.Body.Bytes()

	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("exported archive is not a valid zip: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{"manifest.json", "project.db", "files/logo.png"} {
		if !names[want] {
			t.Errorf("archive is missing %q (has %v)", want, names)
		}
	}
	manifest, err := readManifest(zr)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.Format != projectExportFormat || manifest.FormatVersion != projectExportVersion {
		t.Errorf("manifest format = %q/%d", manifest.Format, manifest.FormatVersion)
	}
	if manifest.UserFlows == nil || *manifest.UserFlows {
		t.Errorf("manifest must record that user-flow tests are off, got %v", manifest.UserFlows)
	}
	if manifest.Name != "My Cool App" || manifest.SourceProjectID != 1 {
		t.Errorf("manifest name/source = %q/%d", manifest.Name, manifest.SourceProjectID)
	}
	if manifest.ProviderVendor != "testvendor" || manifest.ModelID != "test-model-x" {
		t.Errorf("manifest model identity = %q/%q, want testvendor/test-model-x", manifest.ProviderVendor, manifest.ModelID)
	}

	// Import the archive back as a new project.
	body, contentType := multipartFile(t, "file", "my-cool-app.of", archive)
	req := httptest.NewRequest(http.MethodPost, "/projects/import", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	h.Import(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("import: got %d, want 201 (body %q)", w.Code, w.Body.String())
	}
	var imported models.Project
	if err := json.Unmarshal(w.Body.Bytes(), &imported); err != nil {
		t.Fatalf("import response: %v", err)
	}
	if imported.ID != 2 {
		t.Fatalf("imported project id = %d, want 2", imported.ID)
	}
	if imported.Name != "My Cool App (imported)" {
		t.Errorf("imported name = %q", imported.Name)
	}
	if imported.Published || imported.Mode != "paused" {
		t.Errorf("imported published/mode = %v/%q, want unpublished/paused", imported.Published, imported.Mode)
	}
	if imported.Domain != nil {
		t.Errorf("imported project carried a domain: %v", *imported.Domain)
	}
	if reloaded, err := models.GetProjectByID(deps.DB.DB, imported.ID); err != nil || models.ProjectUserFlowsEnabled(reloaded) {
		t.Errorf("the imported project must keep the user-flow tests off (err %v)", err)
	}
	if imported.LLMModelID != 1 {
		t.Errorf("imported llm_model_id = %d, want 1 (resolved by vendor+model_id)", imported.LLMModelID)
	}

	// The imported project database.
	newDB, err := deps.ProjectDBManager.Open(2)
	if err != nil {
		t.Fatalf("open imported db: %v", err)
	}
	var pageBody string
	if err := newDB.QueryRow(`SELECT body FROM of_files WHERE kind = 'page' AND path = '/'`).Scan(&pageBody); err != nil {
		t.Fatalf("read imported page: %v", err)
	}
	if pageBody != "<h1>hello</h1>" {
		t.Errorf("imported page body = %q", pageBody)
	}

	var gotDisk string
	if err := newDB.QueryRow(`SELECT disk_path FROM of_files WHERE storage = 'disk'`).Scan(&gotDisk); err != nil {
		t.Fatalf("read imported disk artifact: %v", err)
	}
	wantDisk := filepath.Join(dataDir, "projects", "2", "files", "logo.png")
	if gotDisk != wantDisk {
		t.Errorf("imported disk_path = %q, want %q", gotDisk, wantDisk)
	}
	newAsset := filepath.Join(dataDir, "projects", "2", "files", "logo.png")
	if b, err := os.ReadFile(newAsset); err != nil || string(b) != "PNG-BYTES" {
		t.Errorf("imported asset bytes: %q, err %v", b, err)
	}

	var secretName, secretVal string
	if err := newDB.QueryRow(`SELECT name, value_encrypted FROM of_secrets`).Scan(&secretName, &secretVal); err != nil {
		t.Fatalf("read imported secret: %v", err)
	}
	if secretName != "STRIPE_KEY" || secretVal != "" {
		t.Errorf("imported secret = %q/%q, want STRIPE_KEY with a blank value", secretName, secretVal)
	}

	var chatCount, histCount int
	newDB.QueryRow(`SELECT COUNT(*) FROM of_chat_messages`).Scan(&chatCount)
	newDB.QueryRow(`SELECT COUNT(*) FROM of_file_history`).Scan(&histCount)
	if chatCount != 0 || histCount != 0 {
		t.Errorf("imported chat/history counts = %d/%d, want 0/0", chatCount, histCount)
	}

	// The source project is untouched.
	var srcSecret string
	pdb.QueryRow(`SELECT value_encrypted FROM of_secrets`).Scan(&srcSecret)
	if srcSecret != "cipher-blob" {
		t.Errorf("source secret was mutated by export: %q", srcSecret)
	}
}

// TestImportRejectsBadArchives covers the import guards: foreign files, a
// missing project.db, and zip-slip entries.
func TestImportRejectsBadArchives(t *testing.T) {
	dataDir := t.TempDir()
	deps := newTransferTestDeps(t, dataDir)
	h := &ProjectTransferHandler{deps: deps}

	goodManifest, _ := json.Marshal(projectManifest{
		Format: projectExportFormat, FormatVersion: projectExportVersion,
		SourceProjectID: 1, Name: "X",
	})
	futureManifest, _ := json.Marshal(projectManifest{
		Format: projectExportFormat, FormatVersion: projectExportVersion + 1, Name: "X",
	})

	cases := []struct {
		name    string
		entries map[string][]byte
		raw     []byte // when set, uploaded verbatim instead of zipping entries
	}{
		{name: "not a zip", raw: []byte("definitely not a zip file")},
		{name: "missing manifest", entries: map[string][]byte{"project.db": []byte("x")}},
		{name: "foreign manifest", entries: map[string][]byte{"manifest.json": []byte(`{"format":"something-else"}`)}},
		{name: "future version", entries: map[string][]byte{"manifest.json": futureManifest}},
		{name: "missing project.db", entries: map[string][]byte{"manifest.json": goodManifest}},
		{name: "zip slip", entries: map[string][]byte{
			"manifest.json":       goodManifest,
			"../escaped/evil.txt": []byte("pwned"),
			"project.db":          []byte("x"),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := c.raw
			if payload == nil {
				payload = zipBytes(t, c.entries)
			}
			body, contentType := multipartFile(t, "file", "x.of", payload)
			req := httptest.NewRequest(http.MethodPost, "/projects/import", body)
			req.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			h.Import(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400 (body %q)", w.Code, w.Body.String())
			}
		})
	}

	// Every rejection must leave no project behind.
	var count int
	deps.DB.DB.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&count)
	if count != 0 {
		t.Errorf("rejected imports left %d project rows behind", count)
	}
	if entries, err := os.ReadDir(filepath.Join(dataDir, "projects")); err == nil && len(entries) > 0 {
		t.Errorf("rejected imports left project dirs behind: %d", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dataDir, "escaped")); err == nil {
		t.Error("zip-slip entry escaped the project directory")
	}
}

// doExport runs the Export handler through a chi router so the {projectID} URL
// parameter resolves the same way it does in the real route table.
func doExport(t *testing.T, h *ProjectTransferHandler, projectID string) (int, *httptest.ResponseRecorder) {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/projects/{projectID}/export", h.Export)
	req := httptest.NewRequest(http.MethodGet, "/projects/"+projectID+"/export", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w
}

// multipartFile wraps data as a single-file multipart form body.
func multipartFile(t *testing.T, field, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// zipBytes builds an in-memory zip from name -> content.
func zipBytes(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		wr, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := wr.Write(content); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}
