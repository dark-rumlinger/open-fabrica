package middleware

import (
	"net"
	"net/http"
	"strings"
)

// IsLoopbackHost reports whether a Host header value names the local machine
// (localhost, a *.localhost name, or a loopback IP), with or without a port.
func IsLoopbackHost(hostHeader string) bool {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LoopbackHostOnly rejects requests whose Host header is not a loopback name.
// A loopback-bound control plane is only ever reached as localhost or 127.0.0.1,
// so any other Host means the request was relayed: a DNS-rebinding page (the
// browser sends the attacker's domain) or a same-host reverse proxy forwarding a
// public name. Without it such a request passes a peer-address check, because the
// peer really is loopback.
func LoopbackHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsLoopbackHost(r.Host) {
			http.Error(w, "unexpected Host header", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// HasProxyHeaders reports whether a request carries forwarding headers, meaning
// a proxy relayed it and the loopback peer address is the proxy's, not the user's.
func HasProxyHeaders(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded", "X-Forwarded-Host"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}
