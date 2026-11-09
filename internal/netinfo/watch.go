package netinfo

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Mismatch reports a public domain whose DNS record points somewhere other
// than this machine's current public IP, the usual symptom of a home
// connection that was handed a new address.
type Mismatch struct {
	ResolvesTo []string `json:"resolves_to"`
	ShouldBe   string   `json:"should_be"`
}

// strikesToFlag is how many consecutive mismatching checks it takes to flag a
// domain. One is not enough: right after a DNS change, resolvers disagree for
// a while and a single check can land on a stale answer.
const strikesToFlag = 2

// Watcher periodically compares each public project domain's IPv4 record with
// the public IP the Detector sees, and remembers the domains that disagree.
// It only compares against an address that came from the router or a local
// interface: an address read from the domain itself would always agree.
type Watcher struct {
	det      *Detector
	domains  func() []string
	lookup   func(ctx context.Context, host string) ([]net.IP, error)
	interval time.Duration
	logger   *slog.Logger

	mu      sync.Mutex
	strikes map[string]int
	flagged map[string]*Mismatch

	started  atomic.Bool
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// NewWatcher builds a Watcher over det. domains lists the public domains to
// watch; it is called on every pass so newly added domains are picked up.
func NewWatcher(det *Detector, domains func() []string, logger *slog.Logger) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Watcher{
		det:      det,
		domains:  domains,
		lookup:   det.lookup,
		interval: 15 * time.Minute,
		logger:   logger,
		strikes:  map[string]int{},
		flagged:  map[string]*Mismatch{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Detector returns the Detector the Watcher compares against, so the admin
// endpoints share its router cache.
func (w *Watcher) Detector() *Detector { return w.det }

// Start runs the periodic check in the background until Stop.
func (w *Watcher) Start() {
	if w.started.Swap(true) {
		return
	}
	go func() {
		defer close(w.done)
		wait := time.Minute // let startup settle before the first pass
		for {
			select {
			case <-w.stop:
				return
			case <-time.After(wait):
			}
			w.checkAll()
			wait = w.interval
		}
	}()
}

// Stop ends the background check and waits for it.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
	if w.started.Load() {
		<-w.done
	}
}

// Mismatch returns the flagged mismatch for domain, or nil.
func (w *Watcher) Mismatch(domain string) *Mismatch {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flagged[domain]
}

// Recheck checks one domain now, for the admin "check again" action, and
// returns the resulting flag.
func (w *Watcher) Recheck(ctx context.Context, domain string) *Mismatch {
	w.det.Refresh()
	info := w.det.Detect(ctx)
	w.check(ctx, domain, info)
	return w.Mismatch(domain)
}

func (w *Watcher) checkAll() {
	domains := w.domains()
	if len(domains) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info := w.det.Detect(ctx)
	for _, d := range domains {
		w.check(ctx, d, info)
	}
}

// check compares one domain against info. A domain that does not resolve is
// left alone: that is a record still to be created, which the going-live
// card already walks through, not an address that changed.
func (w *Watcher) check(ctx context.Context, domain string, info Info) {
	if info.PublicIP == "" || (info.PublicIPSource != SourceRouter && info.PublicIPSource != SourceInterface) {
		return
	}
	ips, err := w.lookup(ctx, domain)
	if err != nil {
		return
	}
	var v4 []string
	for _, ip := range ips {
		if ip4 := ip.To4(); ip4 != nil {
			v4 = append(v4, ip4.String())
		}
	}
	if len(v4) == 0 {
		return
	}
	sort.Strings(v4)

	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ip := range v4 {
		if ip == info.PublicIP {
			if w.flagged[domain] != nil {
				w.logger.Info("domain points at this machine again", "domain", domain, "ip", info.PublicIP)
			}
			delete(w.strikes, domain)
			delete(w.flagged, domain)
			return
		}
	}
	w.strikes[domain]++
	if w.strikes[domain] < strikesToFlag {
		return
	}
	if w.flagged[domain] == nil {
		w.logger.Warn("domain no longer points at this machine; update its A record",
			"domain", domain, "resolves_to", v4, "public_ip", info.PublicIP)
	}
	w.flagged[domain] = &Mismatch{ResolvesTo: v4, ShouldBe: info.PublicIP}
}
