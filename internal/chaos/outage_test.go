package chaos

import (
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func outages(t *testing.T, list ...config.Outage) []config.Outage {
	t.Helper()
	for i := range list {
		if err := list[i].Compile(); err != nil {
			t.Fatal(err)
		}
	}
	return list
}

func pct(v float64) *float64 { return &v }

// TestOutageWindowsSecondsMinutesHours: windows in seconds, minutes and hours
// fail the request only inside the window, with retry-after up to its end;
// outside, the profile decides as before (rate 0: no error).
func TestOutageWindowsSecondsMinutesHours(t *testing.T) {
	list := outages(t,
		config.Outage{Schedule: "*/30 * * * * *", Duration: 5 * time.Second},
		config.Outage{Schedule: "0 */10 * * * *", Duration: time.Minute, Type: "server_error"},
		config.Outage{Schedule: "0 0 */6 * * *", Duration: time.Hour, Type: "bad_gateway"},
	)
	cases := []struct {
		now   string
		kind  core.ErrorKind
		retry time.Duration
	}{
		{"2026-10-31T01:00:30Z", core.ErrOverloaded, 5 * time.Second},
		{"2026-10-31T01:00:34.250Z", core.ErrOverloaded, 750 * time.Millisecond},
		{"2026-10-31T01:00:35Z", core.ErrServerError, 25 * time.Second},
		{"2026-10-31T01:02:35Z", "", 0},
		{"2026-10-31T01:10:40Z", core.ErrServerError, 20 * time.Second},
		{"2026-10-31T01:11:10Z", "", 0},
		{"2026-10-31T06:00:31Z", core.ErrOverloaded, 4 * time.Second}, // first window listed wins
		{"2026-10-31T06:59:59Z", core.ErrBadGateway, time.Second},
		{"2026-10-31T07:00:00Z", core.ErrOverloaded, 5 * time.Second},
		{"2026-10-31T07:00:10Z", core.ErrServerError, 50 * time.Second},
		{"2026-10-31T07:01:10Z", "", 0},
	}
	in := New()
	p := &config.ErrorProfile{}
	for _, tc := range cases {
		d := in.DecideQuery(Query{Profile: p, Model: "m", Driver: "openai", Outages: [][]config.Outage{list}}, rng(), at(tc.now))
		if tc.kind == "" {
			if d.Pre != nil || d.Outage {
				t.Errorf("%s: want no error, got %+v", tc.now, d.Pre)
			}
			continue
		}
		if d.Pre == nil || d.Pre.Kind != tc.kind || d.Pre.RetryAfter != tc.retry || !d.Outage || d.Source != "profile" {
			t.Errorf("%s: got %+v (outage %v, source %q), want %s retry %s", tc.now, d.Pre, d.Outage, d.Source, tc.kind, tc.retry)
		}
	}
}

// TestOutageIgnoresProfileRate: inside a window the profile's rate and
// after_n are not drawn; only the window's rate applies.
func TestOutageIgnoresProfileRate(t *testing.T) {
	in := New()
	p := &config.ErrorProfile{Rate: 0, AfterN: 1000}
	list := outages(t, config.Outage{Schedule: "* * * * * *", Duration: time.Second, Type: "connection_reset"})
	d := in.DecideQuery(Query{Profile: p, Model: "m", Outages: [][]config.Outage{list}}, rng(), at("2026-10-31T00:00:00Z"))
	if d.Pre == nil || d.Pre.Transport != core.TransportReset {
		t.Fatalf("transport outage: %+v", d.Pre)
	}
	if n := in.count("m"); n != 2 {
		t.Fatalf("an outage request still counts for after_n: %d", n)
	}
	zero := outages(t, config.Outage{Schedule: "* * * * * *", Duration: time.Second, Rate: pct(0)})
	if d := in.DecideQuery(Query{Profile: p, Model: "m", Outages: [][]config.Outage{zero}}, rng(), at("2026-10-31T00:00:00Z")); d.Pre != nil {
		t.Fatalf("rate 0 window must not fail: %+v", d.Pre)
	}
}

// TestOutageRateDeterministic: a partial window rate is drawn from the
// request RNG, so the same seed and clock reproduce the same decisions.
func TestOutageRateDeterministic(t *testing.T) {
	list := outages(t, config.Outage{Schedule: "* * * * * *", Duration: time.Second, Rate: pct(50)})
	run := func() string {
		in := New()
		var b strings.Builder
		for i := 0; i < 64; i++ {
			r := generate.NewRNG(42, "req", string(rune('a'+i%26)), string(rune('A'+i/26)))
			d := in.DecideQuery(Query{Profile: &config.ErrorProfile{}, Model: "m", Outages: [][]config.Outage{list}}, r, at("2026-10-31T00:00:00Z"))
			if d.Pre != nil {
				b.WriteByte('x')
			} else {
				b.WriteByte('.')
			}
		}
		return b.String()
	}
	a, b := run(), run()
	if a != b || !strings.Contains(a, "x") || !strings.Contains(a, ".") {
		t.Fatalf("runs %q / %q", a, b)
	}
}

// TestOutageLevelsUnion: windows of every level apply; a list reached twice
// (the same profile through model and key) is evaluated once; the drivers
// filter limits a window to some drivers.
func TestOutageLevelsUnion(t *testing.T) {
	root := outages(t, config.Outage{Schedule: "0 * * * * *", Duration: 5 * time.Second, Drivers: []string{"anthropic"}})
	model := outages(t, config.Outage{Schedule: "30 * * * * *", Duration: 5 * time.Second, Rate: pct(50)})
	in := New()
	q := Query{Profile: &config.ErrorProfile{}, Model: "m", Driver: "openai", Outages: [][]config.Outage{root, nil, model, model}}
	if d := in.DecideQuery(q, rng(), at("2026-10-31T00:00:01Z")); d.Pre != nil {
		t.Fatalf("root window is anthropic-only: %+v", d.Pre)
	}
	q.Driver = "anthropic"
	if d := in.DecideQuery(q, rng(), at("2026-10-31T00:00:01Z")); d.Pre == nil {
		t.Fatal("root window must fail anthropic")
	}
	// One draw for the repeated list: the RNG advanced exactly once.
	r1, r2 := rng(), rng()
	if d := in.DecideQuery(q, r1, at("2026-10-31T00:00:31Z")); d.Pre != nil {
		t.Fatal("the draw must miss for this check to see a second draw")
	}
	r2.Float64()
	if r1.Float64() != r2.Float64() {
		t.Fatal("a repeated outage list must be drawn once")
	}
}

// TestOutageAdminWins: admin chaos (events, scoped profiles, one-shot) beats
// an outage window; a nil profile (no admin decision) evaluates nothing.
func TestOutageAdminWins(t *testing.T) {
	list := outages(t, config.Outage{Schedule: "* * * * * *", Duration: time.Second})
	now := at("2026-10-31T00:00:00Z")
	in := New()
	if _, err := in.Start(EventSpec{Type: "rate_limit", Duration: time.Minute}, now); err != nil {
		t.Fatal(err)
	}
	d := in.DecideQuery(Query{Profile: &config.ErrorProfile{}, Model: "m", Outages: [][]config.Outage{list}}, rng(), now)
	if d.Pre == nil || d.Pre.Kind != core.ErrRateLimit || d.Source != "admin" || d.Outage {
		t.Fatalf("admin event must win: %+v %q", d.Pre, d.Source)
	}
	in.Reset()
	if d := in.DecideQuery(Query{Model: "m", Outages: [][]config.Outage{list}}, rng(), now); d.Pre != nil {
		t.Fatalf("nil profile: %+v", d.Pre)
	}
}

func TestRegisteredTypeCheck(t *testing.T) {
	_, err := config.Parse([]byte("errors:\n  outages:\n    - {schedule: '* * * * * *', duration: 1s, type: not_a_type}\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown error type "not_a_type"`) {
		t.Fatalf("chaos must register its error types with config: %v", err)
	}
	if _, err := config.Parse([]byte("errors:\n  outages:\n    - {schedule: '* * * * * *', duration: 1s, type: hang}\n")); err != nil {
		t.Fatalf("transport fault type: %v", err)
	}
}
