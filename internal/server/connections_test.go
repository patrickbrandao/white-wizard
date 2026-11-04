package server

import (
	"testing"
	"time"
)

// OpenConnections feeds wwz_connections_open (spec 013).
func TestOpenConnections(t *testing.T) {
	h := start(t, "", nil)
	if n := h.s.OpenConnections(); n != 0 {
		t.Fatalf("idle server: %d", n)
	}
	c, err := h.dial(nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "one open connection", 3*time.Second, func() bool { return h.s.OpenConnections() == 1 })
	_ = c.Close()
	waitFor(t, "connection closed", 3*time.Second, func() bool { return h.s.OpenConnections() == 0 })
}
