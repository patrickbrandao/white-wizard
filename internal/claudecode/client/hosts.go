package client

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// realDomains are the hosts of the real Anthropic service: the client
// refuses them and every subdomain (ADR 0074, item 12). There is no option
// to turn the refusal off.
var realDomains = []string{"anthropic.com", "claude.ai", "claude.com"}

// IsRealHost reports whether host (a hostname, without port) is
// anthropic.com, claude.ai, claude.com or a subdomain of one of them,
// ignoring case and a trailing dot.
func IsRealHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	for _, d := range realDomains {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// RealHostError is the refusal of a target on a real Anthropic host.
type RealHostError struct{ Host string }

func (e *RealHostError) Error() string {
	return "refusing real Anthropic host " + e.Host + ": this client only talks to the simulator"
}

// Target is a parsed ANTHROPIC_BASE_URL: where the client connects and what
// it writes in the Host header and before every path. It is only built by
// [ParseTarget], which refuses the real hosts, so no connection to them is
// ever opened.
type Target struct {
	raw      string // the URL without the trailing slash
	tls      bool
	addr     string // host:port to dial
	hostname string // TLS server name
	host     string // Host header (the authority of the URL)
	prefix   string // path of the URL without the trailing slash
}

// ParseTarget parses a base URL (http or https, optional path prefix) and
// refuses the real Anthropic hosts with a [*RealHostError].
func ParseTarget(raw string) (*Target, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("target %q: want http(s)://host[:port][/prefix]", raw)
	}
	hostname := u.Hostname()
	if IsRealHost(hostname) {
		return nil, &RealHostError{Host: strings.TrimSuffix(strings.ToLower(hostname), ".")}
	}
	t := &Target{
		raw:      strings.TrimRight(raw, "/"),
		tls:      u.Scheme == "https",
		hostname: hostname,
		host:     u.Host,
		prefix:   strings.TrimRight(u.EscapedPath(), "/"),
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if t.tls {
			port = "443"
		}
	}
	t.addr = net.JoinHostPort(hostname, port)
	return t, nil
}

// String returns the base URL.
func (t *Target) String() string { return t.raw }

// Host returns the Host header of the target.
func (t *Target) Host() string { return t.host }

// Prefix returns the path prefix of the target ("" at the root).
func (t *Target) Prefix() string { return t.prefix }
