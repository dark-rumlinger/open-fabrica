package admin

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"openfabrica/db/models"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/netinfo"
	"openfabrica/internal/portmap"
	"openfabrica/internal/qrcode"
	"openfabrica/tlsproxy"
)

// HostingHandler surfaces per-project hosting health: domain classification,
// HTTPS certificate state, TLS bind failures, network position, and the
// phone-test QR code. It powers the status chip and the Go Live flow in the
// admin UI.
type HostingHandler struct {
	deps *Deps
	net  *netinfo.Detector
}

// hostingHTTPS is the HTTPS slice of the hosting status payload. State is one
// of: disabled (TLS turned off for the instance), not_applicable (local or IP
// domain, HTTPS never applies), or a tlsproxy state
// (pending/active/unverified/failed). Only failed means something is wrong;
// unverified means the cert is in place but this host cannot dial itself
// through its own router, so only an outside device can confirm.
type hostingHTTPS struct {
	State      string     `json:"state"`
	CertLoaded bool       `json:"cert_loaded"`
	CertExpiry *time.Time `json:"cert_expiry,omitempty"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
	Hint       string     `json:"hint,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type hostingStatus struct {
	Domain       string       `json:"domain"`
	DomainKind   string       `json:"domain_kind"`
	HTTPS        hostingHTTPS `json:"https"`
	TLSBindError string       `json:"tls_bind_error,omitempty"`
	TLSDisabled  bool         `json:"tls_disabled"`
	PublicPort   int          `json:"public_port"`
	// DNSMismatch is set when the domain's A record no longer matches this
	// machine's public IP (a home connection that got a new address).
	DNSMismatch *netinfo.Mismatch `json:"dns_mismatch,omitempty"`
}

// Status reports the hosting health for one project. Read-only and cheap: it
// never probes, it reads the recorded state (the TLS manager's self-probe and
// Recheck keep it fresh).
func (h *HostingHandler) Status(w http.ResponseWriter, r *http.Request) {
	h.respond(w, r, false)
}

// Recheck runs an on-demand HTTPS probe for the project's domain, then returns
// the refreshed status. Admin-only; concurrent rechecks for the same domain
// collapse into one probe inside the TLS manager.
func (h *HostingHandler) Recheck(w http.ResponseWriter, r *http.Request) {
	h.respond(w, r, true)
}

func (h *HostingHandler) respond(w http.ResponseWriter, r *http.Request, probe bool) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}
	project, err := models.GetProjectByID(h.deps.DB.DB, projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	domain := ""
	if project.Domain != nil {
		domain = *project.Domain
	}

	st := hostingStatus{
		Domain:     domain,
		DomainKind: tlsproxy.DomainKind(domain),
		PublicPort: h.deps.Config.PublicPort,
	}
	tlsOff := h.deps.Config.TLSDisabled || h.deps.TLSManager == nil
	st.TLSDisabled = tlsOff

	switch {
	case tlsOff:
		st.HTTPS.State = "disabled"
	case st.DomainKind != tlsproxy.DomainKindPublic:
		st.HTTPS.State = "not_applicable"
	default:
		var ds tlsproxy.DomainStatus
		if probe {
			ds = h.deps.TLSManager.Probe(domain)
		} else {
			ds = h.deps.TLSManager.Status(domain)
		}
		st.HTTPS = hostingHTTPS{
			State:      ds.State,
			CertLoaded: ds.CertLoaded,
			CertExpiry: ds.CertExpiry,
			CheckedAt:  ds.CheckedAt,
			Hint:       msgcat.Localize(r, ds.Hint),
			Error:      ds.Error,
		}
	}
	if h.deps.TLSManager != nil {
		st.TLSBindError = h.deps.TLSManager.LastStartError()
	}
	if st.DomainKind == tlsproxy.DomainKindPublic && h.deps.NetWatcher != nil {
		if probe {
			st.DNSMismatch = h.deps.NetWatcher.Recheck(r.Context(), domain)
		} else {
			st.DNSMismatch = h.deps.NetWatcher.Mismatch(domain)
		}
	}

	writeJSON(w, http.StatusOK, st)
}

type networkResponse struct {
	netinfo.Info
	PortMapping *portmap.Status `json:"port_mapping,omitempty"`
}

// Network reports this machine's public IP, local interface addresses, the
// router in front of it, and the state of router port forwarding. Admin-only:
// the Go Live flow uses it to render the exact DNS record and port-forward
// steps. An empty public_ip means detection failed and the UI shows a manual
// fallback. The optional ?domain= lets detection fall back on what that
// domain currently points to.
func (h *HostingHandler) Network(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	if tlsproxy.DomainKind(domain) != tlsproxy.DomainKindPublic {
		domain = ""
	}
	resp := networkResponse{Info: h.net.DetectFor(r.Context(), domain)}
	if h.deps.PortMapper != nil {
		st := localizedPortStatus(r, h.deps.PortMapper.Status())
		resp.PortMapping = &st
		// PCP only learns the external address once a mapping exists.
		if resp.Router != nil && resp.Router.ExternalIP == "" && st.ExternalIP != "" {
			resp.Router.ExternalIP = st.ExternalIP
			ext := net.ParseIP(st.ExternalIP)
			switch {
			case ext == nil || resp.PublicIPSource == netinfo.SourceInterface:
			case portmap.IsCarrierNAT(ext):
				resp.SharedAddress = true
			case ext.IsPrivate():
				resp.DoubleNAT = true
			default:
				resp.PublicIP, resp.PublicIPSource = st.ExternalIP, netinfo.SourceRouter
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// PortmapStatus reports the router port forwarding state without contacting
// the router. Admin-only.
func (h *HostingHandler) PortmapStatus(w http.ResponseWriter, r *http.Request) {
	if h.deps.PortMapper == nil {
		writeJSON(w, http.StatusOK, portmap.Status{})
		return
	}
	writeJSON(w, http.StatusOK, localizedPortStatus(r, h.deps.PortMapper.Status()))
}

// localizedPortStatus puts the router's reason for a refusal in the caller's language.
func localizedPortStatus(r *http.Request, st portmap.Status) portmap.Status {
	st.Error = msgcat.Localize(r, st.Error)
	return st
}

// PortmapEnable asks the router to forward ports 80 and 443 to this machine
// and keeps the forwards alive. The consent is stored only once the router
// accepted, so a restart re-creates forwards that worked, never retries ones
// that did not. Admin-only; the UI asks before calling it.
func (h *HostingHandler) PortmapEnable(w http.ResponseWriter, r *http.Request) {
	if !h.portmapUsable(w) {
		return
	}
	st, err := h.deps.PortMapper.Enable(r.Context())
	if err == nil {
		if serr := h.deps.DB.SetSetting(portmap.SettingKey, "on"); serr != nil {
			h.deps.Logger.Warn("could not save router port forwarding consent", "error", serr)
		}
	}
	// A refused mapping is an answer, not a server failure: the status
	// carries the reason for the UI to show.
	h.net.Refresh()
	writeJSON(w, http.StatusOK, localizedPortStatus(r, st))
}

// PortmapDisable removes the forwards and withdraws the stored consent.
// Admin-only.
func (h *HostingHandler) PortmapDisable(w http.ResponseWriter, r *http.Request) {
	if h.deps.PortMapper == nil {
		writeError(w, http.StatusServiceUnavailable, "router port forwarding is not available")
		return
	}
	if err := h.deps.DB.SetSetting(portmap.SettingKey, "off"); err != nil {
		h.deps.Logger.Warn("could not clear router port forwarding consent", "error", err)
	}
	if err := h.deps.PortMapper.Disable(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, "the router did not confirm removing the forwards: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, localizedPortStatus(r, h.deps.PortMapper.Status()))
}

func (h *HostingHandler) portmapUsable(w http.ResponseWriter) bool {
	switch {
	case h.deps.PortMapper == nil:
		writeError(w, http.StatusServiceUnavailable, "router port forwarding is not available")
		return false
	case h.deps.Config.TLSDisabled:
		writeError(w, http.StatusConflict, "built-in HTTPS is turned off for this server, so nothing here answers on ports 80 and 443")
		return false
	}
	return true
}

// QR returns the project's public URL and a QR code for it (SVG data URI), so
// the owner can open the project on a phone over mobile data - a genuine
// outside-in reachability test with no external probe service.
func (h *HostingHandler) QR(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}
	project, err := models.GetProjectByID(h.deps.DB.DB, projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if project.Domain == nil || *project.Domain == "" {
		writeError(w, http.StatusBadRequest, "project has no domain configured")
		return
	}

	url := publicURLFor(*project.Domain, h.deps.Config.PublicPort, h.deps.Config.TLSDisabled)
	writeJSON(w, http.StatusOK, map[string]string{
		"url": url,
		"qr":  qrcode.DataURI(url),
	})
}

// publicURLFor builds the visitor-facing URL for a domain: HTTPS for real
// domain names (the TLS proxy serves 443 and redirects 80), plain HTTP with
// the public port for local and IP domains.
func publicURLFor(domain string, publicPort int, tlsDisabled bool) string {
	if tlsproxy.DomainKind(domain) == tlsproxy.DomainKindPublic && !tlsDisabled {
		return "https://" + domain
	}
	return fmt.Sprintf("http://%s:%d", domain, publicPort)
}
