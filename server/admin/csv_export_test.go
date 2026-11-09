package admin

import (
	"bytes"
	"encoding/csv"
	"net/http/httptest"
	"strings"
	"testing"
)

// An export opens correctly in Excel: UTF-8 is announced by a byte-order mark, and
// the text after it round-trips for every script the admin offers.
func TestCSVDownloadStartsWithAByteOrderMark(t *testing.T) {
	rec := httptest.NewRecorder()
	startCSV(rec, "orders")
	w := csv.NewWriter(rec)
	_ = w.Write([]string{"имя", "名前", "الاسم", "नाम", "name"})
	w.Flush()

	body := rec.Body.Bytes()
	if !bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatalf("no byte-order mark: % x", body[:min(len(body), 6)])
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Errorf("content type %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != "attachment; filename=orders.csv" {
		t.Errorf("disposition %q", got)
	}
	rest := strings.TrimPrefix(string(body), utf8BOM)
	if rest != "имя,名前,الاسم,नाम,name\n" {
		t.Errorf("text after the mark: %q", rest)
	}
}
