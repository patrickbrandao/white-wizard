package server

import (
	"crypto/tls"
	"errors"
	"testing"
	"time"
)

type failRawConn struct{}

func (failRawConn) Control(func(uintptr)) error    { return errSeam }
func (failRawConn) Read(func(uintptr) bool) error  { return errSeam }
func (failRawConn) Write(func(uintptr) bool) error { return errSeam }

func TestReuseportControlRawConnError(t *testing.T) {
	if err := reuseportControl("tcp", "x", failRawConn{}); !errors.Is(err, errSeam) {
		t.Fatalf("err = %v", err)
	}
}

// A caller that waited on selfMu finds the certificate another one stored.
func TestSelfSignedTLSDoubleCheck(t *testing.T) {
	s, err := New(newEngine(t, fastCfg), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	want := &tls.Config{MinVersion: tls.VersionTLS12}
	s.selfMu.Lock()
	done := make(chan *tls.Config)
	go func() {
		tc, _ := s.selfSignedTLS()
		done <- tc
	}()
	time.Sleep(100 * time.Millisecond)
	s.selfTLS.Store(want)
	s.selfMu.Unlock()
	if got := <-done; got != want {
		t.Fatal("did not reuse the stored configuration")
	}
}
