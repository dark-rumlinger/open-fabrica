package public

// An upload endpoint's rate_limit is enforced at request time.
//
// manage_uploads documents it as "Req/min per IP." and echoes the configured value
// back in its create result, so a config the handler ignores reads to the model, and
// to the owner, as a cap that is in force. Uploads are the disk-fill surface: without
// this the only backstop is the whole-project storage quota.

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"openfabrica/db"
)

func newUploadRateLimitHandler(t *testing.T, ratePerMin int) (*Handler, *db.ProjectDB) {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.Writer().Exec(
		`INSERT INTO of_upload_endpoints (path, allowed_types, max_size_mb, requires_auth, rate_limit)
		 VALUES ('photos', '[]', 5, 0, ?)`, ratePerMin,
	); err != nil {
		t.Fatalf("seed upload endpoint: %v", err)
	}
	return &Handler{deps: &Deps{ProjectDBManager: mgr}}, pdb
}

// uploadRequest builds a multipart upload from a fixed client IP.
func uploadRequest(t *testing.T, ip string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "a.txt")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := fw.Write([]byte("hello")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/photos/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.RemoteAddr = ip + ":40000"
	return r
}

func TestUploadRateLimitRejectsOverTheConfiguredCap(t *testing.T) {
	const cap = 3
	h, pdb := newUploadRateLimitHandler(t, cap)

	// A distinct IP per run: the limiter's window is process-global and keyed by
	// endpoint+IP, so a shared address would carry counts between tests.
	ip := "203.0.113.7"
	var limited bool
	for i := 0; i < cap+2; i++ {
		w := httptest.NewRecorder()
		h.apiUpload(w, uploadRequest(t, ip), 1, pdb, "photos")
		if w.Code == http.StatusTooManyRequests {
			limited = true
			if i < cap {
				t.Fatalf("request %d of a cap of %d was rate limited too early", i+1, cap)
			}
			break
		}
	}
	if !limited {
		t.Errorf("no request was rejected with 429 after exceeding rate_limit=%d - the documented "+
			"\"Req/min per IP\" cap is not enforced, leaving the upload endpoint uncapped", cap)
	}
}

// rate_limit=0 is the documented "no cap" value and must not reject.
func TestUploadRateLimitZeroDisablesTheCap(t *testing.T) {
	h, pdb := newUploadRateLimitHandler(t, 0)

	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		h.apiUpload(w, uploadRequest(t, fmt.Sprintf("198.51.100.%d", i)), 1, pdb, "photos")
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate limited though rate_limit=0 means no cap", i+1)
		}
	}
}
