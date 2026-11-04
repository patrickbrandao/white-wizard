//go:build !linux && !darwin

package server

import "syscall"

// reuseportControl is unused where SO_REUSEPORT is not available.
func reuseportControl(_, _ string, _ syscall.RawConn) error { return nil }

const reuseportSupported = false
