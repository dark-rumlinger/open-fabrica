package public

import (
	"strings"
	"testing"
	"time"

	"openfabrica/security"
)

// The analytics visitor id is stable for one visitor, differs between visitors,
// and is keyed with the instance secret so it cannot be recomputed from an IP
// address alone.
func TestVisitorHashIsKeyed(t *testing.T) {
	keyed := &Handler{deps: &Deps{JWTManager: security.NewJWTManager("test-jwt-secret-at-least-32-bytes-long", time.Hour)}}
	other := &Handler{deps: &Deps{JWTManager: security.NewJWTManager("a-different-secret-also-32-bytes-long!", time.Hour)}}
	bare := &Handler{deps: &Deps{}}

	a := keyed.visitorHash("203.0.113.7", "Mozilla/5.0")
	if a != keyed.visitorHash("203.0.113.7", "Mozilla/5.0") {
		t.Error("the same visitor must hash to the same id")
	}
	if a == keyed.visitorHash("203.0.113.8", "Mozilla/5.0") {
		t.Error("different IPs collided")
	}
	if a == keyed.visitorHash("203.0.113.7", "curl/8") {
		t.Error("different user agents collided")
	}
	if len(a) != 16 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("visitor id %q is not 16 hex chars", a)
	}
	if a == other.visitorHash("203.0.113.7", "Mozilla/5.0") {
		t.Error("two instances produced the same id: the hash is not keyed")
	}
	if a == bare.visitorHash("203.0.113.7", "Mozilla/5.0") {
		t.Error("the keyed id equals the bare ip+user-agent hash")
	}
}
