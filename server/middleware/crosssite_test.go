package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRejectCrossSite(t *testing.T) {
	h := RejectCrossSite(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"same-origin POST", "POST", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://localhost:5734"}, 200},
		{"POST without browser headers", "POST", nil, 200},
		{"sibling site POST", "POST", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://app.localhost:5733"}, 403},
		{"cross-site POST", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"foreign Origin without fetch metadata", "DELETE", map[string]string{"Origin": "http://evil.example"}, 403},
		{"opaque origin", "PUT", map[string]string{"Origin": "null"}, 403},
		{"proxy that rewrites Host", "POST", map[string]string{"Origin": "http://localhost:5734", "X-Forwarded-Host": "localhost:5734"}, 200},
		{"cross-site GET is not state-changing", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, 200},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://localhost:5734/admin/api/projects", nil)
		req.Host = "localhost:5734"
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}
}
