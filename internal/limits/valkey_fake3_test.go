package limits_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	fv "github.com/patrickbrandao/white-wizard/internal/limits/storetest/fakevalkey"
)

func lfTweak(interval time.Duration) valkeyTweak {
	return func(c *config.StoreConfig, o *limits.ValkeyOptions) {
		c.LocalFirst = true
		o.SyncInterval = interval
	}
}

func TestFakeLocalFirstEdges(t *testing.T) {
	ctx := context.Background()
	v, _ := fakeStore(t, func(r fv.Request) string {
		if r.Name == evn(sCheck) {
			return checkReply(1, 0, "", 0, 0)
		}
		return ""
	}, lfTweak(time.Hour))
	now := time.Now()
	ent := func(id string) limits.Entity { return limits.Entity{Kind: limits.KindKey, ID: id} }
	local := func(id string, rps, rpm int) limits.Subject {
		return limits.Subject{Entity: ent(id), Limits: config.Limits{RPS: rps, RPM: rpm}}
	}
	check := func(subs ...limits.Subject) limits.Decision {
		t.Helper()
		d, err := v.Check(ctx, limits.CheckRequest{Subjects: subs, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	// rpm denial of the only subject returns its own rps token.
	check(local("one", 100, 1))
	if d := check(local("one", 100, 1)); d.Allowed || d.LimitName != "rpm" {
		t.Fatalf("rpm denial: %+v", d)
	}
	// rpm denial of the second subject returns the tokens of the first.
	check(local("two", 100, 1))
	if d := check(local("first", 100, 100), local("two", 100, 1)); d.Allowed || d.LimitName != "rpm" {
		t.Fatalf("second subject denial: %+v", d)
	}

	// The denial reads the token units from Valkey; its failure is only logged.
	tok := limits.Subject{Entity: ent("tok"), Limits: config.Limits{RPM: 1, TPM: 100}}
	check(tok)
	if d := check(tok); d.Allowed || d.LimitName != "rpm" {
		t.Fatalf("rpm denial with token units: %+v", d)
	}

	// Closing syncs once more; the sync fails and the consumption is kept.
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFakeLocalFirstDefaultInterval(t *testing.T) {
	v, _ := fakeStore(t, nil, lfTweak(0))
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFakeQueueTimeoutRoundsUp(t *testing.T) {
	v, srv := fakeStore(t, func(r fv.Request) string {
		if r.Name == evn(sCheck) {
			return checkReply(1, 0, "", 0, 0)
		}
		return ""
	})
	l := config.Limits{Concurrency: 1, OnLimit: "queue", QueueTimeout: 500 * time.Microsecond}
	if _, err := v.Check(context.Background(), limits.CheckRequest{Subjects: oneSubject(l), Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, c := range srv.Calls() {
		if c.Name == evn(sCheck) {
			for _, a := range c.Args {
				if a == "1" {
					return
				}
			}
		}
	}
	t.Fatal("queue timeout not sent")
}

func TestFakePingAndTimeoutless(t *testing.T) {
	ctx := context.Background()
	ping := fv.Err("ERR ping")
	exists := fv.Err("ERR exists")
	v, srv := fakeStore(t, func(r fv.Request) string {
		switch r.Name {
		case evn(sCheck):
			return checkReply(1, 0, "", 0, 0)
		case "PING":
			return ping
		case "EXISTS":
			return exists
		}
		return ""
	}, func(c *config.StoreConfig, _ *limits.ValkeyOptions) { c.ReadTimeout = 0 })
	// A failed PING returns at once, without the EXISTS probes (task 410:
	// deterministic coverage of that path).
	if err := v.Ping(ctx); err == nil {
		t.Fatal("PING failure must surface")
	}
	for _, c := range srv.Calls() {
		if c.Name == "EXISTS" {
			t.Fatal("EXISTS after a failed PING")
		}
	}
	ping = fv.OK()
	if err := v.Ping(ctx); err == nil {
		t.Fatal("EXISTS failure must surface")
	}
	exists = fv.Int(0)
	if err := v.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	// With no read timeout the call context is only cancelable.
	if _, err := v.Check(ctx, limits.CheckRequest{Subjects: oneSubject(config.Limits{RPM: 5}), Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestFakeFallbackFailures(t *testing.T) {
	v, _ := fakeStore(t, nil)
	cfg := testStoreConfig("unused")
	cfg.OnDown = "" // empty = local
	f := limits.NewFallback(context.Background(), cfg, func(context.Context) (*limits.Valkey, error) { return v, nil }, limits.FallbackOptions{ProbeInterval: time.Hour})
	t.Cleanup(func() { _ = f.Close() })

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	sub := []limits.Subject{{Entity: fkeyA, Limits: config.Limits{TPM: 100}}}
	if _, err := f.Check(gone, limits.CheckRequest{Subjects: sub, Now: time.Now()}); err == nil {
		t.Fatal("canceled caller must see the error")
	}
	if err := f.Finish(gone, limits.FinishRequest{Subjects: sub}); err == nil {
		t.Fatal("canceled caller must see the error")
	}
	if _, err := f.Attempts(gone, "c", "s", time.Minute); err == nil {
		t.Fatal("canceled caller must see the error")
	}
	if _, err := f.Strike(gone, fkeyA, "r", time.Minute); err == nil {
		t.Fatal("canceled caller must see the error")
	}
	if f.CircuitOpen() {
		t.Fatal("a caller that went away is not a Valkey failure")
	}

	// Real failures count and fall back to memory.
	ctx := context.Background()
	if n, err := f.Attempts(ctx, "c", "s", time.Minute); err != nil || n != 1 {
		t.Fatalf("attempts fallback: %d %v", n, err)
	}
	if n, err := f.Strike(ctx, fkeyA, "r", time.Minute); err != nil || n != 1 {
		t.Fatalf("strike fallback: %d %v", n, err)
	}
}

func TestFakePoliciesResubscribeAfterError(t *testing.T) {
	v, srv := fakeStore(t, nil)
	var drops atomic.Int32
	srv.SetHandler(func(r fv.Request) string {
		if r.Name == "SUBSCRIBE" && drops.Add(1) == 1 {
			return fv.Err("ERR subscribe refused") // the loop retries after 1 s
		}
		if r.Name == "HGETALL" {
			return fv.Map()
		}
		return fv.OK()
	})
	v.WatchPolicies(func(limits.Policies, error) {})
	waitFor(t, "second SUBSCRIBE after the refusal", func() bool {
		n := 0
		for _, c := range srv.Calls() {
			if c.Name == "SUBSCRIBE" {
				n++
			}
		}
		return n >= 2
	})
}
