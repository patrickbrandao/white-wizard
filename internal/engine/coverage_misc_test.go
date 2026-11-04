package engine

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
)

func covEngine(t *testing.T) *Engine {
	t.Helper()
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, limits.NewMemory(), nil, NewMetrics(metrics.New(100), nil), nil)
}

func TestCoverageEngineAccessors(t *testing.T) {
	e := covEngine(t)
	e.SetPodName("pod-1")
	if e.podName != "pod-1" {
		t.Fatal("pod name")
	}
	if e.Started().IsZero() || time.Since(e.Started()) > time.Minute {
		t.Fatal("Started")
	}
}

func TestCoveragePromotion(t *testing.T) {
	e := covEngine(t)
	if e.Promoter() != nil || e.onUsage() != nil {
		t.Fatal("promotion is off by default")
	}
	if e.accountTier(&config.KeyConfig{Account: "a"}) != "" {
		t.Fatal("no promoter, no tier")
	}
	p := e.EnablePromotion(&limits.PolicyApplier{})
	if p == nil || e.Promoter() != p || p.Store == nil {
		t.Fatal("Memory store is a TierSwapper")
	}
	p.OnPromote("a", "b") // registered counter
	if e.onUsage() == nil {
		t.Fatal("onUsage must be set with a swapping store")
	}
	if e.accountTier(nil) != "" || e.accountTier(&config.KeyConfig{}) != "" || e.accountTier(&config.KeyConfig{Account: "a"}) != "" {
		t.Fatal("accountTier without assignment")
	}

	// A store that cannot swap tiers keeps the promoter but reads nothing back.
	e2 := covEngine(t)
	e2.Store = noSwap{e2.Store}
	e2.M = nil
	p2 := e2.EnablePromotion(nil)
	if p2.Store != nil || e2.onUsage() != nil {
		t.Fatal("store without TierSwapper")
	}
}

// noSwap hides the TierSwapper of the wrapped store.
type noSwap struct{ limits.Store }

func TestCoverageRecorderRing(t *testing.T) {
	r := NewRecorder(0)
	if len(r.buf) != 1000 {
		t.Fatal("default size")
	}
	r = NewRecorder(2)
	for i := 1; i <= 3; i++ {
		r.Add(Record{Status: i})
	}
	got := r.Last(10, nil)
	if len(got) != 2 || got[0].Status != 3 || got[1].Status != 2 {
		t.Fatalf("ring: %+v", got)
	}
	r.Clear()
	if len(r.Last(10, nil)) != 0 {
		t.Fatal("Clear")
	}
}

func TestCoverageResilienceEdges(t *testing.T) {
	rs := NewResilience()
	rs.Observe("", nil, 200, nil)
	var nilRS *Resilience
	nilRS.Observe("t", nil, 200, nil)
	if rs.Report("none") != nil {
		t.Fatal("unknown report")
	}
	// Past 10000 tests a new id is dropped.
	for i := 0; i < 10000; i++ {
		rs.tests[string(rune('a'))+time.Duration(i).String()] = &TestReport{}
	}
	rs.Observe("overflow", nil, 200, nil)
	if rs.Report("overflow") != nil {
		t.Fatal("tracker is capped")
	}
	rs.Reset()

	// 201+ retry intervals keep the last 200; 3 growing intervals flag backoff.
	for i := 0; i < 205; i++ {
		rs.Observe("t", nil, 429, nil)
	}
	if rep := rs.Report("t"); len(rep.Intervals) != 200 {
		t.Fatalf("intervals %d", len(rep.Intervals))
	}

	// List: limit and ordering by last activity.
	rs.Observe("older", nil, 200, nil)
	time.Sleep(2 * time.Millisecond)
	rs.Observe("newer", nil, 200, nil)
	l := rs.List(2)
	if len(l) != 2 || l[0].TestID != "newer" {
		t.Fatalf("list: %+v", l)
	}
	if len(rs.List(0)) != 3 {
		t.Fatal("no limit")
	}
	rs.Observe("retry", nil, 429, &core.Error{RetryAfter: time.Hour})
	rs.Observe("retry", nil, 200, nil)
	if rep := rs.Report("retry"); rep.RetryAfterSent != 1 || rep.RetryAfterEarly != 1 {
		t.Fatalf("retry-after early: %+v", rep)
	}
}
