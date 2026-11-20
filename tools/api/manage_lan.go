package api

import (
	"context"
	"strings"
	"time"

	"openfabrica/internal/lan"
	"openfabrica/tools"
)

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_lan",
		MaxResult:          16000,
		Category:           "api",
		FullResultInEvents: true,
		OneLiner:           "Local-network toolkit: Wake-on-LAN magic packets, LAN TCP reachability pings, and mDNS/Bonjour service discovery.",
		WhenToUse: `Use when building home-lab control panels, NAS/PC Wake-on-LAN dashboards,
local network monitors, or discovering LAN MQTT brokers, Home Assistant,
ESPHome, Shelly, printers, and SMB shares via mDNS. Actions: wake, ping, discover.`,
		Actions: []tools.Action{
			&LANWake{},
			&LANPing{},
			&LANDiscover{},
		},
	})
}

// --- wake -------------------------------------------------------------------

type LANWake struct {
	MAC       string `arg:"mac,required" desc:"Target 6-byte Ethernet MAC address (e.g. AA:BB:CC:DD:EE:FF)."`
	Broadcast string `arg:"broadcast" desc:"Optional IPv4 broadcast address (default 255.255.255.255 or subnet broadcast like 192.168.1.255)."`
	Port      int    `arg:"port" max:"65535" desc:"Optional UDP port (default 9, max 65535)."`
}

func (a *LANWake) Doc() string {
	return "Send a Wake-on-LAN (IEEE 802.3) UDP magic packet to wake a sleeping PC, NAS, or server on the LAN."
}
func (a *LANWake) Example() any {
	return &LANWake{MAC: "AA:BB:CC:DD:EE:FF", Broadcast: "255.255.255.255", Port: 9}
}
func (a *LANWake) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	bcast, port, err := lan.Wake(a.MAC, a.Broadcast, a.Port)
	if err != nil {
		return tools.Fail("%v", err), nil
	}
	return tools.Ok(map[string]any{
		"ok":        true,
		"mac":       strings.TrimSpace(a.MAC),
		"broadcast": bcast,
		"port":      port,
	}), nil
}

// --- ping -------------------------------------------------------------------

type LANPing struct {
	Host      string `arg:"host,required" desc:"Private LAN IP (e.g. 192.168.1.50), localhost, or .local mDNS hostname."`
	Port      int    `arg:"port,required" max:"65535" desc:"TCP port to probe (1..65535, e.g. 1883, 80, 445, 22)."`
	TimeoutMS int    `arg:"timeout_ms" max:"3000" desc:"Optional timeout in milliseconds (default 1000, max 3000)."`
}

func (a *LANPing) Doc() string {
	return "Check TCP reachability and latency of a private LAN or .local host and port."
}
func (a *LANPing) ReadOnly() bool { return true }
func (a *LANPing) Example() any {
	return &LANPing{Host: "192.168.1.50", Port: 1883, TimeoutMS: 1000}
}
func (a *LANPing) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	timeout := time.Duration(a.TimeoutMS) * time.Millisecond
	reachable, latencyMS, err := lan.TCPPing(context.Background(), a.Host, a.Port, timeout)
	if err != nil {
		return tools.Fail("%v", err), nil
	}
	return tools.Ok(map[string]any{
		"host":       strings.TrimSpace(a.Host),
		"port":       a.Port,
		"reachable":  reachable,
		"latency_ms": latencyMS,
	}), nil
}

// --- discover ---------------------------------------------------------------

type LANDiscover struct {
	Service   string `arg:"service" desc:"mDNS service type to browse (default _http._tcp.local.; try _mqtt._tcp.local., _esphomelib._tcp.local., _home-assistant._tcp.local., _smb._tcp.local., or _services._dns-sd._udp.local.)."`
	TimeoutMS int    `arg:"timeout_ms" max:"4000" desc:"Browse window in milliseconds (default 1500, max 4000)."`
}

func (a *LANDiscover) Doc() string {
	return "Discover local network devices and services via mDNS/Bonjour (RFC 6762)."
}
func (a *LANDiscover) ReadOnly() bool { return true }
func (a *LANDiscover) Example() any {
	return &LANDiscover{Service: "_mqtt._tcp.local.", TimeoutMS: 1500}
}
func (a *LANDiscover) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	timeout := time.Duration(a.TimeoutMS) * time.Millisecond
	services, err := lan.Discover(context.Background(), a.Service, timeout)
	if err != nil {
		return tools.Fail("mdns discover: %v", err), nil
	}
	if services == nil {
		services = []lan.DiscoveredService{}
	}
	return tools.Ok(map[string]any{
		"count":    len(services),
		"services": services,
	}), nil
}
