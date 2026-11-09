package security

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestJWT_GenerateForProject_NoTenantClaim(t *testing.T) {
	jm := NewJWTManager("test-secret", time.Hour)
	token, err := jm.Generate(TokenRequest{ProjectID: 1, UserID: 42, Username: "alice", Role: "user"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	c, err := jm.Validate(token)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.UserID != 42 || c.ProjectID != 1 || c.Role != "user" {
		t.Errorf("unexpected claims: %+v", c)
	}
	if c.TenantID != 0 || c.TenantRole != "" {
		t.Errorf("expected zero-value tenant claims for non-tenanted token, got tenant_id=%d tenant_role=%q", c.TenantID, c.TenantRole)
	}
}

func TestJWT_GenerateAdmin_CarriesVersionAndIsUnscoped(t *testing.T) {
	jm := NewJWTManager("test-secret", time.Hour)
	token, err := jm.Generate(TokenRequest{UserID: 7, Username: "root", Role: "admin", TokenVersion: 3})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	c, err := jm.Validate(token)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// Admin tokens must be unscoped so the admin guard's ProjectID==0 check holds.
	if c.ProjectID != 0 {
		t.Errorf("admin token must have ProjectID 0, got %d", c.ProjectID)
	}
	if c.UserID != 7 || c.Role != "admin" {
		t.Errorf("unexpected claims: %+v", c)
	}
	if c.TokenVersion != 3 {
		t.Errorf("token_version not round-tripped: got %d want 3", c.TokenVersion)
	}
}

func TestJWT_GenerateForTenant_RoundTrips(t *testing.T) {
	jm := NewJWTManager("test-secret", time.Hour)
	token, err := jm.Generate(TokenRequest{ProjectID: 1, UserID: 42, TenantID: 7, Username: "alice", Role: "user", TenantRole: "owner"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	c, err := jm.Validate(token)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.TenantID != 7 || c.TenantRole != "owner" {
		t.Errorf("tenant claims wrong: %+v", c)
	}
	if c.UserID != 42 || c.Role != "user" {
		t.Errorf("user claims wrong: %+v", c)
	}
}

func TestJWT_GenerateForProjectWithExpiry_OverridesAndFallsBack(t *testing.T) {
	jm := NewJWTManager("test-secret", time.Hour)

	// A positive override wins over the manager default (1h here).
	tok, err := jm.Generate(TokenRequest{ProjectID: 1, UserID: 42, Username: "alice", Role: "user", Expiry: 3 * time.Hour})
	if err != nil {
		t.Fatalf("generate with expiry: %v", err)
	}
	c, err := jm.Validate(tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if ttl := time.Until(c.ExpiresAt.Time); ttl < 2*time.Hour+50*time.Minute || ttl > 3*time.Hour+time.Minute {
		t.Errorf("override expiry not applied: ttl=%s want ~3h", ttl)
	}

	// A non-positive override falls back to the manager default.
	tok2, err := jm.Generate(TokenRequest{ProjectID: 1, UserID: 42, Username: "alice", Role: "user", Expiry: 0})
	if err != nil {
		t.Fatalf("generate default expiry: %v", err)
	}
	c2, err := jm.Validate(tok2)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if ttl := time.Until(c2.ExpiresAt.Time); ttl < 50*time.Minute || ttl > time.Hour+time.Minute {
		t.Errorf("fallback expiry wrong: ttl=%s want ~1h", ttl)
	}
}

// TestJWT_Validate_RejectsTokenWithoutExpiration pins the WithExpirationRequired
// hardening: a token that is correctly signed but carries no exp claim (a
// non-expiring credential) must be refused, even though every current mint path
// sets exp.
func TestJWT_Validate_RejectsTokenWithoutExpiration(t *testing.T) {
	const secret = "test-secret-at-least-long-enough"
	jm := NewJWTManager(secret, time.Hour)

	// Mint a valid-signature token with no exp claim.
	raw := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID:   1,
		Username: "alice",
		Role:     "user",
	})
	signed, err := raw.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := jm.Validate(signed); err == nil {
		t.Fatal("expected Validate to reject a token with no exp claim")
	}
}

func TestTenantFilterClause(t *testing.T) {
	if got, _ := TenantFilterClause(""); got != "" {
		t.Errorf("empty input should yield empty clause, got %q", got)
	}
	clause, err := TenantFilterClause("tenant_id")
	if err != nil {
		t.Fatalf("valid column: %v", err)
	}
	if clause != `"tenant_id" = ?` {
		t.Errorf("clause shape wrong: %q", clause)
	}
	if _, err := TenantFilterClause("tenant_id; DROP TABLE x"); err == nil {
		t.Fatal("expected error on injection attempt")
	}
}

// Session revocation compares a token's issue time with the instant of the
// revocation, so tokens must carry more than whole-second resolution: two tokens
// minted a few milliseconds apart must be distinguishable, and the issue time must
// survive the round trip through the token.
func TestTokenIssueTimeHasMillisecondPrecision(t *testing.T) {
	m := NewJWTManager("test-jwt-secret-at-least-32-bytes-long", time.Hour)
	mint := func() *Claims {
		tok, err := m.Generate(TokenRequest{ProjectID: 1, UserID: 1, Username: "u", Role: "user"})
		if err != nil {
			t.Fatal(err)
		}
		c, err := m.Validate(tok)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first := mint()
	time.Sleep(5 * time.Millisecond)
	second := mint()

	if first.IssuedAt == nil || second.IssuedAt == nil {
		t.Fatal("a token carries no issue time")
	}
	if !second.IssuedAt.Time.After(first.IssuedAt.Time) {
		t.Errorf("tokens minted 5ms apart have issue times %v and %v: the resolution is too coarse to order a revocation",
			first.IssuedAt.Time, second.IssuedAt.Time)
	}
	// Expiry keeps working at the finer resolution.
	if d := second.ExpiresAt.Time.Sub(second.IssuedAt.Time); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("token lifetime = %v, want about an hour", d)
	}
}
