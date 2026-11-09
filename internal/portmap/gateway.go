package portmap

import (
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"strings"
)

// parseProcNetRoute finds the default route (destination 0.0.0.0 with the
// gateway flag set) in the text of Linux's /proc/net/route. Addresses there are
// hex dumps of the address in little-endian order, so the bytes are reversed.
// Kept OS-agnostic so it is unit-tested on every platform.
func parseProcNetRoute(table string) (net.IP, error) {
	const rtfGateway = 0x2
	lines := strings.Split(table, "\n")
	if len(lines) > 0 {
		lines = lines[1:] // column header
	}
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) < 4 || f[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(f[3], 16, 32)
		if err != nil || flags&rtfGateway == 0 {
			continue
		}
		raw, err := hex.DecodeString(f[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		return net.IPv4(raw[3], raw[2], raw[1], raw[0]).To4(), nil
	}
	return nil, errors.New("no IPv4 default route")
}
