package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"time"
)

// Test seams over the crypto primitives of newSelfSignedCert.
var (
	randReader        io.Reader = rand.Reader
	generateKey                 = ecdsa.GenerateKey
	randInt                     = rand.Int
	createCertificate           = x509.CreateCertificate
	parseCertificate            = x509.ParseCertificate
)

// selfSignedValidity is how long the in-memory certificate is valid.
const selfSignedValidity = 365 * 24 * time.Hour

// selfSignedHosts lists the SANs of the in-memory certificate: localhost,
// the loopback addresses, the host of server.listen (when set) and the
// machine hostname.
func selfSignedHosts(listen string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if h, _, err := net.SplitHostPort(listen); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	return hosts
}

// newSelfSignedCert builds an ECDSA P-256 certificate valid from now for
// selfSignedValidity, covering hosts (IPs go to IPAddresses, the rest to
// DNSNames; duplicates are dropped).
func newSelfSignedCert(hosts []string, now time.Time) (tls.Certificate, error) {
	key, err := generateKey(elliptic.P256(), randReader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("server: self-signed key: %w", err)
	}
	serial, err := randInt(randReader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("server: self-signed serial: %w", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"White Wizard"}, CommonName: "wwz self-signed"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	seen := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if seen[h] {
			continue
		}
		seen[h] = true
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := createCertificate(randReader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("server: self-signed certificate: %w", err)
	}
	leaf, err := parseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("server: self-signed certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// selfSignedTLS returns the TLS configuration with the in-memory
// certificate, generating it on first use (New when server.tls.self_signed
// is on at boot; Serve when a reload turns it on). The certificate lives as
// long as the Server; a restart issues a new one.
func (s *Server) selfSignedTLS() (*tls.Config, error) {
	if tc := s.selfTLS.Load(); tc != nil {
		return tc, nil
	}
	s.selfMu.Lock()
	defer s.selfMu.Unlock()
	if tc := s.selfTLS.Load(); tc != nil {
		return tc, nil
	}
	cert, err := newSelfSignedCert(selfSignedHosts(s.Config().Server.Listen), time.Now())
	if err != nil {
		return nil, err
	}
	tc := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}
	s.selfTLS.Store(tc)
	s.log.Warn("data plane uses a self-signed TLS certificate generated in memory; clients must skip verification or trust it explicitly",
		"dns", cert.Leaf.DNSNames, "ips", cert.Leaf.IPAddresses, "not_after", cert.Leaf.NotAfter)
	return tc, nil
}

// Certificate returns the in-memory self-signed certificate, or nil when
// none was generated (server.tls.self_signed off).
func (s *Server) Certificate() *x509.Certificate {
	tc := s.selfTLS.Load()
	if tc == nil {
		return nil
	}
	return tc.Certificates[0].Leaf
}
