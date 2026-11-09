//go:build !windows

package server

import "syscall"

// setReuseAddr enables SO_REUSEADDR on the listening socket so a fresh
// process can rebind the port even while the kernel still holds the previous
// socket in TIME_WAIT. Without this, a quick restart after a rough shutdown
// fails with "address already in use".
func setReuseAddr(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return sockErr
}
