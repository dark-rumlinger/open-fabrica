package content

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"openfabrica/store"
)

func TestSanitizePDFFilename(t *testing.T) {
	cases := []struct {
		in, fallback, want string
	}{
		{"invoice-5", "document", "invoice-5.pdf"},
		{"invoice-5.pdf", "document", "invoice-5.pdf"},
		{"", "document", "document.pdf"},
		{"  ", "document", "document.pdf"},
		{"../../etc/passwd", "document", "passwd.pdf"},
		{"reports/2026/q1", "document", "q1.pdf"},
		{"weird name!!@#$.pdf", "document", "weird-name.pdf"},
		{"...", "document", "document.pdf"},
	}
	for _, c := range cases {
		if got := sanitizePDFFilename(c.in, c.fallback); got != c.want {
			t.Errorf("sanitizePDFFilename(%q, %q) = %q, want %q", c.in, c.fallback, got, c.want)
		}
	}
}

// browserFail must tag a missing-Chrome error with the stable browser_unavailable
// code so the LLM treats it as an environment gap, not a project bug; any other
// error stays an untagged failure.
func TestBrowserFailClassifiesUnavailable(t *testing.T) {
	res := browserFail(fmt.Errorf("launch: %w", store.ErrBrowserUnavailable))
	if res.Success {
		t.Fatal("browser_unavailable should not be a success result")
	}
	if res.Code != "browser_unavailable" {
		t.Errorf("Code = %q, want browser_unavailable", res.Code)
	}

	other := browserFail(errors.New("some render glitch"))
	if other.Code == "browser_unavailable" {
		t.Error("a non-browser error must not be tagged browser_unavailable")
	}
}

// Integration: with a real Chrome/Edge on the host, an inline HTML string
// renders to genuine PDF bytes. Skipped on hosts without a browser so the unit
// suite stays green in headless CI - the same degrade-gracefully contract the
// runtime probe relies on.
func TestRenderHTMLToPDF_Integration(t *testing.T) {
	if store.DetectChromePath() == "" {
		t.Skip("no Chrome/Edge binary on host; skipping headless PDF render")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	html := `<html><body style="font-family:sans-serif"><h1>Receipt</h1><p>Total: $42.00</p></body></html>`
	data, err := store.RenderHTMLToPDF(ctx, store.BrowserConfig{}, html, store.PDFOptions{PageSize: "A4"})
	if errors.Is(err, store.ErrBrowserUnavailable) {
		// Chrome is present but could not launch/connect here (common on loaded
		// CI runners, and more so under -race). Same degrade-gracefully contract
		// as the DetectChromePath guard above: skip rather than fail.
		t.Skipf("headless browser could not launch on this host: %v", err)
	}
	if err != nil {
		t.Fatalf("RenderHTMLToPDF: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("render produced no bytes")
	}
	// Every PDF starts with the %PDF- magic header.
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		t.Errorf("output is not a PDF (first bytes: %q)", data[:min(8, len(data))])
	}
}
