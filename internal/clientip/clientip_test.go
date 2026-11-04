package clientip

import "testing"

func TestDefaultList(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	r, err := Load(nil)
	if err != nil || r.Mode != "default" {
		t.Fatal(err, r)
	}
	cases := []struct{ peer, xff, xri, want, src string }{
		{"45.80.10.20:1", "6.6.6.6", "6.6.6.6", "45.80.10.20", "tcp"},
		{"172.18.0.5:2", "45.80.10.20", "", "45.80.10.20", "x-forwarded-for"},
		{"172.18.0.5:2", "6.6.6.6, 45.80.10.20", "", "45.80.10.20", "x-forwarded-for"},
		{"10.0.0.3:3", "", "45.80.10.20", "45.80.10.20", "x-real-ip"},
		{"192.168.1.10:4", "unknown", "45.80.10.20", "45.80.10.20", "x-real-ip"},
		{"172.18.0.5:5", "", "", "172.18.0.5", "tcp"},
		{"172.18.0.5:6", "2804:14c::5", "", "2804:14c::5", "x-forwarded-for"},
		{"[::ffff:172.18.0.5]:7", "45.80.10.20:4711", "", "45.80.10.20", "x-forwarded-for"},
		{"100.64.0.7:8", "45.80.10.20", "", "45.80.10.20", "x-forwarded-for"},
		{"127.0.0.1:9", "45.80.10.20", "", "45.80.10.20", "x-forwarded-for"},
		{"172.18.0.5:10", "45.80.10.20, 10.0.0.9", "", "45.80.10.20", "x-forwarded-for"},
		{"172.18.0.5:11", "10.0.0.9", "", "10.0.0.9", "x-forwarded-for"},
		{"[fd00::2]:12", "45.80.10.20", "", "45.80.10.20", "x-forwarded-for"},
		{"[2804:14c::9]:13", "6.6.6.6", "", "2804:14c::9", "tcp"},
	}
	for _, c := range cases {
		got := r.Resolve(c.peer, c.xff, c.xri)
		if got.ClientIP != c.want || got.Source != c.src {
			t.Errorf("%s xff=%q xri=%q: got %s/%s want %s/%s", c.peer, c.xff, c.xri, got.ClientIP, got.Source, c.want, c.src)
		}
	}
}

func TestEnvList(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "172.30.0.2/32")
	r, err := Load(nil)
	if err != nil || r.Mode != "env" {
		t.Fatal(err, r)
	}
	cases := []struct{ peer, xff, xri, want, src string }{
		{"172.30.0.2:1", "45.80.10.20", "", "45.80.10.20", "x-forwarded-for"},
		{"172.18.0.5:1", "45.80.10.20", "45.80.10.20", "172.18.0.5", "tcp"},
		{"172.30.0.2:1", "45.80.10.20, 10.0.0.9", "", "10.0.0.9", "x-forwarded-for"},
		{"100.64.0.7:1", "6.6.6.6", "", "100.64.0.7", "tcp"},
		{"172.30.0.7:1", "45.80.10.20", "45.80.10.20", "172.30.0.7", "tcp"},
	}
	for _, c := range cases {
		got := r.Resolve(c.peer, c.xff, c.xri)
		if got.ClientIP != c.want || got.Source != c.src || got.PeerIP == "" {
			t.Errorf("%s: got %+v want %s/%s", c.peer, got, c.want, c.src)
		}
	}
}

func TestValidation(t *testing.T) {
	for _, bad := range []string{"172.30.0.2/32, abc", "10.0.0.0/33"} {
		t.Setenv("TRUSTED_PROXIES", bad)
		if _, err := Load(nil); err == nil {
			t.Errorf("expected failure for %q", bad)
		}
	}
	t.Setenv("TRUSTED_PROXIES", "172.30.0.2/32, fd00:1::10")
	r, err := Load(nil)
	if err != nil || len(r.Prefixes) != 2 {
		t.Fatal(err, r)
	}
}

// TestPeerTrusted: PeerTrusted is the condition of every X-Forwarded-*
// header (ADR 0073), set by the same trusted set as X-Forwarded-For.
func TestPeerTrusted(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	r, err := Load([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		peer string
		want bool
	}{
		{"10.0.0.5:1", true},
		{"[::ffff:10.0.0.5]:2", true},
		{"45.80.10.20:3", false},
		{"127.0.0.1:4", false},
		{"pipe", false},
	} {
		if got := r.Resolve(c.peer, "", "").PeerTrusted; got != c.want {
			t.Errorf("%s: PeerTrusted %v, want %v", c.peer, got, c.want)
		}
	}
}
