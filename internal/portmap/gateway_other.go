//go:build !linux && !darwin && !windows

package portmap

import (
	"errors"
	"net"
)

// defaultGateway is not implemented here. NAT-PMP and PCP need it and are
// skipped; UPnP discovery is multicast and still works.
func defaultGateway() (net.IP, error) {
	return nil, errors.New("default gateway lookup not supported on this OS")
}
