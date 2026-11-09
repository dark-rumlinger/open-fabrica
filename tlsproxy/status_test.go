package tlsproxy

import (
	"log/slog"
	"testing"
	"time"

	"openfabrica/config"
)

// newTestManager builds a Manager with no DB and no listeners. Sufficient for
// the status-registry paths, which never touch the network or the database.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	cfg := &config.Config{DataDir: t.TempDir()}
	return NewManager(cfg, nil, slog.Default())
}

// TestDomainKind covers the classification the admin hosting-status endpoint
// words its guidance around.
func TestDomainKind(t *testing.T) {
	cases := map[string]string{
		"":               DomainKindNone,
		"localhost":      DomainKindLocal,
		"app.localhost":  DomainKindLocal,
		"127.0.0.1":      DomainKindIP,
		"::1":            DomainKindIP,
		"192.168.1.10":   DomainKindIP,
		"example.com":    DomainKindPublic,
		"sub.example.io": DomainKindPublic,
	}
	for in, want := range cases {
		if got := DomainKind(in); got != want {
			t.Errorf("DomainKind(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStatusDefaultsToPending verifies an unprobed domain reports pending with
// no cert loaded, so the UI shows "issuing" rather than an error while ACME
// runs.
func TestStatusDefaultsToPending(t *testing.T) {
	m := newTestManager(t)
	st := m.Status("example.com")
	if st.State != StatePending {
		t.Errorf("State = %q, want %q", st.State, StatePending)
	}
	if st.CertLoaded {
		t.Error("CertLoaded = true for a domain with no certificate")
	}
	if st.CheckedAt != nil {
		t.Error("CheckedAt set for a domain that was never probed")
	}
}

// TestStatusReturnsRecordedProbe verifies a recorded probe result round-trips
// through Status, including the failure hint the UI surfaces.
func TestStatusReturnsRecordedProbe(t *testing.T) {
	m := newTestManager(t)
	now := time.Now()
	m.setStatus("example.com", DomainStatus{
		State:     StateFailed,
		CheckedAt: &now,
		Error:     "connection refused",
		Hint:      "port 443 is not reachable",
	})

	st := m.Status("example.com")
	if st.State != StateFailed {
		t.Errorf("State = %q, want %q", st.State, StateFailed)
	}
	if st.Hint != "port 443 is not reachable" {
		t.Errorf("Hint = %q, want the recorded hint", st.Hint)
	}
	if st.CheckedAt == nil || !st.CheckedAt.Equal(now) {
		t.Errorf("CheckedAt = %v, want %v", st.CheckedAt, now)
	}

	// Other domains are unaffected.
	if other := m.Status("other.com"); other.State != StatePending {
		t.Errorf("unrelated domain State = %q, want %q", other.State, StatePending)
	}
}

// TestProbeSkipsLocalDomains verifies Probe never dials loopback or IP-literal
// domains (HTTPS does not apply to them) and just returns the current status.
func TestProbeSkipsLocalDomains(t *testing.T) {
	m := newTestManager(t)
	for _, d := range []string{"localhost", "app.localhost", "10.0.0.5"} {
		st := m.Probe(d)
		if st.State != StatePending {
			t.Errorf("Probe(%q).State = %q, want %q (no probe recorded)", d, st.State, StatePending)
		}
	}
}

// TestLastStartError verifies the bind-failure capture starts empty.
func TestLastStartError(t *testing.T) {
	m := newTestManager(t)
	if got := m.LastStartError(); got != "" {
		t.Errorf("LastStartError() = %q on a fresh manager, want empty", got)
	}
}
