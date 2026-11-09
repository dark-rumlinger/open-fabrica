package middleware

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"openfabrica/internal/httpx"
	"openfabrica/internal/replayid"
)

// maxBuckets caps the number of tracked IPs so a flood of distinct source
// addresses cannot grow the map without bound (and make the periodic sweep
// arbitrarily expensive). At the cap a sweep runs immediately and, if still
// full, the least-recently-seen entry is evicted to admit the new IP.
const maxBuckets = 50000

// bucket is a simple token bucket for a single IP.
type bucket struct {
	tokens    float64
	lastCheck time.Time
}

// RateLimiter is a per-IP token bucket rate limiter.
type RateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	rate      float64 // tokens per second
	burst     float64 // max tokens (bucket capacity)
	lastSweep time.Time
}

// NewRateLimiter creates a rate limiter that allows rate requests per second
// with the given burst capacity.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return &RateLimiter{
		buckets:   make(map[string]*bucket),
		rate:      rate,
		burst:     float64(burst),
		lastSweep: time.Now(),
	}
}

// Allow checks whether the given IP is allowed to proceed and consumes a
// token if so. Use this for ad-hoc per-action rate limiting (e.g. gating a
// specific sub-route inside a larger handler), separate from the Handler
// middleware which gates a whole router subtree.
func (rl *RateLimiter) Allow(ip string) bool {
	return rl.allow(ip)
}

// allow checks whether the given IP is allowed to proceed.
func (rl *RateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	// Sweep stale entries every minute.
	if now.Sub(rl.lastSweep) > time.Minute {
		rl.sweepLocked(now)
	}

	b, ok := rl.buckets[ip]
	if !ok {
		// Bound memory: at the ceiling, sweep now and, if still full, evict the
		// least-recently-seen entry to make room for this IP.
		if len(rl.buckets) >= maxBuckets {
			rl.sweepLocked(now)
			if len(rl.buckets) >= maxBuckets {
				rl.evictOldestLocked()
			}
		}
		b = &bucket{tokens: rl.burst, lastCheck: now}
		rl.buckets[ip] = b
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.lastCheck).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.lastCheck = now

	if b.tokens < 1 {
		return false
	}

	b.tokens--
	return true
}

// sweepLocked removes entries idle beyond the retention window. Caller must
// hold rl.mu.
func (rl *RateLimiter) sweepLocked(now time.Time) {
	for k, b := range rl.buckets {
		if now.Sub(b.lastCheck) > 2*time.Minute {
			delete(rl.buckets, k)
		}
	}
	rl.lastSweep = now
}

// evictOldestLocked removes the single least-recently-seen bucket so a new IP
// can be admitted at the cap. Caller must hold rl.mu.
func (rl *RateLimiter) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, b := range rl.buckets {
		if first || b.lastCheck.Before(oldest) {
			oldestKey, oldest, first = k, b.lastCheck, false
		}
	}
	if oldestKey != "" {
		delete(rl.buckets, oldestKey)
	}
}

// Handler returns HTTP middleware that rate limits by client IP (a replay session by
// its simulated visitor, see RateLimitIdentity).
func (rl *RateLimiter) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(RateLimitIdentity(r)) {
			httpx.WriteError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RateLimitIdentity is who a rate limiter counts a request against: the client IP,
// except for a user-flow replay's session tab, which is counted as the simulated
// visitor it is (see replayid). Every replay comes from one loopback browser, so by IP
// its flows and users would share one bucket no real visitor shares. Only a loopback
// request carrying an active replay's token qualifies.
func RateLimitIdentity(r *http.Request) string {
	if IsLoopbackAddr(r.RemoteAddr) {
		if c, err := r.Cookie(replayid.Cookie); err == nil {
			if id := replayid.Identity(c.Value); id != "" {
				return id
			}
		}
	}
	return ClientIP(r)
}

// IsLoopbackAddr reports whether the host portion of a net/http RemoteAddr is a
// loopback address. Used to decide whether to trust proxy-injected headers
// (X-Forwarded-For): those are only legitimate from the in-process reverse
// proxy, which reaches the public server over localhost. A direct external peer
// is never loopback.
func IsLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ClientIP extracts the client IP from the request. X-Forwarded-For and
// X-Real-IP are believed only when the direct peer is a trusted proxy: loopback
// (the built-in TLS proxy, or nginx on the same host) or a network listed in
// trusted_proxies. The chain is read from the right and the first address that
// is not itself a trusted proxy is the client: a proxy appends the peer it saw,
// so the leftmost entries are whatever the caller chose to send.
func ClientIP(r *http.Request) string {
	peer := hostOf(r.RemoteAddr)
	if !isTrustedProxyIP(net.ParseIP(peer)) {
		return peer
	}
	var chain []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				chain = append(chain, p)
			}
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		ip := net.ParseIP(chain[i])
		if ip == nil {
			return peer
		}
		if !isTrustedProxyIP(ip) {
			return chain[i]
		}
	}
	if len(chain) > 0 {
		// Every hop is a trusted proxy (a local client): the first one is it.
		if ip := net.ParseIP(chain[0]); ip != nil {
			return chain[0]
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" && net.ParseIP(xri) != nil {
		return xri
	}
	return peer
}

// trustedProxies are extra proxy networks (beyond loopback) whose forwarding
// headers are believed. Set once at startup from the trusted_proxies setting.
var trustedProxies atomic.Pointer[[]*net.IPNet]

// SetTrustedProxies installs the proxy allow-list: each entry is an IP or CIDR.
// Entries that do not parse are returned as errors and skipped.
func SetTrustedProxies(entries []string) []error {
	var nets []*net.IPNet
	var errs []error
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				errs = append(errs, fmt.Errorf("trusted proxy %q is not an IP or CIDR", e))
				continue
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			errs = append(errs, fmt.Errorf("trusted proxy %q: %w", e, err))
			continue
		}
		nets = append(nets, n)
	}
	trustedProxies.Store(&nets)
	return errs
}

// isTrustedProxyIP reports whether ip is loopback or inside a configured proxy network.
func isTrustedProxyIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	if p := trustedProxies.Load(); p != nil {
		for _, n := range *p {
			if n.Contains(ip) {
				return true
			}
		}
	}
	return false
}

func hostOf(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
