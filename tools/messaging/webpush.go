package messaging

// Web Push (RFC 8030 / RFC 8291) implementation. Self-contained on stdlib crypto
// so push adds no external dep.
//
// Flow per subscriber:
//   1. ECDH(P-256) between our ephemeral key and the subscriber's p256dh
//      public key, mixed with the subscriber's auth secret -> content key.
//   2. AES-128-GCM encrypt the JSON payload.
//   3. POST to subscription endpoint URL with:
//        Authorization: vapid t=<JWT>, k=<base64url(server public key)>
//        Crypto-Key:    p256ecdsa=<...> (server identity for VAPID)
//        Encryption:    salt=<base64url(salt)>
//        Content-Encoding: aes128gcm

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"crypto/aes"
	"crypto/cipher"

	"openfabrica/internal/netguard"
)

// SendWebPush delivers `payload` to a single subscriber. Returns (gone, err):
// `gone` is true when the push service returns 404/410, signalling the
// caller should prune the subscription row.
//
// vapidPubB64URL / vapidPrivB64URL are the project-scoped VAPID keypair
// (base64url-encoded raw 65-byte uncompressed P-256 public key and 32-byte
// scalar private key, respectively).
func SendWebPush(client *http.Client, subscriptionURL, p256dhB64URL, authSecretB64URL string,
	payload []byte, vapidPubB64URL, vapidPrivB64URL, contactEmail string) (gone bool, err error) {

	// Defense-in-depth against SSRF: the subscription endpoint is supplied by a
	// browser PushSubscription and is therefore attacker-influenceable. Reject
	// endpoints resolving to loopback/private/link-local addresses before we dial
	// (blocking reach into the admin panel, RFC-1918 hosts, or cloud metadata),
	// and fall back to the SSRF-hardened client - which re-checks at connect time
	// and on redirects - when the caller passes none. Mirrors the guard every
	// other user-URL egress path uses (triggers, webhooks, jobs).
	if err := netguard.ValidateExternalURL(subscriptionURL); err != nil {
		return false, fmt.Errorf("push endpoint rejected: %w", err)
	}
	if client == nil {
		client = netguard.SSRFSafeClient()
	}

	subPub, err := decodeB64URL(p256dhB64URL)
	if err != nil || len(subPub) != 65 {
		return false, fmt.Errorf("invalid p256dh")
	}
	authSecret, err := decodeB64URL(authSecretB64URL)
	if err != nil || len(authSecret) != 16 {
		return false, fmt.Errorf("invalid auth secret")
	}

	ciphertext, ephPub, salt, err := encryptAES128GCM(subPub, authSecret, payload)
	if err != nil {
		return false, fmt.Errorf("encrypt: %w", err)
	}

	// Build aes128gcm record body: salt(16) || rs(4) || idlen(1) || keyid(idlen) || ciphertext.
	body := bytes.NewBuffer(nil)
	body.Write(salt)                                       // 16 bytes
	_ = binary.Write(body, binary.BigEndian, uint32(4096)) // rs; bytes.Buffer writes cannot fail
	body.WriteByte(byte(len(ephPub)))                      // idlen
	body.Write(ephPub)                                     // keyid = server public key
	body.Write(ciphertext)

	req, err := http.NewRequest("POST", subscriptionURL, body)
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400")

	vapidJWT, vapidPubHdr, err := signVAPID(subscriptionURL, contactEmail, vapidPubB64URL, vapidPrivB64URL)
	if err != nil {
		return false, fmt.Errorf("vapid sign: %w", err)
	}
	req.Header.Set("Authorization", "vapid t="+vapidJWT+", k="+vapidPubHdr)

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(io.LimitReader(resp.Body, 2048)) // drain body so the connection is reused

	if resp.StatusCode == 404 || resp.StatusCode == 410 {
		return true, fmt.Errorf("subscription gone: %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("push service: %d", resp.StatusCode)
	}
	return false, nil
}

// GenerateVAPIDKeypair returns (publicKeyB64URL, privateScalarB64URL).
// Callers store the private piece in of_secrets and the public piece in
// of_push_endpoints.vapid_public_key.
func GenerateVAPIDKeypair() (string, string, error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	// Bytes() encodings match the stored format: 65-byte uncompressed point,
	// 32-byte fixed-length scalar.
	return encodeB64URL(priv.PublicKey().Bytes()), encodeB64URL(priv.Bytes()), nil
}

// ErrPushGone is the sentinel returned by SendWebPush when the push service
// reports the subscription is no longer deliverable.
var ErrPushGone = errors.New("push subscription gone")

// VAPID JWT signing

func signVAPID(subscriptionURL, contactEmail, vapidPubB64URL, vapidPrivB64URL string) (string, string, error) {
	u, err := url.Parse(subscriptionURL)
	if err != nil {
		return "", "", err
	}
	aud := u.Scheme + "://" + u.Host

	header := `{"typ":"JWT","alg":"ES256"}`
	claims := map[string]any{
		"aud": aud,
		"exp": time.Now().Add(12 * time.Hour).Unix(),
		"sub": "mailto:" + contactEmail,
	}
	if contactEmail == "" {
		// No abuse contact configured for this endpoint. Fall back to the
		// RFC 2606 reserved documentation domain, which asserts ownership of
		// no real domain. Operators should set contact_email per push endpoint
		// so a push service can reach a real address about abuse.
		claims["sub"] = "mailto:no-reply@example.com"
	}
	cb, _ := json.Marshal(claims)
	signingInput := encodeB64URL([]byte(header)) + "." + encodeB64URL(cb)

	privBytes, err := decodeB64URL(vapidPrivB64URL)
	if err != nil || len(privBytes) != 32 {
		return "", "", fmt.Errorf("invalid vapid private key")
	}
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), privBytes)
	if err != nil {
		return "", "", fmt.Errorf("invalid vapid private key")
	}

	hash := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, priv, hash[:])
	if err != nil {
		return "", "", err
	}
	// ES256 wants a 64-byte (R || S) signature, NOT ASN.1.
	rPad := r.FillBytes(make([]byte, 32))
	sPad := s.FillBytes(make([]byte, 32))
	sig := append(rPad, sPad...)
	jwt := signingInput + "." + encodeB64URL(sig)
	return jwt, vapidPubB64URL, nil
}

// AES-128-GCM payload encryption (aes128gcm content-encoding)

func encryptAES128GCM(subPub65, authSecret, payload []byte) (ciphertext, ephPub, salt []byte, err error) {
	curve := ecdh.P256()
	ephPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	subPubKey, err := curve.NewPublicKey(subPub65)
	if err != nil {
		return nil, nil, nil, err
	}
	sharedSecret, err := ephPriv.ECDH(subPubKey)
	if err != nil {
		return nil, nil, nil, err
	}

	salt = make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, nil, err
	}

	ephPub = ephPriv.PublicKey().Bytes() // 65 bytes uncompressed
	// PRK_key = HMAC(authSecret, sharedSecret)
	// keyInfo = "WebPush: info" || 0x00 || ua_public || as_public
	// IKM = HMAC(PRK_key, keyInfo || 0x01)
	// PRK = HMAC(salt, IKM)
	// cek = HMAC(PRK, "Content-Encoding: aes128gcm" || 0x00 || 0x01) [:16]
	// nonce = HMAC(PRK, "Content-Encoding: nonce" || 0x00 || 0x01) [:12]

	prkKey := hmacSHA256(authSecret, sharedSecret)
	keyInfo := append([]byte("WebPush: info\x00"), subPub65...)
	keyInfo = append(keyInfo, ephPub...)
	ikm := hmacSHA256(prkKey, append(keyInfo, 0x01))
	prk := hmacSHA256(salt, ikm)

	cek := hmacSHA256(prk, []byte("Content-Encoding: aes128gcm\x00\x01"))[:16]
	nonce := hmacSHA256(prk, []byte("Content-Encoding: nonce\x00\x01"))[:12]

	// aes128gcm record: payload || 0x02 || padding (we use no padding -> single record terminator 0x02)
	plain := append(payload, 0x02)

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, err
	}
	ciphertext = aead.Seal(nil, nonce, plain, nil)
	return ciphertext, ephPub, salt, nil
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// base64url helpers (no padding, RFC 7515)

func encodeB64URL(b []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
}

func decodeB64URL(s string) ([]byte, error) {
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(s)
}
