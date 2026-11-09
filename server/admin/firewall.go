package admin

import (
	"net/http"

	"openfabrica/db/models"
	"openfabrica/internal/firewall"
	"openfabrica/internal/msgcat"
)

// FirewallHandler exposes the local OS firewall helper: read status, add the
// allow-rule (prompts for elevation), and remove it.
//
// Scope: this opens the LOCAL firewall only, making the machine reachable from
// the same network. Internet reachability still needs router port-forwarding.
// The UI copy states this; the handler does not.
type FirewallHandler struct {
	deps *Deps
}

// firewallRule builds the rule for the current install. The public port is
// always relevant for local-network reachability; 80/443 are added only when a
// project actually has a custom domain (those are the ports the TLS proxy
// binds). Ports drive the Linux rule; the Windows rule is program-scoped and
// covers them all.
func (h *FirewallHandler) firewallRule() firewall.Rule {
	ports := []int{h.deps.Config.PublicPort}
	if h.anyDomainConfigured() {
		ports = append(ports, 80, 443)
	}
	return firewall.Rule{Name: firewall.RuleName, Ports: ports}
}

func (h *FirewallHandler) anyDomainConfigured() bool {
	projects, err := models.ListPublishedProjects(h.deps.DB.DB)
	if err != nil {
		return false
	}
	for _, p := range projects {
		if p.Domain != nil && *p.Domain != "" {
			return true
		}
	}
	return false
}

// Status reports whether the firewall is blocking inbound and whether our rule
// already exists. Read-only, never prompts. Recommended is true only when the
// firewall is actually active, our rule is missing, and a custom domain makes
// inbound access a concrete need - so the UI never nags localhost-only users.
func (h *FirewallHandler) Status(w http.ResponseWriter, r *http.Request) {
	st, err := h.deps.FirewallManager.Detect(r.Context(), h.firewallRule())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read firewall status: "+err.Error())
		return
	}
	st.Recommended = st.Supported && st.Active && !st.RulePresent && h.anyDomainConfigured()
	st.Detail = msgcat.Localize(r, st.Detail)
	writeJSON(w, http.StatusOK, st)
}

// Allow adds the allow-rule, prompting for elevation (UAC/polkit) on the host
// machine. Admin-only.
func (h *FirewallHandler) Allow(w http.ResponseWriter, r *http.Request) {
	res, err := h.deps.FirewallManager.Ensure(r.Context(), h.firewallRule())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Remove deletes the allow-rule, prompting for elevation. Admin-only.
func (h *FirewallHandler) Remove(w http.ResponseWriter, r *http.Request) {
	res, err := h.deps.FirewallManager.Remove(r.Context(), firewall.RuleName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
