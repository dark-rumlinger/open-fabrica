package tlsproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"
)

// selfSignedFor mints a throwaway certificate for the domain so a test can put
// the manager in the "issuance already worked" state without touching ACME.
func selfSignedFor(t *testing.T, domain string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating test certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing test certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// A loaded certificate means issuance worked and :443 is served, so a failed
// self-dial is almost always a router without NAT loopback. That must not be
// recorded as a failure: it is the single most common false alarm and would
// otherwise paint a healthy site red.
func TestVerdictUnverifiedWhenCertLoaded(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.magic.CacheUnmanagedTLSCertificate(context.Background(), selfSignedFor(t, "example.com"), nil); err != nil {
		t.Fatalf("caching test certificate: %v", err)
	}
	if !m.certLoadedFor("example.com") {
		t.Fatal("certLoadedFor = false right after caching a certificate for the domain")
	}

	st := m.verdictForFailedProbe("example.com", errors.New("dial tcp 203.0.113.7:443: i/o timeout"), time.Now())
	if st.State != StateUnverified {
		t.Errorf("State = %q, want %q", st.State, StateUnverified)
	}
	if st.Error != "" {
		t.Errorf("Error = %q, want empty: an unverifiable probe is not a failure", st.Error)
	}
	if st.Hint == "" {
		t.Error("Hint is empty, want the phone-check explanation")
	}
}

// Inside the issuance grace a failed dial means "ACME has not finished", not
// "broken". The first health probe runs 20s in and issuance regularly takes
// longer, so this window is hit on nearly every new domain.
func TestVerdictPendingInsideIssuanceGrace(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.since["example.com"] = time.Now()
	m.mu.Unlock()

	st := m.verdictForFailedProbe("example.com", errors.New("connection refused"), time.Now())
	if st.State != StatePending {
		t.Errorf("State = %q, want %q", st.State, StatePending)
	}
	if st.Hint != "" || st.Error != "" {
		t.Errorf("Hint/Error = %q/%q, want both empty while issuance is still in flight", st.Hint, st.Error)
	}
}

// Past the grace with no certificate, the failure is real and carries the hint.
func TestVerdictFailsPastIssuanceGrace(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.since["example.com"] = time.Now().Add(-2 * issuanceGrace)
	m.mu.Unlock()

	st := m.verdictForFailedProbe("example.com", errors.New("lookup example.com: no such host"), time.Now())
	if st.State != StateFailed {
		t.Errorf("State = %q, want %q", st.State, StateFailed)
	}
	if st.Hint == "" {
		t.Error("Hint is empty on a real failure, want the diagnosis")
	}
	if st.Error == "" {
		t.Error("Error is empty on a real failure, want the underlying dial error")
	}
}

// A domain that was never managed has no issuance to wait for (filterValidDomains
// drops domains whose DNS does not resolve), so an on-demand probe reports the
// failure straight away instead of sitting in a grace period that will never end.
func TestVerdictFailsForUnmanagedDomain(t *testing.T) {
	m := newTestManager(t)
	st := m.verdictForFailedProbe("never-managed.example", errors.New("lookup failed: no such host"), time.Now())
	if st.State != StateFailed {
		t.Errorf("State = %q, want %q", st.State, StateFailed)
	}
}
