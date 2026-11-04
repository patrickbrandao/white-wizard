package client

import (
	"errors"
	"testing"
)

func TestIsRealHost(t *testing.T) {
	for host, want := range map[string]bool{
		"anthropic.com": true, "api.anthropic.com": true, "API.Anthropic.COM.": true, "console.anthropic.com": true,
		"claude.ai": true, "x.claude.ai": true, "claude.com": true, "x.claude.com": true, "platform.claude.com": true,
		"notanthropic.com": false, "anthropic.com.evil.test": false, "claude.ai.example": false, "myclaude.com": false,
		"127.0.0.1": false, "localhost": false, "wwz.test": false, "": false,
	} {
		if got := IsRealHost(host); got != want {
			t.Errorf("IsRealHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestParseTargetRefusesRealHosts: a target on a real Anthropic host is
// refused with the message of spec 015; there is no way around it.
func TestParseTargetRefusesRealHosts(t *testing.T) {
	for _, raw := range []string{
		"https://api.anthropic.com", "https://api.anthropic.com/", "http://API.ANTHROPIC.COM.:443/v1",
		"https://claude.ai", "https://x.claude.com/anthropic", "https://claude.com", "https://platform.claude.com:8443",
	} {
		tg, err := ParseTarget(raw)
		var re *RealHostError
		if tg != nil || !errors.As(err, &re) {
			t.Fatalf("ParseTarget(%q) = %v, %v", raw, tg, err)
		}
		if want := "refusing real Anthropic host " + re.Host + ": this client only talks to the simulator"; err.Error() != want {
			t.Errorf("message %q", err.Error())
		}
	}
	_, err := ParseTarget("https://Api.Anthropic.com./x")
	if err.Error() != "refusing real Anthropic host api.anthropic.com: this client only talks to the simulator" {
		t.Errorf("normalized host: %v", err)
	}
}

func TestParseTarget(t *testing.T) {
	for _, raw := range []string{"ftp://x", "http://", "http://h?q=1", "http://h#f", "http://u@h", "://bad", "h:80"} {
		if _, err := ParseTarget(raw); err == nil {
			t.Errorf("ParseTarget(%q) accepted", raw)
		}
	}
	cases := []struct{ raw, str, addr, host, prefix, hostname string }{
		{"http://127.0.0.1:8000/anthropic/", "http://127.0.0.1:8000/anthropic", "127.0.0.1:8000", "127.0.0.1:8000", "/anthropic", "127.0.0.1"},
		{"https://wwz.test", "https://wwz.test", "wwz.test:443", "wwz.test", "", "wwz.test"},
		{"http://wwz.test", "http://wwz.test", "wwz.test:80", "wwz.test", "", "wwz.test"},
		{"http://[::1]:9/a%20b", "http://[::1]:9/a%20b", "[::1]:9", "[::1]:9", "/a%20b", "::1"},
	}
	for _, tc := range cases {
		tg, err := ParseTarget(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if tg.String() != tc.str || tg.addr != tc.addr || tg.Host() != tc.host || tg.Prefix() != tc.prefix || tg.hostname != tc.hostname ||
			tg.tls != (tc.raw[:5] == "https") {
			t.Errorf("%s: %+v", tc.raw, tg)
		}
	}
}
