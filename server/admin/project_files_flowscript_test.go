package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	"openfabrica/probe"
	sdeps "openfabrica/server/deps"
	"openfabrica/store"

	_ "modernc.org/sqlite"
)

// The user-flow script is stored under kind='test' (never published, never a page
// module) but listed in Files: the owner can read it, edit it with the same checks
// write_flows applies, see its history, and never delete it or shadow it with a
// public kind='file' copy.
func TestProjectFilesListsAndGuardsFlowScript(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(7)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	const script = `export default { flows: [{ name: "home", steps: [{ goto: "/" }, { expectText: "Hi" }] }] };`
	if _, _, err := store.ArtifactSave(pdb.Writer(), &store.Artifact{Kind: probe.FlowKind, Path: probe.FlowPath,
		Storage: store.StorageInline, Body: script, ContentType: "text/javascript", Scope: probe.FlowKind}, "brain"); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	if _, err := pdb.ExecWrite(`INSERT INTO of_files (kind, path, body, scope, storage, content_type, metadata, status, is_deleted)
		VALUES ('file','app.js','console.log(1)','page','inline','application/javascript','{}','published',0),
		       ('page','/','<h1>Hi</h1>','page','inline','text/html','{}','published',0)`); err != nil {
		t.Fatalf("seed files: %v", err)
	}
	var scriptID, pageID int
	_ = pdb.QueryRow(`SELECT id FROM of_files WHERE kind = 'test'`).Scan(&scriptID)
	_ = pdb.QueryRow(`SELECT id FROM of_files WHERE kind = 'page'`).Scan(&pageID)

	h := &ProjectFilesHandler{deps: &Deps{Core: sdeps.Core{ProjectDBManager: mgr, Logger: slog.Default()}}}
	call := func(method string, fileID int, fn http.HandlerFunc, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body))
		if strings.HasPrefix(body, "{") {
			req.Header.Set("Content-Type", "application/json")
		}
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("projectID", "7")
		rctx.URLParams.Add("fileID", strconv.Itoa(fileID))
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()
		fn(w, req)
		return w
	}
	put := func(content string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"content": content})
		return call("PUT", scriptID, h.UpdateContent, string(b))
	}
	stored := func() string { return probe.LoadFlowSource(pdb.Reader()) }

	// Listed, marked as the test script; pages are not.
	w := call("GET", 0, h.List, "")
	var files []projectFile
	if err := json.Unmarshal(w.Body.Bytes(), &files); err != nil {
		t.Fatalf("list: %v (%s)", err, w.Body.String())
	}
	kinds := map[string]string{}
	for _, f := range files {
		kinds[f.Filename] = f.Kind
	}
	if kinds[probe.FlowPath] != probe.FlowKind || kinds["app.js"] != "file" || len(files) != 2 {
		t.Fatalf("listed = %+v", files)
	}

	// Readable; a page is not reachable through the Files routes.
	if w := call("GET", scriptID, h.Content, ""); w.Code != http.StatusOK || w.Body.String() != script {
		t.Fatalf("content = %d %q", w.Code, w.Body.String())
	}
	if w := call("GET", pageID, h.Content, ""); w.Code != http.StatusNotFound {
		t.Fatalf("page through Files: %d", w.Code)
	}

	// An edit that would not load is refused with the reason; the script is unchanged.
	if w := put(`export default { flows: [{ name: "x", steps: [{ clik: "#a" }] }] };`); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "was not saved") {
		t.Fatalf("invalid edit: %d %s", w.Code, w.Body.String())
	}
	if stored() != script {
		t.Fatal("a refused edit must leave the script as it was")
	}

	// A valid edit is stored under kind='test' with history.
	edited := strings.Replace(script, `"Hi"`, `"Hello"`, 1)
	if w := put(edited); w.Code != http.StatusOK {
		t.Fatalf("valid edit: %d %s", w.Code, w.Body.String())
	}
	if stored() != edited {
		t.Fatalf("stored = %q", stored())
	}
	var copies int
	_ = pdb.QueryRow(`SELECT COUNT(*) FROM of_files WHERE path = ? AND kind = 'file'`, probe.FlowPath).Scan(&copies)
	if copies != 0 {
		t.Fatal("editing must not create a public kind='file' copy")
	}
	var versions []artifactVersion
	_ = json.Unmarshal(call("GET", scriptID, h.ListVersions, "").Body.Bytes(), &versions)
	if len(versions) < 2 {
		t.Fatalf("versions = %+v", versions)
	}

	// Never deleted, and no second file of the same name.
	if w := call("DELETE", scriptID, h.Delete, ""); w.Code != http.StatusForbidden {
		t.Fatalf("delete script: %d", w.Code)
	}
	if w := call("POST", 0, h.Create, `{"filename":"OF_TEST.js","content":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("create copy: %d %s", w.Code, w.Body.String())
	}
}
