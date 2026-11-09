package data

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// makeDocx builds a minimal .docx: a zip with a single word/document.xml member
// holding the given body XML.
func makeDocx(t *testing.T, bodyXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write([]byte(bodyXML)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractDocxNormal(t *testing.T) {
	doc := makeDocx(t, `<w:document><w:body>`+
		`<w:p><w:r><w:t>Hello</w:t></w:r></w:p>`+
		`<w:p><w:r><w:t>World</w:t></w:r></w:p>`+
		`</w:body></w:document>`)
	got, err := extractDocx(doc)
	if err != nil {
		t.Fatalf("extractDocx: %v", err)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "World") {
		t.Fatalf("extracted text missing content: %q", got)
	}
}

// TestExtractDocxRejectsOversized pins the decompression cap: a small archive
// whose member inflates past the limit must error, not buffer the whole thing.
// The cap is lowered so the test stays cheap.
func TestExtractDocxRejectsOversized(t *testing.T) {
	orig := maxExtractBytes
	maxExtractBytes = 1 << 10 // 1 KiB
	defer func() { maxExtractBytes = orig }()

	// Highly compressible body that decompresses well past the cap.
	body := "<w:t>" + strings.Repeat("A", maxExtractBytes*4) + "</w:t>"
	doc := makeDocx(t, body)
	if _, err := extractDocx(doc); err == nil {
		t.Fatal("extractDocx accepted an over-cap document; want an error")
	}
}
