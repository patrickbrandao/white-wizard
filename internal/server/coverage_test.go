package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

func TestNewRejectsBadTrustedProxiesAndDefaultsLogger(t *testing.T) {
	eng := newEngine(t, "server: {trusted_proxies: ['not-a-cidr']}\n"+latCfg)
	if _, err := New(eng, Options{Logger: quietLog}); err == nil {
		t.Fatal("expected an error for an invalid trusted proxy")
	}
	// Without a logger the data plane logs through slog's default.
	s, err := New(newEngine(t, latCfg), Options{})
	if err != nil || s.log == nil {
		t.Fatalf("New without logger: %v", err)
	}
	_ = s.Shutdown(context.Background())
}

func TestListenFailureWithoutReuseport(t *testing.T) {
	s, err := New(newEngine(t, "server: {reuseport: false}\n"+latCfg), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Listen("256.256.256.256:0"); err == nil {
		t.Fatal("expected a listen error")
	}
	ls, err := s.Listen("127.0.0.1:0")
	if err != nil || len(ls) != 1 {
		t.Fatalf("single listener: %v %d", err, len(ls))
	}
	_ = ls[0].Close()
	_ = s.Shutdown(context.Background())
}

func TestListenFailureWithReuseport(t *testing.T) {
	s, err := New(newEngine(t, "server: {reuseport: true}\n"+latCfg), Options{Logger: quietLog, Listeners: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Listen("256.256.256.256:0"); err == nil {
		t.Fatal("expected a listen error")
	}
	_ = s.Shutdown(context.Background())
}

// writeCertPair writes a throwaway certificate and key to dir.
func writeCertPair(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "wwz test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// server.tls.cert_file/key_file serve TLS from the files.
func TestServeTLSFromFiles(t *testing.T) {
	certFile, keyFile := writeCertPair(t, t.TempDir())
	s, err := New(newEngine(t, "server: {tls: {cert_file: "+certFile+", key_file: "+keyFile+"}}\n"+latCfg), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln) }()
	cl := tlsClient(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // throwaway certificate of the test
	resp, body := postChat(t, cl, "https://"+ln.Addr().String(), `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
	<-errc
}

// A key file that does not exist ends Serve with the error.
func TestServeTLSMissingFiles(t *testing.T) {
	s, err := New(newEngine(t, "server: {tls: {cert_file: /nonexistent/c.pem, key_file: /nonexistent/k.pem}}\n"+latCfg), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if err := s.Serve(ln); err == nil {
		t.Fatal("expected an error for missing certificate files")
	}
}

func TestRouterLookupMethodNotAllowedOnWildcard(t *testing.T) {
	s, err := New(newEngine(t, latCfg), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	rt := s.st.Load().routes
	if m, other := rt.lookup("GET", "/v1/models/gpt-test", driver.MapHeaders{}); m == nil || other {
		t.Fatalf("GET model: %+v %v", m, other)
	}
	if m, other := rt.lookup("DELETE", "/v1/models/gpt-test", driver.MapHeaders{}); m != nil || !other {
		t.Fatalf("DELETE on a wildcard path must be 405, got %+v %v", m, other)
	}
	if m, other := rt.lookup("GET", "/v1/nothing/here", driver.MapHeaders{}); m != nil || other {
		t.Fatalf("unknown path: %+v %v", m, other)
	}
	_ = s.Shutdown(context.Background())
}

func TestValidationWithoutConfigKnowsEveryModel(t *testing.T) {
	if known, retired := (&validation{}).Model(driver.KindChat, "whatever"); !known || retired {
		t.Fatalf("validation without config: %v %v", known, retired)
	}
}

// A non-streaming transport hang sleeps server.hang and then drops the
// connection with no response.
func TestFastWriterNonStreamingHang(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	w := newFastWriter(ctx, func() {}, 20*time.Millisecond, false)
	start := time.Now()
	if !w.Fault(core.TransportHang) {
		t.Fatal("hang is handled")
	}
	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("hang must wait")
	}
	if !w.faulted || w.transport != core.TransportHang {
		t.Fatalf("fault state: %v %v", w.faulted, w.transport)
	}
}

func TestFastWriterCloseConnTCPAndMemory(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	acc := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		acc <- c
	}()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv := <-acc
	w := &fastWriter{ctx: &fasthttp.RequestCtx{}, conn: srv}
	w.closeConn()
	// The peer sees a reset or EOF, never a hang.
	_ = cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := cli.Read(make([]byte, 1)); err == nil {
		t.Fatal("peer must observe the closed connection")
	}
	_ = cli.Close()
	// Non-TCP connections are simply closed.
	a, b := net.Pipe()
	w = &fastWriter{ctx: &fasthttp.RequestCtx{}, conn: a}
	w.closeConn()
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := b.Read(make([]byte, 1)); err == nil {
		t.Fatal("pipe must be closed")
	}
	// No connection at all: nothing to do.
	(&fastWriter{ctx: &fasthttp.RequestCtx{}}).closeConn()
}
