package data

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ledongthuc/pdf"
)

// Document text extraction for knowledge ingestion. Dispatches by file
// extension: plain-text formats pass through, HTML is tag-stripped, and PDF /
// DOCX are decoded to their text layer. All pure-Go (no cgo), matching the
// single-binary build. Scanned/image-only PDFs carry no text layer, so they
// extract to little or nothing - there is no OCR step.

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

// maxExtractBytes caps the decompressed text a single document may yield. Upload
// paths bound the compressed bytes, but a small archive (a .docx is a zip, a PDF
// can hold packed streams) can inflate to gigabytes, so the decoder reads through
// an io.LimitReader and rejects anything past the cap instead of buffering it. A
// var, not a const, so a test can lower it without allocating the full cap.
var maxExtractBytes = 64 << 20 // 64 MiB

// extractText returns the plain text of a document's bytes. Rather than an
// allowlist of extensions (always incomplete), it special-cases the formats that
// need decoding (PDF, DOCX) or cleanup (HTML), excludes known binary types, and
// otherwise ingests anything that looks like text - so .xml, .js, .ts, .py,
// .yaml, .sql, .css, and any other UTF-8 text/code file just work.
//   - supported=false  -> binary / not text (skip it)
//   - supported=true, err!=nil -> a format we tried but failed to decode
//   - supported=true, err==nil -> text is the content (may be empty for a
//     scanned PDF with no text layer)
func extractText(filename string, data []byte) (text string, supported bool, err error) {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".pdf":
		t, err := extractPDF(data)
		return t, true, err
	case ".docx":
		t, err := extractDocx(data)
		return t, true, err
	case ".html", ".htm", ".xhtml":
		return stripHTML(string(data)), true, nil
	}
	if isBinaryExt(filename) {
		return "", false, nil
	}
	if !looksLikeText(data) {
		return "", false, nil
	}
	return string(data), true, nil
}

// binaryExts are extensions we never treat as text: media, archives,
// executables, fonts, and binary/zip office formats we don't decode. PDF and
// DOCX are handled above, so they are deliberately absent here.
var binaryExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".bmp": true, ".ico": true, ".tif": true, ".tiff": true, ".heic": true,
	".mp3": true, ".wav": true, ".ogg": true, ".flac": true, ".m4a": true,
	".aac": true, ".mp4": true, ".mov": true, ".avi": true, ".mkv": true,
	".webm": true, ".wmv": true, ".flv": true,
	".zip": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true,
	".7z": true, ".rar": true, ".tar": true, ".jar": true, ".war": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".bin": true,
	".o": true, ".a": true, ".class": true, ".wasm": true, ".pyc": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".doc": true, ".xls": true, ".ppt": true, ".xlsx": true, ".pptx": true,
	".db": true, ".sqlite": true,
}

func isBinaryExt(filename string) bool {
	return binaryExts[strings.ToLower(filepath.Ext(filename))]
}

// looksLikeText reports whether bytes are plausibly text: no NUL byte and a low
// proportion of non-whitespace control characters in the leading sample. Cheap
// content sniff that lets unknown extensions through when they're really text
// and skips binaries that slip past the extension denylist.
func looksLikeText(data []byte) bool {
	if len(data) == 0 {
		return false // nothing to ingest
	}
	sample := data
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return false
	}
	nonText := 0
	for _, b := range sample {
		if b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if b < 0x20 || b == 0x7f {
			nonText++
		}
	}
	return float64(nonText)/float64(len(sample)) <= 0.10
}

func stripHTML(s string) string {
	return strings.TrimSpace(htmlTagRe.ReplaceAllString(s, " "))
}

// extractPDF pulls the text layer out of a PDF. The underlying parser can panic
// on malformed input, so a recover turns that into an ordinary error rather
// than taking down the caller.
func extractPDF(data []byte) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = "", fmt.Errorf("pdf parse panicked: %v", r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	b, err := r.GetPlainText()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(b, int64(maxExtractBytes)+1)); err != nil {
		return "", err
	}
	if buf.Len() > maxExtractBytes {
		return "", fmt.Errorf("pdf text exceeds %d bytes", maxExtractBytes)
	}
	return buf.String(), nil
}

// extractDocx reads the main document part of a .docx (a zip of XML) and strips
// it to text, treating paragraph and line-break tags as newlines.
func extractDocx(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		raw, err := io.ReadAll(io.LimitReader(rc, int64(maxExtractBytes)+1))
		rc.Close()
		if err != nil {
			return "", err
		}
		if len(raw) > maxExtractBytes {
			return "", fmt.Errorf("docx document.xml exceeds %d bytes", maxExtractBytes)
		}
		s := string(raw)
		s = strings.ReplaceAll(s, "</w:p>", "\n")
		s = strings.ReplaceAll(s, "<w:br/>", "\n")
		s = strings.ReplaceAll(s, "<w:br />", "\n")
		return strings.TrimSpace(htmlTagRe.ReplaceAllString(s, "")), nil
	}
	return "", fmt.Errorf("no word/document.xml found in docx")
}
