package data

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/tools/tooltest"
)

// uploadFile stores a file the way the admin Files tab does: bytes on disk under
// the project's files/ dir, an of_files row with storage='disk' and no body.
func uploadFile(t *testing.T, h *tooltest.Harness, name string, data []byte) {
	t.Helper()
	dir := filepath.Join(h.Context().ProjectDir(), "files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB().Exec(
		`INSERT INTO of_files (kind, path, storage, content_type, size, disk_path, is_deleted, updated_at)
		 VALUES ('file', ?, 'disk', 'application/octet-stream', ?, ?, 0, CURRENT_TIMESTAMP)`,
		name, len(data), p); err != nil {
		t.Fatal(err)
	}
}

func kbHarness(t *testing.T) *tooltest.Harness {
	t.Helper()
	h := tooltest.New(t)
	h.Env.EmbedFunc = fakeEmbed
	h.MustOK("manage_knowledge", map[string]any{"action": "create_base", "name": "handbook"})
	return h
}

// A file uploaded in the admin Files tab is stored on disk with no body, so
// from_file must ingest it without a "converting NULL" error, even for plain
// Markdown, and retrieval must find it.
func TestIngestFromUploadedTextFile(t *testing.T) {
	h := kbHarness(t)
	uploadFile(t, h, "pets.md", []byte("# Pets\n\nOur office cat is called Miso."))

	res := h.MustOK("manage_knowledge", map[string]any{"action": "ingest", "base": "handbook", "from_file": "pets.md"})
	if !strings.Contains(tooltest.DataJSON(res), `"chunks":1`) {
		t.Fatalf("ingest result = %s, want one chunk", tooltest.DataJSON(res))
	}
	got := h.MustOK("manage_knowledge", map[string]any{"action": "retrieve", "base": "handbook", "query": "cat"})
	if !strings.Contains(tooltest.DataJSON(got), "Miso") {
		t.Fatalf("retrieve did not find the uploaded file's text: %s", tooltest.DataJSON(got))
	}
}

// An uploaded .docx goes through the same extractor as ingest_path.
func TestIngestFromUploadedDocx(t *testing.T) {
	h := kbHarness(t)
	uploadFile(t, h, "policy.docx", makeMinimalDocx(t, "The cat policy allows one cat per desk."))

	h.MustOK("manage_knowledge", map[string]any{"action": "ingest", "base": "handbook", "from_file": "policy.docx"})
	got := h.MustOK("manage_knowledge", map[string]any{"action": "retrieve", "base": "handbook", "query": "cat"})
	if !strings.Contains(tooltest.DataJSON(got), "one cat per desk") {
		t.Fatalf("retrieve did not find the docx text: %s", tooltest.DataJSON(got))
	}
}

// A model-saved (inline) file still ingests as before.
func TestIngestFromInlineFile(t *testing.T) {
	h := kbHarness(t)
	if _, err := h.DB().Exec(`INSERT INTO of_files (kind, path, storage, body, size, is_deleted) VALUES ('file', 'docs/faq.md', 'inline', 'Q: pets? A: the cat stays.', 26, 0)`); err != nil {
		t.Fatal(err)
	}
	h.MustOK("manage_knowledge", map[string]any{"action": "ingest", "base": "handbook", "from_file": "docs/faq.md"})
}

func TestIngestFromFileRefusals(t *testing.T) {
	h := kbHarness(t)

	// A binary upload is named as unsupported, not ingested as garbage.
	uploadFile(t, h, "photo.png", []byte("\x89PNG\r\n\x1a\n\x00\x00binary"))
	if r := h.MustFail("manage_knowledge", map[string]any{"action": "ingest", "base": "handbook", "from_file": "photo.png"}, ""); !strings.Contains(r.Error, "binary") {
		t.Errorf("png: error = %q, want the unsupported-file message", r.Error)
	}

	// A row whose disk_path escapes the project directory is refused, never read.
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("cat secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB().Exec(`INSERT INTO of_files (kind, path, storage, size, disk_path, is_deleted) VALUES ('file', 'secret.md', 'disk', 10, ?, 0)`, outside); err != nil {
		t.Fatal(err)
	}
	if r := h.MustFail("manage_knowledge", map[string]any{"action": "ingest", "base": "handbook", "from_file": "secret.md"}, ""); !strings.Contains(r.Error, "escapes") {
		t.Errorf("escaping disk_path: error = %q, want the containment refusal", r.Error)
	}

	h.MustFail("manage_knowledge", map[string]any{"action": "ingest", "base": "handbook", "from_file": "missing.md"}, "")
}

// A base an LLM endpoint is bound to cannot be deleted out from under it; once the
// endpoint lets go, the delete goes through.
func TestDeleteBaseRefusedWhileAnEndpointUsesIt(t *testing.T) {
	h := kbHarness(t)
	if _, err := h.DB().Exec(`INSERT INTO of_llm_endpoints (path, knowledge_base) VALUES ('ask', 'handbook')`); err != nil {
		t.Fatal(err)
	}
	res := h.MustFail("manage_knowledge", map[string]any{"action": "delete_base", "base": "handbook"}, "")
	if !strings.Contains(res.Error, "ask") {
		t.Errorf("the refusal should name the endpoint: %q", res.Error)
	}
	if _, err := h.DB().Exec(`UPDATE of_llm_endpoints SET knowledge_base = '' WHERE path = 'ask'`); err != nil {
		t.Fatal(err)
	}
	h.MustOK("manage_knowledge", map[string]any{"action": "delete_base", "base": "handbook"})
}
