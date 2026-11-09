package portmap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"openfabrica/internal/msgcat"
)

// SettingKey is the settings-table key that records the owner's consent to
// forward ports on the router, so the forwards come back after a restart.
const SettingKey = "router_port_mapping"

// WebPorts are the only ports the Mapper ever forwards: the ones the TLS proxy
// serves. The admin panel and the plain-HTTP public port are never exposed.
var WebPorts = []int{80, 443}

const (
	// requestedLease is what the Mapper asks for. Short leases mean a crashed
	// process leaves nothing open for long; renewal keeps them alive.
	requestedLease = 2 * time.Hour
	// permanentReassert is how often a lease the router made permanent is
	// re-asserted, which also repairs it after a router reboot.
	permanentReassert = time.Hour
	// retryAfterFailure paces renewals that failed (router rebooting, Wi-Fi
	// switched), rediscovering the router each time.
	retryAfterFailure = time.Minute
	opTimeout         = 10 * time.Second
)

// Status is the Mapper's state, as reported to the admin UI.
type Status struct {
	Enabled    bool       `json:"enabled"`
	Active     bool       `json:"active"`
	Protocol   string     `json:"protocol,omitempty"`
	ExternalIP string     `json:"external_ip,omitempty"`
	InternalIP string     `json:"internal_ip,omitempty"`
	Error      string     `json:"error,omitempty"`
	RenewedAt  *time.Time `json:"renewed_at,omitempty"`
}

// Mapper keeps WebPorts forwarded on the router while enabled, renewing the
// leases in the background.
type Mapper struct {
	discover func(context.Context) (Client, Router, error)
	logger   *slog.Logger

	mu     sync.Mutex
	client Client
	status Status
	cancel context.CancelFunc // stops the renewal loop
	done   chan struct{}
}

// NewMapper builds a Mapper that finds the router with Discover.
func NewMapper(logger *slog.Logger) *Mapper {
	if logger == nil {
		logger = slog.Default()
	}
	return &Mapper{discover: Discover, logger: logger}
}

// Status returns a snapshot of the current state.
func (m *Mapper) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Enable forwards WebPorts now and keeps them forwarded. On failure nothing
// stays mapped, the loop does not start, and the error is also in Status.
func (m *Mapper) Enable(ctx context.Context) (Status, error) {
	m.stopLoop()

	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	client, _, err := m.discover(ctx)
	if err == nil {
		var next time.Duration
		next, err = m.mapAll(ctx, client)
		if err == nil {
			m.startLoop(next)
			return m.Status(), nil
		}
	}

	m.mu.Lock()
	m.status = Status{Error: err.Error()}
	m.mu.Unlock()
	return m.Status(), err
}

// Disable stops renewal and removes the forwards.
func (m *Mapper) Disable(ctx context.Context) error {
	m.stopLoop()
	m.mu.Lock()
	client := m.client
	m.client = nil
	m.status = Status{}
	m.mu.Unlock()
	if client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	var errs []error
	for _, p := range WebPorts {
		errs = append(errs, client.DeleteTCP(ctx, p, p))
	}
	return errors.Join(errs...)
}

// Stop removes the forwards on shutdown. Nothing listens on the ports once
// the process exits, so leaving them open would only expose whatever claims
// the ports next.
func (m *Mapper) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.Disable(ctx); err != nil {
		m.logger.Debug("could not remove router port forwards on shutdown", "error", err)
	}
}

// mapAll forwards every web port through client and records the result. It
// returns when the next renewal is due. A partial failure rolls back the ports
// already mapped, so the router never holds half a setup.
func (m *Mapper) mapAll(ctx context.Context, client Client) (time.Duration, error) {
	var got []Mapping
	for _, p := range WebPorts {
		mp, err := client.AddTCP(ctx, p, p, requestedLease)
		if err != nil {
			for _, done := range got {
				_ = client.DeleteTCP(ctx, done.ExternalPort, done.InternalPort)
			}
			return 0, fmt.Errorf(msgcat.Mark("port %d: %w"), p, err)
		}
		got = append(got, mp)
	}

	next := permanentReassert
	for _, mp := range got {
		if mp.Lifetime > 0 && mp.Lifetime/2 < next {
			next = mp.Lifetime / 2
		}
	}
	now := time.Now()
	st := Status{Enabled: true, Active: true, Protocol: client.Protocol(), RenewedAt: &now}
	for _, mp := range got {
		if st.ExternalIP == "" {
			st.ExternalIP = mp.ExternalIP
		}
		if st.InternalIP == "" {
			st.InternalIP = mp.InternalIP
		}
	}
	m.mu.Lock()
	m.client = client
	m.status = st
	m.mu.Unlock()
	return next, nil
}

func (m *Mapper) startLoop(first time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.mu.Lock()
	m.cancel = cancel
	m.done = done
	m.mu.Unlock()
	go m.renewLoop(ctx, done, first)
}

func (m *Mapper) stopLoop() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// renewLoop re-asserts the forwards before they lapse. A failed renewal
// rediscovers the router, since the usual cause is a router reboot or a new
// local address, and keeps retrying while enabled.
func (m *Mapper) renewLoop(ctx context.Context, done chan struct{}, wait time.Duration) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = m.renew(ctx)
	}
}

func (m *Mapper) renew(ctx context.Context) time.Duration {
	opCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	m.mu.Lock()
	client := m.client
	m.mu.Unlock()
	if client != nil {
		if next, err := m.mapAll(opCtx, client); err == nil {
			return next
		}
	}
	client, _, err := m.discover(opCtx)
	if err == nil {
		var next time.Duration
		if next, err = m.mapAll(opCtx, client); err == nil {
			return next
		}
	}
	if ctx.Err() != nil {
		return retryAfterFailure
	}
	m.logger.Warn("router port forward renewal failed, retrying", "error", err)
	m.mu.Lock()
	m.status.Active = false
	m.status.Error = err.Error()
	m.mu.Unlock()
	return retryAfterFailure
}
