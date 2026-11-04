package clientip

import (
	"strings"
	"testing"
)

func TestLoadConfigList(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	r, err := Load([]string{"203.0.113.0/24", "198.51.100.7"})
	if err != nil || r.Mode != "config" || len(r.Prefixes) != 2 {
		t.Fatal(err, r)
	}
	if got := r.Describe(); got != "203.0.113.0/24,198.51.100.7/32" {
		t.Fatalf("Describe %q", got)
	}
	if _, err := Load([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "server.trusted_proxies") {
		t.Fatalf("expected config error, got %v", err)
	}
}

func TestParseListEdges(t *testing.T) {
	p, err := ParseList([]string{"", "  ", "10.1.2.3/8", "::ffff:1.2.3.4"})
	if err != nil || len(p) != 2 {
		t.Fatal(err, p)
	}
	if p[0].String() != "10.0.0.0/8" || p[1].String() != "1.2.3.4/32" {
		t.Fatalf("masked/unmapped: %v", p)
	}
	if _, err := ParseList([]string{"999.1.1.1"}); err == nil {
		t.Fatal("bare invalid IP must fail")
	}
	if _, err := ParseList([]string{"1.2.3.4/99"}); err == nil {
		t.Fatal("bad prefix must fail")
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"1.2.3.4":          "1.2.3.4",
		" 1.2.3.4:80 ":     "1.2.3.4",
		"[2001:db8::1]":    "2001:db8::1",
		"[2001:db8::1]:90": "2001:db8::1",
		"fe80::1%eth0":     "fe80::1",
		"::ffff:9.9.9.9":   "9.9.9.9",
	} {
		a, ok := Normalize(in)
		if !ok || a.String() != want {
			t.Errorf("Normalize(%q) = %v,%v want %s", in, a, ok, want)
		}
	}
	for _, bad := range []string{"", "  ", "unknown", "[x]"} {
		if _, ok := Normalize(bad); ok {
			t.Errorf("Normalize(%q) must fail", bad)
		}
	}
}

func TestResolveLimitsAndFallbacks(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	r, _ := Load(nil)

	// Non-IP peer: returned verbatim as tcp.
	if got := r.Resolve("weird", "6.6.6.6", ""); got.ClientIP != "weird" || got.Source != "tcp" {
		t.Fatalf("non-ip peer: %+v", got)
	}

	// More than 30 hops: only the last 30 are examined.
	hops := make([]string, 0, 40)
	hops = append(hops, "45.80.10.20")
	for i := 0; i < 39; i++ {
		hops = append(hops, "10.0.0.1")
	}
	got := r.Resolve("172.18.0.5:1", strings.Join(hops, ","), "")
	// All 30 kept hops are trusted: falls back to the first kept hop.
	if got.ClientIP != "10.0.0.1" || got.Source != "x-forwarded-for" {
		t.Fatalf(">30 hops: %+v", got)
	}

	// Header longer than 2048 bytes is cut to its tail.
	long := "45.80.10.20," + strings.Repeat(" ", 3000) + "8.8.8.8"
	got = r.Resolve("172.18.0.5:1", long, "")
	if got.ClientIP != "8.8.8.8" {
		t.Fatalf("long xff: %+v", got)
	}

	// Garbage hop with no valid right-side untrusted hop falls through to x-real-ip.
	got = r.Resolve("172.18.0.5:1", "garbage", "45.80.10.20")
	if got.ClientIP != "45.80.10.20" || got.Source != "x-real-ip" {
		t.Fatalf("invalid xff: %+v", got)
	}
	// x-real-ip with a comma is ignored.
	got = r.Resolve("172.18.0.5:1", "", "1.1.1.1, 2.2.2.2")
	if got.ClientIP != "172.18.0.5" || got.Source != "tcp" {
		t.Fatalf("xri list: %+v", got)
	}
	// All hops trusted and valid: first hop wins.
	got = r.Resolve("172.18.0.5:1", "10.0.0.9, 10.0.0.8", "")
	if got.ClientIP != "10.0.0.9" || got.Source != "x-forwarded-for" {
		t.Fatalf("all trusted: %+v", got)
	}
}
