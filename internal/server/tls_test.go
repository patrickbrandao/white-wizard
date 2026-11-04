package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

const selfSignedCfg = "server: {seed: 42, tls: {self_signed: true}}\n" + latCfg

// serveTLS starts yaml on a real loopback listener and returns the server
// and its https base URL.
func serveTLS(t *testing.T, yaml string) (*Server, string) {
	t.Helper()
	s, err := New(newEngine(t, yaml), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		noLeaks(t)
	})
	return s, "https://" + ln.Addr().String()
}

func tlsClient(tc *tls.Config) *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tc, DisableKeepAlives: true},
	}
}

func postChat(t *testing.T, cl *http.Client, base, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer wwz-key-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func TestSelfSignedTLSOpenAIChat(t *testing.T) {
	s, base := serveTLS(t, selfSignedCfg)
	cert := s.Certificate()
	if cert == nil {
		t.Fatal("no self-signed certificate generated")
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)

	// Verified against the generated certificate: the SAN covers 127.0.0.1.
	resp, body := postChat(t, tlsClient(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}), base,
		`{"model":"wwz-fast","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if resp.TLS == nil || !resp.TLS.PeerCertificates[0].Equal(cert) {
		t.Fatal("response not served with the self-signed certificate")
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Object != "chat.completion" || len(out.Choices) != 1 || out.Choices[0].Message.Content == "" {
		t.Fatalf("bad body (%v): %s", err, body)
	}

	// Streaming over TLS with a client that skips verification.
	resp, body = postChat(t, tlsClient(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}), base, //nolint:gosec // test client for a self-signed cert
		`{"model":"wwz-fast","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("data: [DONE]")) {
		t.Fatalf("stream status %d: %s", resp.StatusCode, body)
	}

	// A plain-HTTP client cannot talk to it.
	if _, err := http.Post(strings.Replace(base, "https", "http", 1)+"/v1/chat/completions", "application/json", strings.NewReader("{}")); err == nil {
		t.Fatal("plain HTTP accepted on a TLS listener")
	}
}

func TestSelfSignedTLSUntrustedByDefault(t *testing.T) {
	_, base := serveTLS(t, selfSignedCfg)
	cl := tlsClient(&tls.Config{MinVersion: tls.VersionTLS12})
	if _, err := cl.Get(base + "/v1/models"); err == nil {
		t.Fatal("self-signed certificate verified against the system pool")
	}
}

func TestSelfSignedCertificate(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	c, err := newSelfSignedCert([]string{"localhost", "127.0.0.1", "::1", "wwz.test", "localhost"}, now)
	if err != nil {
		t.Fatal(err)
	}
	leaf := c.Leaf
	if k, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || k.Curve != elliptic.P256() {
		t.Fatalf("key is not ECDSA P-256: %T", leaf.PublicKey)
	}
	if !slices.Equal(leaf.DNSNames, []string{"localhost", "wwz.test"}) {
		t.Fatalf("dns names: %v", leaf.DNSNames)
	}
	if len(leaf.IPAddresses) != 2 || !leaf.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) || !leaf.IPAddresses[1].Equal(net.IPv6loopback) {
		t.Fatalf("ip addresses: %v", leaf.IPAddresses)
	}
	if !leaf.NotBefore.Before(now) || !leaf.NotAfter.Equal(now.Add(selfSignedValidity)) {
		t.Fatalf("validity %v - %v", leaf.NotBefore, leaf.NotAfter)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	for _, host := range []string{"localhost", "wwz.test", "127.0.0.1", "::1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: pool, CurrentTime: now}); err != nil {
			t.Errorf("verify %s: %v", host, err)
		}
	}
}

func TestSelfSignedHosts(t *testing.T) {
	hosts := selfSignedHosts("wwz.internal:8000")
	if !slices.Equal(hosts[:4], []string{"localhost", "127.0.0.1", "::1", "wwz.internal"}) {
		t.Fatalf("hosts: %v", hosts)
	}
	if got := selfSignedHosts(":8000"); slices.Contains(got, "") {
		t.Fatalf("empty host in %v", got)
	}
}

func TestSelfSignedOffOrOverridden(t *testing.T) {
	s, err := New(newEngine(t, fastCfg), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	if s.Certificate() != nil {
		t.Fatal("certificate generated without server.tls.self_signed")
	}
	// cert_file wins: no in-memory certificate is generated.
	s, err = New(newEngine(t, "server: {tls: {self_signed: true, cert_file: a.pem, key_file: a.key}}\n"+latCfg), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	if s.Certificate() != nil {
		t.Fatal("certificate generated although cert_file is set")
	}
}
