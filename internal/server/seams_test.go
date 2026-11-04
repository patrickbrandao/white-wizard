package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

var errSeam = errors.New("seam failure")

func TestReuseportControlSetsockoptError(t *testing.T) {
	old := setsockoptInt
	t.Cleanup(func() { setsockoptInt = old })
	setsockoptInt = func(int, int, int, int) error { return errSeam }
	lc := net.ListenConfig{Control: reuseportControl}
	if _, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0"); !errors.Is(err, errSeam) {
		t.Fatalf("err = %v", err)
	}
}

func TestListenClosesPartialListeners(t *testing.T) {
	old := setsockoptInt
	t.Cleanup(func() { setsockoptInt = old })
	calls := 0
	setsockoptInt = func(fd, l, o, v int) error {
		calls++
		if calls == 2 {
			return errSeam
		}
		return syscall.SetsockoptInt(fd, l, o, v)
	}
	s, err := New(newEngine(t, "server: {reuseport: true}\n"+latCfg), Options{Logger: quietLog, Listeners: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Listen("127.0.0.1:0"); !errors.Is(err, errSeam) {
		t.Fatalf("err = %v", err)
	}
}

func TestSelfSignedCertErrors(t *testing.T) {
	oldGen, oldInt, oldCreate, oldParse := generateKey, randInt, createCertificate, parseCertificate
	reset := func() {
		generateKey, randInt, createCertificate, parseCertificate = oldGen, oldInt, oldCreate, oldParse
	}
	t.Cleanup(reset)
	now := time.Now()

	generateKey = func(elliptic.Curve, io.Reader) (*ecdsa.PrivateKey, error) { return nil, errSeam }
	if _, err := newSelfSignedCert([]string{"localhost"}, now); !errors.Is(err, errSeam) {
		t.Fatalf("key: %v", err)
	}
	reset()
	randInt = func(io.Reader, *big.Int) (*big.Int, error) { return nil, errSeam }
	if _, err := newSelfSignedCert([]string{"localhost"}, now); !errors.Is(err, errSeam) {
		t.Fatalf("serial: %v", err)
	}
	reset()
	createCertificate = func(io.Reader, *x509.Certificate, *x509.Certificate, any, any) ([]byte, error) {
		return nil, errSeam
	}
	if _, err := newSelfSignedCert([]string{"localhost"}, now); !errors.Is(err, errSeam) {
		t.Fatalf("create: %v", err)
	}
	reset()
	parseCertificate = func([]byte) (*x509.Certificate, error) { return nil, errSeam }
	if _, err := newSelfSignedCert([]string{"localhost"}, now); !errors.Is(err, errSeam) {
		t.Fatalf("parse: %v", err)
	}
}

func TestSelfSignedTLSErrorPropagates(t *testing.T) {
	oldGen := generateKey
	t.Cleanup(func() { generateKey = oldGen })
	failGen := func(elliptic.Curve, io.Reader) (*ecdsa.PrivateKey, error) { return nil, errSeam }

	// New fails at boot.
	generateKey = failGen
	if _, err := New(newEngine(t, selfSignedCfg), Options{Logger: quietLog}); !errors.Is(err, errSeam) {
		t.Fatalf("New: %v", err)
	}

	// Serve fails when a reload turns self_signed on.
	generateKey = oldGen
	eng := newEngine(t, fastCfg)
	s, err := New(eng, Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(selfSignedCfg))
	if err != nil {
		t.Fatal(err)
	}
	st, err := newState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.st.Store(st)
	generateKey = failGen
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := s.Serve(ln); !errors.Is(err, errSeam) {
		t.Fatalf("Serve: %v", err)
	}
}

func TestDefaultHangFor(t *testing.T) {
	if defaultHangFor != 60*time.Second {
		t.Fatalf("default = %v", defaultHangFor)
	}
}

// Without server.hang the hang fault lasts defaultHangFor.
func TestFastWriterHangDefault(t *testing.T) {
	old := defaultHangFor
	t.Cleanup(func() { defaultHangFor = old })
	defaultHangFor = 20 * time.Millisecond
	w := newFastWriter(&fasthttp.RequestCtx{}, func() {}, 0, false)
	start := time.Now()
	if !w.Fault(core.TransportHang) {
		t.Fatal("hang is handled")
	}
	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("hang must wait the default")
	}
}
