package tlsproxy

import (
	"fmt"
	"net"
	"strings"
)

// bindWithDiag attempts to listen on addr and returns a clear, actionable
// error message if it fails. The most common failure modes (port in use,
// missing capability) are translated to plain-English fixes so a
// self-hosting user gets a useful log line instead of a raw syscall error.
func bindWithDiag(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}
	return nil, fmt.Errorf("could not bind %s: %s", addr, diagnoseBindError(addr, err))
}

func diagnoseBindError(addr string, err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "address already in use"), strings.Contains(msg, "only one usage"):
		return fmt.Sprintf("port %s is already in use by another program. Stop the other program (common culprits: nginx, Apache, IIS, another Open Fabrica instance) or set OPENFABRICA_TLS_DISABLED=1 to skip TLS bind",
			portOf(addr))
	case strings.Contains(msg, "permission denied"), strings.Contains(msg, "access is denied"):
		return fmt.Sprintf("insufficient permissions to bind port %s. On Linux: run with sudo, or grant the capability once via `sudo setcap cap_net_bind_service=+ep ./openfabrica`. On Windows: run as Administrator",
			portOf(addr))
	default:
		return err.Error()
	}
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}
