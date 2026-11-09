package portmap

import (
	"net"
	"os"
)

// defaultGateway reads the IPv4 default route from /proc/net/route.
func defaultGateway() (net.IP, error) {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil, err
	}
	return parseProcNetRoute(string(data))
}
