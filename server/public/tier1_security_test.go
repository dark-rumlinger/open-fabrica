package public

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCaseInsensitiveColumnGuards(t *testing.T) {
	ep := &apiEndpoint{
		SecureCols:   map[string]string{"password_hash": "hash", "api_secret": "encrypt"},
		OwnerColumn:  "user_id",
		TenantColumn: "tenant_id",
	}

	// ?fields= must strip secure columns regardless of casing.
	r := httptest.NewRequest("GET", "/api/users?fields=id,name,PASSWORD_HASH,Api_Secret", nil)
	gotFields := parseFieldsParam(r, ep)
	if strings.Contains(strings.ToLower(gotFields), "password_hash") || strings.Contains(strings.ToLower(gotFields), "api_secret") {
		t.Fatalf("parseFieldsParam leaked secure column: %q", gotFields)
	}

	// stripSecureCols must remove mixed-case keys returned by SQLite.
	rows := []map[string]any{{"id": 1, "PASSWORD_HASH": "$2a$...", "Api_Secret": "enc", "name": "ada"}}
	stripSecureCols(rows, ep.SecureCols)
	if _, ok := rows[0]["PASSWORD_HASH"]; ok {
		t.Fatal("stripSecureCols failed to remove PASSWORD_HASH")
	}
	if _, ok := rows[0]["Api_Secret"]; ok {
		t.Fatal("stripSecureCols failed to remove Api_Secret")
	}

	// stripScope must remove mixed-case owner/tenant columns on PUT.
	body := map[string]any{"User_ID": 99, "TENANT_ID": 77, "title": "ok"}
	stripScope(ep, body)
	if _, ok := body["User_ID"]; ok {
		t.Fatal("stripScope failed to remove User_ID")
	}
	if _, ok := body["TENANT_ID"]; ok {
		t.Fatal("stripScope failed to remove TENANT_ID")
	}
}

func TestDisallowedFunctionResponseHeaders(t *testing.T) {
	for _, h := range []string{
		"Content-Security-Policy",
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Credentials",
		"X-Frame-Options",
		"X-Content-Type-Options",
		"Strict-Transport-Security",
		"Cross-Origin-Opener-Policy",
		"Set-Cookie",
	} {
		if !disallowedResponseHeader(h) {
			t.Errorf("expected %q to be disallowed in function responses", h)
		}
	}
	if disallowedResponseHeader("X-Custom-Header") {
		t.Error("expected X-Custom-Header to be allowed")
	}
}

func TestSanitizeUploadExt(t *testing.T) {
	if got := sanitizeUploadExt(".PNG"); got != ".png" {
		t.Fatalf("want .png, got %q", got)
	}
	for _, bad := range []string{".png/../../x", ".a b", ".toolongextension", "png", "."} {
		if got := sanitizeUploadExt(bad); got != "" {
			t.Errorf("expected %q to be rejected, got %q", bad, got)
		}
	}
}
