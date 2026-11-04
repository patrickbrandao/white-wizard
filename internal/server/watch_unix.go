//go:build linux || darwin

package server

import (
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// watchSupported reports whether watchDisconnect detects a client that went
// away while the response is still being prepared.
const watchSupported = true

// watchDisconnect calls onGone when the peer of c closes or resets the
// connection. It peeks at the socket (MSG_PEEK) through the runtime
// netpoller, so it consumes no bytes and costs one parked goroutine. Data
// from the client (a pipelined request) ends the watch without calling
// onGone. The returned stop function ends the watch and restores the read
// deadline; it must be called before fasthttp reads from c again and is safe
// to call more than once. Connections that are not raw sockets (TLS, in
// memory pipes) are not watched: stop is a no-op and the transport still
// notices the disconnect on the next streamed write.
func watchDisconnect(c net.Conn, onGone func()) (stop func()) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return func() {}
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return func() {}
	}
	var (
		mu      sync.Mutex
		stopped bool
		done    = make(chan struct{})
	)
	go func() {
		defer close(done)
		var buf [1]byte
		for {
			gone := false
			err := rc.Read(func(fd uintptr) bool {
				n, _, rerr := syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK)
				if rerr == syscall.EAGAIN || rerr == syscall.EINTR {
					return false // wait for readability
				}
				gone = n == 0 || rerr != nil
				return true
			})
			if err == nil {
				if gone {
					onGone()
				}
				return
			}
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				onGone() // the socket itself failed
				return
			}
			// A deadline fired: ours (stop) or fasthttp's read timeout,
			// which does not apply while the handler runs.
			mu.Lock()
			if stopped {
				mu.Unlock()
				return
			}
			_ = c.SetReadDeadline(time.Time{})
			mu.Unlock()
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			stopped = true
			_ = c.SetReadDeadline(time.Unix(1, 0))
			mu.Unlock()
			<-done
			_ = c.SetReadDeadline(time.Time{})
		})
	}
}
