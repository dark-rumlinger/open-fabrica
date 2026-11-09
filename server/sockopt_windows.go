package server

import "syscall"

// setReuseAddr is a no-op on Windows: unlike Linux, Windows SO_REUSEADDR lets
// two live processes bind the same port, silently letting a second instance
// steal half the traffic. A second bind must fail, so the option stays unset.
// Single-instance enforcement lives in internal/singleinstance.
func setReuseAddr(network, address string, c syscall.RawConn) error {
	return nil
}
