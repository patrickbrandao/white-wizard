// Package clientip derives the client IP behind a reverse proxy: headers are
// trusted only when the TCP peer is a trusted proxy, X-Forwarded-For is
// walked right-to-left, and both the derived IP and the peer IP are kept.
package clientip

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// DefaultPrivate is the default trusted set (RFC 5735 + RFC 6598 + IPv6 docs/ULA ranges).
var DefaultPrivate = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
	"2001:db8::/32", "4000::/2", "8000::/1",
}

// Resolver holds the effective trusted-proxy set.
type Resolver struct {
	Prefixes []netip.Prefix
	Mode     string // default | env | config
}

// Result is the derived client address.
type Result struct {
	ClientIP string
	PeerIP   string
	Source   string // x-forwarded-for | x-real-ip | tcp
	// PeerTrusted reports whether the TCP peer is a trusted proxy: the
	// condition for every X-Forwarded-* header, not only X-Forwarded-For
	// (the origin of the URLs of wwz itself, ADR 0073).
	PeerTrusted bool
}

// Load builds the resolver: TRUSTED_PROXIES env wins, then the config list,
// then the default private ranges. Invalid entries fail loudly.
func Load(configList []string) (*Resolver, error) {
	if raw := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES")); raw != "" {
		p, err := ParseList(strings.Split(raw, ","))
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES inválido: %w", err)
		}
		return &Resolver{Prefixes: p, Mode: "env"}, nil
	}
	if len(configList) > 0 {
		p, err := ParseList(configList)
		if err != nil {
			return nil, fmt.Errorf("server.trusted_proxies inválido: %w", err)
		}
		return &Resolver{Prefixes: p, Mode: "config"}, nil
	}
	p, _ := ParseList(DefaultPrivate)
	return &Resolver{Prefixes: p, Mode: "default"}, nil
}

// ParseList parses prefixes or bare IPs (bare IP = /32 or /128).
func ParseList(items []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		if strings.Contains(it, "/") {
			p, err := netip.ParsePrefix(it)
			if err != nil {
				return nil, fmt.Errorf("%q", it)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(it)
		if err != nil {
			return nil, fmt.Errorf("%q", it)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// Normalize strips port and zone and unmaps IPv4-in-IPv6. ok=false when not an IP.
func Normalize(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().WithZone("").Unmap(), true
	}
	s = strings.TrimPrefix(strings.TrimSuffix(s, "]"), "[")
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.WithZone("").Unmap(), true
}

// Trusted reports whether a is inside the trusted set.
func (r *Resolver) Trusted(a netip.Addr) bool {
	for _, p := range r.Prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Resolve applies the algorithm. xff is every X-Forwarded-For header joined by
// commas in order; xri is the first X-Real-IP.
func (r *Resolver) Resolve(remoteAddr, xff, xri string) Result {
	peerAddr, ok := Normalize(remoteAddr)
	peer := remoteAddr
	if ok {
		peer = peerAddr.String()
	}
	res := Result{ClientIP: peer, PeerIP: peer, Source: "tcp"}
	if !ok || !r.Trusted(peerAddr) {
		return res
	}
	res.PeerTrusted = true
	if xff = strings.TrimSpace(xff); xff != "" {
		if len(xff) > 2048 {
			xff = xff[len(xff)-2048:]
		}
		hops := strings.Split(xff, ",")
		if len(hops) > 30 {
			hops = hops[len(hops)-30:]
		}
		valid := true
		for i := len(hops) - 1; i >= 0; i-- {
			ip, ok := Normalize(hops[i])
			if !ok {
				valid = false
				break
			}
			if !r.Trusted(ip) {
				res.ClientIP, res.Source = ip.String(), "x-forwarded-for"
				return res
			}
		}
		if valid && len(hops) > 0 {
			if ip, ok := Normalize(hops[0]); ok {
				res.ClientIP, res.Source = ip.String(), "x-forwarded-for"
				return res
			}
		}
	}
	if ip, ok := Normalize(xri); ok && !strings.Contains(xri, ",") {
		res.ClientIP, res.Source = ip.String(), "x-real-ip"
		return res
	}
	return res
}

// Describe renders the effective prefixes for the startup log.
func (r *Resolver) Describe() string {
	parts := make([]string, 0, len(r.Prefixes))
	for _, p := range r.Prefixes {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, ",")
}
