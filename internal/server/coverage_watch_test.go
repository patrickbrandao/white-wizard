//go:build linux || darwin

package server

import (
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// fakeRaw is a RawConn whose Read follows a script.
type fakeRaw struct {
	calls   atomic.Int32
	release chan struct{}
	err     error // returned by every Read when set
}

func (f *fakeRaw) Control(func(uintptr)) error { return nil }
func (f *fakeRaw) Write(func(uintptr) bool) error {
	return nil
}

func (f *fakeRaw) Read(func(uintptr) bool) error {
	if f.err != nil {
		return f.err
	}
	if f.calls.Add(1) == 1 {
		return os.ErrDeadlineExceeded // fasthttp's own deadline, not ours
	}
	<-f.release // parked until stop() moves the deadline
	return os.ErrDeadlineExceeded
}

// fakeConn is a net.Conn that exposes a scripted RawConn.
type fakeConn struct {
	net.Conn
	raw     *fakeRaw
	rawErr  error
	once    sync.Once
	cleared atomic.Int32
}

func (c *fakeConn) SyscallConn() (syscall.RawConn, error) { return c.raw, c.rawErr }

func (c *fakeConn) SetReadDeadline(t time.Time) error {
	if t.IsZero() {
		c.cleared.Add(1)
		return nil
	}
	c.once.Do(func() { close(c.raw.release) })
	return nil
}

func TestWatchDisconnectSyscallConnFails(t *testing.T) {
	c := &fakeConn{raw: &fakeRaw{}, rawErr: errors.New("no raw conn")}
	stop := watchDisconnect(c, func() { t.Error("onGone must not run") })
	stop()
}

func TestWatchDisconnectSocketFailureCallsOnGone(t *testing.T) {
	c := &fakeConn{raw: &fakeRaw{err: errors.New("socket broke"), release: make(chan struct{})}}
	gone := make(chan struct{})
	stop := watchDisconnect(c, func() { close(gone) })
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("onGone not called")
	}
	stop()
}

func TestWatchDisconnectForeignDeadlineIsCleared(t *testing.T) {
	raw := &fakeRaw{release: make(chan struct{})}
	c := &fakeConn{raw: raw}
	stop := watchDisconnect(c, func() { t.Error("onGone must not run") })
	deadline := time.Now().Add(2 * time.Second)
	for c.cleared.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.cleared.Load() == 0 {
		t.Fatal("a deadline that is not ours must be cleared")
	}
	stop()
	stop() // safe to call twice
}

func TestValidationModelVariantMiss(t *testing.T) {
	cfg, err := config.Parse([]byte(fastCfg))
	if err != nil {
		t.Fatal(err)
	}
	drv, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	v := &validation{cfg: cfg, protocol: "openai", drv: drv}
	// An unknown id with a variant suffix is answered by the engine itself.
	if known, retired := v.Model(driver.KindChat, "white-wizard/wwz-fast:free"); !known || retired {
		t.Fatalf("variant miss: %v %v", known, retired)
	}
}
