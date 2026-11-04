//go:build !linux && !darwin

package server

import "net"

// watchSupported reports whether watchDisconnect detects a client that went
// away while the response is still being prepared.
const watchSupported = false

// watchDisconnect is a no-op on this platform: the transport notices a
// disconnect only on the next streamed write.
func watchDisconnect(net.Conn, func()) (stop func()) { return func() {} }
