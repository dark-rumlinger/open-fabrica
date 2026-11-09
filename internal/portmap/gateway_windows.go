package portmap

import (
	"encoding/binary"
	"errors"
	"net"
	"syscall"
	"unsafe"
)

var procGetBestRoute = syscall.NewLazyDLL("iphlpapi.dll").NewProc("GetBestRoute")

// mibIPForwardRow mirrors MIB_IPFORWARDROW: fourteen DWORDs.
type mibIPForwardRow struct {
	Dest, Mask, Policy, NextHop, IfIndex, Type, Proto, Age, NextHopAS uint32
	Metric1, Metric2, Metric3, Metric4, Metric5                       uint32
}

// defaultGateway asks the routing table which next hop an internet-bound packet
// would take. GetBestRoute is a table lookup; it sends nothing.
func defaultGateway() (net.IP, error) {
	var row mibIPForwardRow
	// 8.8.8.8 stands for "anywhere on the internet"; it reads the same in
	// either byte order.
	const anyPublic = 0x08080808
	if rc, _, _ := procGetBestRoute.Call(anyPublic, 0, uintptr(unsafe.Pointer(&row))); rc != 0 {
		return nil, syscall.Errno(rc)
	}
	if row.NextHop == 0 {
		return nil, errors.New("no IPv4 default route")
	}
	// Addresses are stored in network byte order, so the in-memory bytes of
	// the DWORD are the dotted-quad octets.
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, row.NextHop)
	return ip, nil
}
