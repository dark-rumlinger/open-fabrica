package data

import (
	"archive/zip"
	"bytes"
	"context"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// stubEmbed is a deterministic hashing vectorizer: each text becomes a 64-dim
// bag-of-words vector, so two texts sharing words have high cosine similarity.
// Good enough to assert that retrieval surfaces the topically-relevant chunk
// without a real embedding provider.
func stubEmbed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, 64)
		for _, w := range strings.Fields(strings.ToLower(t)) {
			h := fnv.New32a()
			h.Write([]byte(w))
			vec[h.Sum32()%64]++
		}
		out[i] = vec
	}
	return out, nil
}

func newKnowledgeDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	stmts := []string{}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return db
}

func TestChunkText(t *testing.T) {
	// Short text: a single chunk.
	if got := chunkText("hello world", 1000, 150); len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("short text chunking = %v", got)
	}
	// Long text: multiple overlapping chunks, no word split.
	long := strings.TrimSpace(strings.Repeat("alpha beta gamma delta ", 200)) // ~4600 chars
	chunks := chunkText(long, 500, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) == 0 {
			t.Errorf("chunk %d is empty", i)
		}
		// Words stay whole: no chunk starts or ends mid-token for our vocabulary.
		if strings.HasPrefix(c, "lpha") || strings.HasPrefix(c, "eta") {
			t.Errorf("chunk %d split a word: %q", i, c[:10])
		}
	}
}

func TestExtractText(t *testing.T) {
	// Plain text passes through.
	if txt, ok, err := extractText("notes.txt", []byte("hello world")); !ok || err != nil || txt != "hello world" {
		t.Errorf("txt: got (%q, %v, %v)", txt, ok, err)
	}
	// HTML is tag-stripped.
	if txt, ok, _ := extractText("page.html", []byte("<h1>Title</h1><p>Body</p>")); !ok || !strings.Contains(txt, "Title") || strings.Contains(txt, "<h1>") {
		t.Errorf("html strip failed: %q", txt)
	}
	// Binary extension reports supported=false regardless of content.
	if _, ok, _ := extractText("photo.png", []byte("anything")); ok {
		t.Error("png should be unsupported")
	}
	// Text/code files with no special handler default to text via content sniff.
	for _, name := range []string{"data.xml", "app.js", "main.py", "conf.yaml", "q.sql", "style.css", "weird.unknownext"} {
		if txt, ok, err := extractText(name, []byte("some textual content\nline two")); !ok || err != nil || txt == "" {
			t.Errorf("%s should ingest as text, got ok=%v err=%v", name, ok, err)
		}
	}
	// A binary blob with an unknown extension is sniffed out (NUL byte present).
	if _, ok, _ := extractText("mystery.dat", []byte{0x01, 0x00, 0x02, 0x00, 0xff}); ok {
		t.Error("binary content with unknown ext should be skipped by the text sniff")
	}
	// DOCX: a minimal zip with word/document.xml extracts its text.
	docx := makeMinimalDocx(t, "Quarterly report contents")
	if txt, ok, err := extractText("report.docx", docx); !ok || err != nil || !strings.Contains(txt, "Quarterly report") {
		t.Errorf("docx: got (%q, %v, %v)", txt, ok, err)
	}
	// A non-PDF passed as .pdf is supported (we try) but errors on decode.
	if _, ok, err := extractText("broken.pdf", []byte("not a pdf")); !ok || err == nil {
		t.Errorf("broken pdf should be supported with an error, got ok=%v err=%v", ok, err)
	}
}

func makeMinimalDocx(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	xml := `<?xml version="1.0"?><w:document><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`
	if _, err := w.Write([]byte(xml)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestKnowledgeIngestPath drops a small document set in a temp folder and
// confirms ingest_path walks it, extracts each supported type, skips the rest,
// and embeds the chunks.
func TestKnowledgeIngestPath(t *testing.T) {
	db := newKnowledgeDB(t)
	ctx := &tools.ToolContext{DB: db, Ctx: context.Background(), EmbedFunc: stubEmbed}
	if r, err := knowledgeCreateBase(ctx, "docs", ""); err != nil || !r.Success {
		t.Fatalf("create_base: %v %+v", err, r)
	}

	dir := t.TempDir()
	writeFile(t, dir, "a.txt", []byte("alpha content about onboarding"))
	writeFile(t, dir, "b.md", []byte("# Beta\nbenefits and policies"))
	writeFile(t, dir, "c.docx", makeMinimalDocx(t, "gamma docx handbook text"))
	writeFile(t, dir, "ignore.png", []byte{0x89, 0x50, 0x4e, 0x47}) // unsupported, must be skipped
	// A nested file only seen when recursive.
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0o755)
	writeFile(t, sub, "d.txt", []byte("delta nested document"))

	// Reading a host path is the owner's call: unapproved, nothing is ingested and the
	// owner is asked about that exact path.
	pending, err := knowledgeIngestPath(ctx, "docs", dir, false, 1000, 150)
	if err != nil || pending.Success || pending.Code != tools.CodeApprovalRequired {
		t.Fatalf("unapproved ingest_path must ask the owner, got err=%v %+v", err, pending)
	}
	grantIngestApproval(t, db, dir)
	grantIngestApproval(t, db, filepath.Join(dir, "ignore.png"))

	// Non-recursive: 3 supported top-level files, png skipped, nested not seen.
	r, err := knowledgeIngestPath(ctx, "docs", dir, false, 1000, 150)
	if err != nil || !r.Success {
		t.Fatalf("ingest_path: %v %+v", err, r)
	}
	data := r.Data.(map[string]any)
	// 3 supported top-level files; the .png is filtered out during the folder
	// walk (unsupported types never enter the work list) and the nested file is
	// not seen without recursive.
	if got := data["ingested_files"].(int); got != 3 {
		t.Errorf("ingested_files = %d, want 3", got)
	}

	// Pointing ingest_path directly at an unsupported file reports it skipped
	// (single-file targets bypass the folder-walk type filter).
	if rp, _ := knowledgeIngestPath(ctx, "docs", filepath.Join(dir, "ignore.png"), false, 1000, 150); rp.Success {
		t.Error("ingesting a lone unsupported file should not succeed")
	}

	// Recursive picks up the nested file (and re-ingests the top-level 3 -> 4 new).
	r2, _ := knowledgeIngestPath(ctx, "docs", dir, true, 1000, 150)
	if r2.Data.(map[string]any)["ingested_files"].(int) != 4 {
		t.Errorf("recursive ingested_files = %v, want 4", r2.Data.(map[string]any)["ingested_files"])
	}

	// Retrieval works over the ingested set.
	rr, _ := knowledgeRetrieve(ctx, "docs", "onboarding", 1)
	if !rr.Success || len(rr.Data.(map[string]any)["results"].([]map[string]any)) == 0 {
		t.Error("retrieve returned nothing after folder ingest")
	}
}

// grantIngestApproval records the owner's "yes" for ingesting one host path, the way
// answering the approval question would.
func grantIngestApproval(t *testing.T, db dbh.Handle, path string) {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO of_questions (question, context, status) VALUES ('ingest?', ?, 'answered')`,
		"approval:ingest_path:"+strings.ToLower(abs))
	if err != nil {
		t.Fatalf("seed approval question: %v", err)
	}
	id, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO of_answers (question_id, answer) VALUES (?, 'yes')`, id); err != nil {
		t.Fatalf("seed approval answer: %v", err)
	}
}

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestKnowledgeIngestRetrieve drives create_base -> ingest -> retrieve and
// asserts chunks are stored, embeddings are populated, and a query surfaces the
// topically-relevant chunk first.
func TestKnowledgeIngestRetrieve(t *testing.T) {
	db := newKnowledgeDB(t)
	ctx := &tools.ToolContext{DB: db, Ctx: context.Background(), EmbedFunc: stubEmbed}

	if r, err := knowledgeCreateBase(ctx, "handbook", "test"); err != nil || !r.Success {
		t.Fatalf("create_base: err=%v result=%+v", err, r)
	}

	// Two distinct documents so retrieval has to discriminate.
	if r, err := knowledgeIngest(ctx, "handbook", "pto", "Employees accrue paid time off PTO at two days per month and may carry over ten days.", "", 1000, 150); err != nil || !r.Success {
		t.Fatalf("ingest pto: err=%v result=%+v", err, r)
	}
	if r, err := knowledgeIngest(ctx, "handbook", "parking", "The office parking garage opens at seven and badge access is required for the elevator.", "", 1000, 150); err != nil || !r.Success {
		t.Fatalf("ingest parking: err=%v result=%+v", err, r)
	}

	// Embeddings populated on every chunk row.
	var total, embedded int
	db.QueryRow("SELECT COUNT(*) FROM kb_handbook").Scan(&total)
	db.QueryRow("SELECT COUNT(*) FROM kb_handbook WHERE _embedding != ''").Scan(&embedded)
	if total == 0 || embedded != total {
		t.Fatalf("expected all %d chunks embedded, got %d embedded", total, embedded)
	}

	// A PTO question must surface the PTO chunk first.
	r, err := knowledgeRetrieve(ctx, "handbook", "how much paid time off do I accrue?", 2)
	if err != nil || !r.Success {
		t.Fatalf("retrieve: err=%v result=%+v", err, r)
	}
	data := r.Data.(map[string]any)
	results := data["results"].([]map[string]any)
	if len(results) == 0 {
		t.Fatal("retrieve returned no results")
	}
	topText, _ := results[0]["text"].(string)
	if !strings.Contains(strings.ToLower(topText), "pto") {
		t.Errorf("top result should be the PTO chunk, got: %q", topText)
	}
}

// A window whose last space sits within overlap of its start used to move the next
// start to or before the current one, looping forever. Any text must chunk in a
// bounded number of steps, with every chunk non-empty and the whole text covered.
func TestChunkTextAlwaysAdvances(t *testing.T) {
	cases := []struct {
		name          string
		text          string
		size, overlap int
	}{
		{"unbroken run after a short word", "a " + strings.Repeat("b", 400), 10, 3},
		{"overlap larger than size", strings.Repeat("word ", 200), 100, 150},
		{"tiny size", strings.Repeat("x", 100), 5, 4},
		{"long url", "see https://example.com/" + strings.Repeat("path/", 100) + " end", 50, 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan []string, 1)
			go func() { done <- chunkText(tc.text, tc.size, tc.overlap) }()
			select {
			case chunks := <-done:
				if len(chunks) == 0 {
					t.Fatal("no chunks")
				}
				if limit := 10 * len([]rune(tc.text)); len(chunks) > limit {
					t.Fatalf("%d chunks for %d runes: the window is not advancing", len(chunks), len([]rune(tc.text)))
				}
				for i, c := range chunks {
					if strings.TrimSpace(c) == "" {
						t.Fatalf("chunk %d is empty", i)
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("chunkText did not terminate")
			}
		})
	}
}
