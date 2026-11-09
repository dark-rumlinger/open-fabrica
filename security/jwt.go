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
	// TokenVersion pins the token to the issuing user's token_version at mint
	// time. The admin auth guard rejects the token once the stored version moves
	// ahead (password change / sign-out-everywhere). Only meaningful for admin
	// (project 0) tokens today.
	TokenVersion int `json:"tv,omitempty"`
	jwt.RegisteredClaims
}

type JWTManager struct {
	secret        []byte
	expiry        time.Duration
	SecureCookies bool
}

func NewJWTManager(secret string, expiry time.Duration) *JWTManager {
	return &JWTManager{
		secret: []byte(secret),
		expiry: expiry,
	}
}

// TokenRequest describes a token to mint. Zero-value fields are omitted from the
// claims (see the omitempty tags on Claims), so a plain project login leaves
// TenantID and TokenVersion unset. ProjectID 0 is an admin/unscoped token. A
// non-positive Expiry falls back to the manager default.
type TokenRequest struct {
	ProjectID    int
	UserID       int
	Username     string
	Role         string
	TenantID     int
	TenantRole   string
	TokenVersion int
	Expiry       time.Duration
}

// Generate mints a signed JWT for req.
func (j *JWTManager) Generate(req TokenRequest) (string, error) {
	expiry := req.Expiry
	if expiry <= 0 {
		expiry = j.expiry
	}
	claims := Claims{
		ProjectID:    req.ProjectID,
		UserID:       req.UserID,
		Username:     req.Username,
		Role:         req.Role,
		TenantID:     req.TenantID,
		TenantRole:   req.TenantRole,
		TokenVersion: req.TokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(j.secret)
}

// AuthCookieName is the name of the HttpOnly cookie used for SSE auth.
const AuthCookieName = "of_auth"

// SetAuthCookie writes an HttpOnly, SameSite=Strict cookie with the given token.
func (j *JWTManager) SetAuthCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    token,
		Path:     "/admin",
		MaxAge:   int(j.expiry.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   j.SecureCookies,
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

	return claims, nil
}
