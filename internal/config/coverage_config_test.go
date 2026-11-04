package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoverageDistUnmarshal(t *testing.T) {
	cases := []struct {
		in   string
		kind string
		err  bool
	}{
		{"", "", false},
		{"300ms", "fixed", false},
		{"'250'", "fixed", false},
		{"250", "fixed", false},
		{"'abc'", "", true},
		{"mean: 1s\nstddev: 100ms", "normal", false},
		{"median: 1s", "lognormal", false},
		{"min: 1s\nmax: 2s", "uniform", false},
		{"mean: 1s", "exponential", false},
		{"p99: 1s", "fixed", false},
		{"mean: notaduration", "", true},
	}
	for _, c := range cases {
		var d Dist
		err := d.UnmarshalYAML([]byte(c.in))
		if (err != nil) != c.err {
			t.Fatalf("%q: err=%v", c.in, err)
		}
		if d.Kind != c.kind {
			t.Fatalf("%q: kind %q want %q", c.in, d.Kind, c.kind)
		}
	}
	var d Dist
	_ = d.UnmarshalYAML([]byte("250"))
	if d.Value != 250*time.Millisecond {
		t.Fatalf("value %v", d.Value)
	}
}

func TestCoverageLoad(t *testing.T) {
	c, err := Load("")
	if err != nil || len(c.Models) == 0 {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("missing file: %v", err)
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("server:\n  seed: 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(p)
	if err != nil || c.Server.Seed != 7 {
		t.Fatalf("Load: %v", err)
	}
	if c.Raw() == nil {
		t.Fatal("Raw must hold the document")
	}
	c2, _ := Parse(nil)
	if c2.Raw() != nil {
		t.Fatal("Raw of defaults must be nil")
	}
}

func TestCoverageApplyEnv(t *testing.T) {
	t.Setenv("WWZ_LISTEN", ":1111")
	t.Setenv("WWZ_PORT", ":2222")
	t.Setenv("WWZ_ADMIN_LISTEN", ":3333")
	t.Setenv("WWZ_AUTH", "none")
	t.Setenv("WWZ_SEED", "42")
	t.Setenv("WWZ_VALKEY_ADDR", "a:1,b:2")
	t.Setenv("WWZ_STORE", "valkey")
	t.Setenv("WWZ_VALKEY_PASSWORD", "pw")
	t.Setenv("WWZ_LOG_LEVEL", "debug")
	t.Setenv("WWZ_REUSEPORT", "true")
	t.Setenv("WWZ_SCENARIOS_DIR", "/sc")
	c := &Config{}
	ApplyEnv(c)
	if c.Server.Listen != ":2222" || c.Admin.Listen != ":3333" || c.Server.Auth != "none" || c.Server.Seed != 42 ||
		c.Store.Kind != "valkey" || len(c.Store.Addr) != 2 || c.Store.Password != "pw" ||
		c.Logging.Level != "debug" || !c.Server.Reuseport || c.Scenarios.Dir != "/sc" {
		t.Fatalf("env not applied: %+v", c)
	}
	t.Setenv("WWZ_SEED", "x")
	t.Setenv("WWZ_REUSEPORT", "0")
	c = &Config{}
	ApplyEnv(c)
	if c.Server.Seed != 0 || c.Server.Reuseport {
		t.Fatalf("bad seed/reuseport: %+v", c.Server)
	}
}

func TestCoverageValidateErrors(t *testing.T) {
	m := "models:\n  - id: m1\n"
	cases := map[string]string{
		"models:\n  - id: ''\n":                              "id is required",
		m + "  - id: m1\n":                                   "duplicate model id",
		"models:\n  - id: m1\n    latency: nope\n":           "unknown latency profile",
		"models:\n  - id: m1\n    errors: nope\n":            "unknown errors profile",
		"models:\n  - id: m1\n    generation: nope\n":        "unknown generation profile",
		m + "server:\n  auth: bad\n":                         "server.auth",
		m + "store:\n  kind: bad\n":                          "store.kind",
		m + "store:\n  kind: valkey\n":                       "store.addr",
		m + "store:\n  on_down: bad\n":                       "on_down",
		m + "drivers:\n  - name: ''\n":                       "name is required",
		m + "drivers:\n  - name: openai\n    prefix: nope\n": "must start with /",
		m + "drivers:\n  - name: anthropic\n    options:\n      require_version: maybe\n": "require_version",
		m + "keys:\n  - key: k\n    latency: nope\n":                                      "unknown latency profile",
		m + "keys:\n  - key: k\n    errors: nope\n":                                       "unknown errors profile",
		m + "keys:\n  - key: k\n    limits:\n      rps: -1\n":                             "keys[0].limits",
		m + "resilience:\n  max_reports: -1\n":                                            "max_reports",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err=%v want %q", in, err, want)
		}
	}
}

func TestCoverageLegacyProtocolsKeepDrivers(t *testing.T) {
	c, err := Parse([]byte("models:\n  - id: m1\n    protocols: [openrouter]\n    drivers: [openai]\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := c.FindModel("m1")
	if len(m.Drivers) != 2 || m.Drivers[0] != "openrouter" || m.Drivers[1] != "openai" {
		t.Fatalf("drivers %v", m.Drivers)
	}
}

func TestCoverageFind(t *testing.T) {
	c, _ := Parse([]byte("models:\n  - id: m1\n    aliases: [al]\nkeys:\n  - key: k1\n"))
	if c.FindModel("al") == nil || c.FindModel("m1") == nil || c.FindModel("zz") != nil {
		t.Fatal("FindModel")
	}
	if c.FindKey("k1") == nil || c.FindKey("zz") != nil {
		t.Fatal("FindKey")
	}
}

func TestCoverageResolveProfile(t *testing.T) {
	named := map[string]Limits{"default": {RPS: 1}, "x": {RPS: 2}}
	if p, err := resolveProfile[Limits](nil, named); err != nil || p.RPS != 1 {
		t.Fatal(p, err)
	}
	if _, err := resolveProfile[Limits](nil, map[string]Limits{}); err == nil {
		t.Fatal("no default")
	}
	if p, err := resolveProfile[Limits]("x", named); err != nil || p.RPS != 2 {
		t.Fatal(p, err)
	}
	if _, err := resolveProfile[Limits]("zz", named); err == nil {
		t.Fatal("unknown")
	}
	if p, err := resolveProfile[Limits](map[string]any{"rps": 5}, named); err != nil || p.RPS != 5 {
		t.Fatal(p, err)
	}
	if _, err := resolveProfile[Limits](map[string]any{"bogus": 5}, named); err == nil {
		t.Fatal("strict")
	}
	if _, err := resolveProfile[Limits](make(chan int), named); err == nil {
		t.Fatal("marshal error")
	}
}

func TestCoverageMergeLimits(t *testing.T) {
	f := 1.5
	over := Limits{
		RPS: 1, Burst: 2, RPM: 3, RPD: 4, TPM: 5, ITPM: 6, OTPM: 7, TPD: 8, Concurrency: 9,
		MaxQueue: 10, QueueTimeout: time.Second, SpendMonthlyUSD: 1, SpendSoftUSD: 2,
		CreditsUSD: &f, QuotaUSD: &QuotaUSD{}, Algorithm: "fixed_window", TPMReserve: "none",
		ExcludeCacheRead: true, OnLimit: "queue", Mode: "after_n", AfterN: 3,
		Window: &Window{Every: time.Minute}, Ramp: &Ramp{},
	}
	got := mergeLimits(Limits{}, over)
	if got.RPS != 1 || got.Burst != 2 || got.RPM != 3 || got.RPD != 4 || got.TPM != 5 || got.ITPM != 6 ||
		got.OTPM != 7 || got.TPD != 8 || got.Concurrency != 9 || got.MaxQueue != 10 || got.QueueTimeout != time.Second ||
		got.SpendMonthlyUSD != 1 || got.SpendSoftUSD != 2 || got.CreditsUSD != &f || got.QuotaUSD == nil ||
		got.Algorithm != "fixed_window" || got.TPMReserve != "none" || !got.ExcludeCacheRead || got.OnLimit != "queue" ||
		got.Mode != "after_n" || got.AfterN != 3 || got.Window == nil || got.Ramp == nil {
		t.Fatalf("merge: %+v", got)
	}
	base := Limits{RPS: 9, Mode: "always"}
	if g := mergeLimits(base, Limits{}); g.RPS != 9 || g.Mode != "always" {
		t.Fatalf("empty override must keep base: %+v", g)
	}
}
