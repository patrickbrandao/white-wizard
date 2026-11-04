//go:build darwin

package server

import "syscall"

// soReusePort is SO_REUSEPORT as exposed by the syscall package on Darwin.
const soReusePort = syscall.SO_REUSEPORT
