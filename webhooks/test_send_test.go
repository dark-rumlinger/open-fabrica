package webhooks

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/security"
)

func TestSendTestSignsAndReportsStatus(t *testing.T) {
	var gotSig, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotSig = r.Header.Get("X-Webhook-Signature")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	res := sendTest(context.Background(), srv.Client(), false, srv.URL, "whsec-test", 7, "orders")
	if !res.Success || res.StatusCode != http.StatusNoContent {
		t.Fatalf("result = %+v, want success with 204", res)
	}
	if !strings.Contains(gotBody, `"event":"test"`) {
		t.Errorf("body %q does not carry the test event", gotBody)
	}
	if want := "sha256=" + security.HMACSHA256Hex([]byte("whsec-test"), []byte(gotBody)); gotSig != want {
		t.Errorf("signature %q, want %q", gotSig, want)
	}
}

func TestSendTestUnsignedWithoutSecret(t *testing.T) {
	var sawSig bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawSig = r.Header["X-Webhook-Signature"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if res := sendTest(context.Background(), srv.Client(), false, srv.URL, "", 1, "x"); !res.Success {
		t.Fatalf("result = %+v, want success", res)
	}
	if sawSig {
		t.Error("a signature header was sent although the webhook has no secret")
	}
}

func TestSendTestReportsRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
	}))
	defer srv.Close()

	res := sendTest(context.Background(), srv.Client(), false, srv.URL, "s", 1, "x")
	if res.Success || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("result = %+v, want a failed 401", res)
	}
	if !strings.Contains(res.Response, "bad signature") {
		t.Errorf("response %q should carry the receiver's reason", res.Response)
	}
}

func TestSendTestRefusesPrivateTargets(t *testing.T) {
	res := SendTest(context.Background(), "http://127.0.0.1:1/hook", "", 1, "x")
	if res.Success || res.Error == "" {
		t.Fatalf("result = %+v, want the SSRF guard to refuse a loopback URL", res)
	}
}
