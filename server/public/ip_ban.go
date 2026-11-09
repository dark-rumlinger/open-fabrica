package public

import (
	"net"
	"net/http"
	"strings"

	"openfabrica/db/models"
	"openfabrica/server/middleware"
)

// IPBanCheck blocks visitors whose client IP matches the project's owner-managed
// ban list (IP addresses and CIDR ranges, configured in project settings).
// It must run after ProjectResolver so the resolved project is in context.
// Banned visitors get 403 Forbidden: a branded HTML page for browsers, JSON
// for API clients, mirroring the rest of the public error surface.
func (h *Handler) IPBanCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if project := getProject(r); project != nil {
			banned := models.ProjectBannedIPs(project)
			if len(banned) > 0 && ipMatchesAny(middleware.ClientIP(r), banned) {
				if wantsHTML(r) {
					serveErrorPage(w, http.StatusForbidden, "Access Denied", "You do not have permission to access this site.")
				} else {
					writeError(w, http.StatusForbidden, "forbidden")
				}
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ipMatchesAny reports whether ipStr falls within any of the given IP addresses
// or CIDR ranges. Entries are validated at save time; an unparseable one is
// skipped defensively rather than blocking or erroring.
func ipMatchesAny(ipStr string, entries []string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, entry := range entries {
		if strings.Contains(entry, "/") {
			if _, network, err := net.ParseCIDR(entry); err == nil && network.Contains(ip) {
				return true
			}
			continue
		}
		if entryIP := net.ParseIP(entry); entryIP != nil && entryIP.Equal(ip) {
			return true
		}
	}
	return false
}
