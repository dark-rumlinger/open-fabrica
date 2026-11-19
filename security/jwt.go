package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func init() {
	// Token times carry millisecond precision instead of the library's default
	// whole seconds. Revoking a user's sessions compares a token's issue time with
	// the instant of the revocation, and at one-second resolution a token minted in
	// the same second as a password reset could not be told from one minted just
	// before it. This is a package-wide setting of the JWT library; it only changes
	// how times are written (as a decimal), and parsing accepts both forms.
	jwt.TimePrecision = time.Millisecond
}

type Claims struct {
	ProjectID int    `json:"project_id,omitempty"`
	UserID    int    `json:"user_id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	// TenantID names the active tenant (workspace) for this session. Zero
	// when the project has no tenancy enabled, or when the user has not
	// selected a tenant yet. Tenanted endpoints reject TenantID=0 sessions.
	TenantID int `json:"tenant_id,omitempty"`
	// TenantRole is the user's role within TenantID (owner/admin/member/...).
	// Empty when TenantID is zero. Distinct from Role (project-wide).
	TenantRole string `json:"tenant_role,omitempty"`
	// TokenVersion pins an admin token to the issuing user's token_version at
	// mint time. The admin auth guard rejects the token once the stored version
	// moves ahead (password change / sign-out-everywhere).
	TokenVersion int `json:"tv,omitempty"`
	jwt.RegisteredClaims
}

// The two kinds of token, carried in the audience claim. Both are signed with
// the same key, so the audience is what keeps a project user's token off the
// admin API and an admin token off a project's API.
const (
	AudienceAdmin   = "openfabrica-admin"
	AudienceProject = "openfabrica-project"
)

// ForAdmin reports whether this is a platform admin's token.
func (c *Claims) ForAdmin() bool {
	return c.ProjectID == 0 && len(c.Audience) == 1 && c.Audience[0] == AudienceAdmin
}

type JWTManager struct {
	secret          []byte
	expiry          time.Duration
	SecureCookies   bool
	secureCookiesFn func() bool
}

func NewJWTManager(secret string, expiry time.Duration) *JWTManager {
	return &JWTManager{
		secret: []byte(secret),
		expiry: expiry,
	}
}

// NewJWTManagerWithSecureCookies constructs a JWTManager whose auth cookies
// derive their Secure flag dynamically from secureCookies().
func NewJWTManagerWithSecureCookies(secret string, expiry time.Duration, secureCookies func() bool) *JWTManager {
	return &JWTManager{
		secret:          []byte(secret),
		expiry:          expiry,
		secureCookiesFn: secureCookies,
	}
}

// TokenRequest describes a project user's token. Zero-value fields are omitted
// from the claims (see the omitempty tags on Claims), so a plain project login
// leaves TenantID unset. A non-positive Expiry falls back to the manager default.
type TokenRequest struct {
	ProjectID  int
	UserID     int
	Username   string
	Role       string
	TenantID   int
	TenantRole string
	Expiry     time.Duration
}

// AdminTokenRequest describes a platform admin's token.
type AdminTokenRequest struct {
	UserID       int
	Username     string
	Role         string
	TokenVersion int
}

// Generate mints a project user's token. It refuses a request without a
// project: a token for the platform itself comes only from GenerateAdmin.
func (j *JWTManager) Generate(req TokenRequest) (string, error) {
	if req.ProjectID <= 0 {
		return "", fmt.Errorf("a project token needs a project id")
	}
	return j.sign(Claims{
		ProjectID:  req.ProjectID,
		UserID:     req.UserID,
		Username:   req.Username,
		Role:       req.Role,
		TenantID:   req.TenantID,
		TenantRole: req.TenantRole,
	}, AudienceProject, req.Expiry)
}

// GenerateAdmin mints a platform admin's token.
func (j *JWTManager) GenerateAdmin(req AdminTokenRequest) (string, error) {
	return j.sign(Claims{
		UserID:       req.UserID,
		Username:     req.Username,
		Role:         req.Role,
		TokenVersion: req.TokenVersion,
	}, AudienceAdmin, 0)
}

func (j *JWTManager) sign(claims Claims, audience string, expiry time.Duration) (string, error) {
	if expiry <= 0 {
		expiry = j.expiry
	}
	claims.RegisteredClaims = jwt.RegisteredClaims{
		Audience:  jwt.ClaimStrings{audience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(j.secret)
}

// AuthCookieName is the name of the HttpOnly cookie used for SSE auth.
const AuthCookieName = "of_auth"

// SetAuthCookie writes an HttpOnly, SameSite=Strict cookie with the given token.
func (j *JWTManager) SetAuthCookie(w http.ResponseWriter, token string) {
	secure := j.SecureCookies || (j.secureCookiesFn != nil && j.secureCookiesFn())
	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    token,
		Path:     "/admin",
		MaxAge:   int(j.expiry.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})
}

// ClearAuthCookie removes the auth cookie.
func ClearAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// HMAC computes HMAC-SHA256 of data using the JWT secret. Used for OAuth state signing.
func (j *JWTManager) HMAC(data []byte) []byte {
	mac := hmac.New(sha256.New, j.secret)
	mac.Write(data)
	return mac.Sum(nil)
}

func (j *JWTManager) Validate(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return j.secret, nil
	}, jwt.WithExpirationRequired()) // reject any token missing an exp claim, so a
	// non-expiring token can never be honored even if some future mint path forgets to set one.
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	// The audience must name the kind the claims describe: admin tokens carry
	// no project, project tokens always carry one.
	want := AudienceProject
	if claims.ProjectID == 0 {
		want = AudienceAdmin
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != want {
		return nil, fmt.Errorf("invalid token audience")
	}

	return claims, nil
}
