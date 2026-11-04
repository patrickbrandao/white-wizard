package chaos

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// TestBuildErrorEveryType: every known type builds an error of its own
// kind with a message; unknown names become server_error; retry-after uses
// the profile value or the per-kind default.
func TestBuildErrorEveryType(t *testing.T) {
	for k := range knownTypes {
		e := BuildError(string(k), nil)
		if e.Kind != k || e.Message == "" {
			t.Errorf("%s: %+v", k, e)
		}
	}
	defaults := map[string]time.Duration{"rate_limit": 20 * time.Second, "overloaded": 5 * time.Second, "slow_down": time.Minute}
	for name, want := range defaults {
		if e := BuildError(name, nil); e.RetryAfter != want {
			t.Errorf("%s retry-after %s, want %s", name, e.RetryAfter, want)
		}
		if e := BuildError(name, &config.ErrorProfile{RetryAfter: 3 * time.Second}); e.RetryAfter != 3*time.Second {
			t.Errorf("%s profile retry-after %s", name, e.RetryAfter)
		}
	}
	if e := BuildError("nope", nil); e.Kind != core.ErrServerError || e.Message != "injected error: nope" {
		t.Fatalf("unknown: %+v", e)
	}
	if e := BuildError("hang", nil); e.Transport != core.TransportHang {
		t.Fatalf("transport: %+v", e)
	}
}

func TestBuildErrorSlowHeadersDelay(t *testing.T) {
	if e := BuildError("slow_headers", nil); e.Transport != core.TransportSlowHeaders || e.TransportDelay != 15*time.Second {
		t.Fatalf("default: %+v", e)
	}
	p := &config.ErrorProfile{SlowHeadersDelay: 2 * time.Second}
	if e := BuildError("slow_headers", p); e.TransportDelay != 2*time.Second {
		t.Fatalf("profile: %+v", e)
	}
	if e := BuildError("hang", p); e.TransportDelay != 0 {
		t.Fatalf("other fault: %+v", e)
	}
}

func TestDecideAfterNAndBurst(t *testing.T) {
	in := New()
	p := &config.ErrorProfile{Rate: 100, Types: []string{"bad_gateway"}, AfterN: 2}
	for i := 1; i <= 3; i++ {
		d := in.Decide(p, "m", "", rng(), time.Now())
		if (d.Pre != nil) != (i > 2) {
			t.Fatalf("request %d: %+v", i, d.Pre)
		}
	}
	b := &config.ErrorProfile{Burst: &config.Burst{Every: time.Minute, Duration: 5 * time.Second, Rate: 100}, Types: []string{"overloaded"}}
	if d := in.Decide(b, "b", "", rng(), in.started.Add(time.Second)); d.Pre == nil || d.Pre.Kind != core.ErrOverloaded {
		t.Fatalf("inside burst: %+v", d.Pre)
	}
	if d := in.Decide(b, "b", "", rng(), in.started.Add(10*time.Second)); d.Pre != nil {
		t.Fatalf("outside burst: %+v", d.Pre)
	}
}

func TestDecideMidToolModeration(t *testing.T) {
	in := New()
	p := &config.ErrorProfile{
		MidStreamRate:   100,
		ToolMisbehavior: &config.ToolMisbehavior{Rate: 100},
		Moderation:      &config.Moderation{Probability: 100, Mode: "refusal"},
	}
	d := in.Decide(p, "m", "", rng(), time.Now())
	if d.Mid == nil || d.ToolBreak == "" || d.Moderation == nil || d.Source != "profile" {
		t.Fatalf("decision %+v", d)
	}
	switch d.Mid.Kind {
	case core.StreamErrorEvent, core.StreamTruncated, core.StreamHang:
	default:
		t.Fatalf("default mid kinds: %s", d.Mid.Kind)
	}
	p.MidStreamTypes = []string{"stream_error_event"}
	p.ToolMisbehavior.Kinds = []string{"unknown_tool"}
	p.HangDuration = time.Second
	d = in.Decide(p, "m", "", rng(), time.Now())
	if d.Mid.Kind != core.StreamErrorEvent || d.Mid.Error == nil || d.Mid.HangFor != time.Second || d.ToolBreak != "unknown_tool" {
		t.Fatalf("decision %+v", d)
	}
	if d := in.Decide(&config.ErrorProfile{}, "m", "", rng(), time.Now()); d.Source != "" {
		t.Fatalf("quiet profile: %+v", d)
	}
}

// TestDecideDefaultTypes: without types the pre-response error comes from
// the default list.
func TestDecideDefaultTypes(t *testing.T) {
	d := New().Decide(&config.ErrorProfile{Rate: 100}, "m", "", rng(), time.Now())
	if d.Pre == nil || !KnownType(string(d.Pre.Kind)) {
		t.Fatalf("decision %+v", d)
	}
}

func TestPickWeighted(t *testing.T) {
	names := []string{"a", "b", "c"}
	got := map[string]int{}
	r := generate.NewRNG(7, "w")
	for i := 0; i < 400; i++ {
		got[pickWeighted(names, map[string]float64{"a": 0, "c": 6}, r)]++
	}
	if got["c"] <= got["a"] || got["c"] <= got["b"] || got["a"] == 0 || got["b"] == 0 {
		t.Fatalf("weights: %v", got)
	}
	if pickWeighted([]string{"x"}, map[string]float64{"x": 5}, r) != "x" {
		t.Fatal("single name")
	}
}

func TestSetScopedNilDeletes(t *testing.T) {
	in := New()
	in.SetScoped("t", &config.ErrorProfile{Rate: 100})
	in.SetScoped("t", nil)
	if in.Scoped("t") != nil {
		t.Fatal("nil must delete the scoped profile")
	}
}

func TestScheduleNextPendingStop(t *testing.T) {
	in := New()
	if err := in.ScheduleNext(Next{}); err == nil {
		t.Fatal("test_id is required")
	}
	for _, id := range []string{"t2", "t1"} {
		if err := in.ScheduleNext(Next{TestID: id, Type: "overloaded"}); err != nil {
			t.Fatal(err)
		}
	}
	if p := in.PendingNext(); len(p) != 2 || p[0].TestID != "t1" {
		t.Fatalf("pending %+v", p)
	}
	in.SetScoped("t1", &config.ErrorProfile{Rate: 100})
	if n := in.Stop("", "t1"); n != 2 {
		t.Fatalf("stop t1: %d", n)
	}
	now := time.Now()
	if _, err := in.Start(EventSpec{Type: "overloaded", Duration: time.Minute}, now); err != nil {
		t.Fatal(err)
	}
	if ev := in.Events(now); len(ev) != 1 || ev[0].Type != "overloaded" {
		t.Fatalf("events %+v", ev)
	}
}
