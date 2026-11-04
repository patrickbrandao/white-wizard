//go:build linux || darwin

package server

import "syscall"

// setsockoptInt is a test seam over syscall.SetsockoptInt.
var setsockoptInt = syscall.SetsockoptInt

// reuseportControl sets SO_REUSEPORT so several listeners share one port and
// the kernel spreads connections across them.
func reuseportControl(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = setsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1)
	})
	if err != nil {
		return err
	}
	return serr
}

const reuseportSupported = true
