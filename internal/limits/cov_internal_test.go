package limits

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	fv "github.com/patrickbrandao/white-wizard/internal/limits/storetest/fakevalkey"
)

func TestBucketEdges(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	var b bucket
	b.refill(t0, 10, 1)
	b.refill(t0, 5, 1) // smaller capacity clamps the tokens
	if b.tokens != 5 || b.cap != 5 {
		t.Fatalf("bucket after shrink: %+v", b)
	}
	if got := (&bucket{}).resetIn(); got != 0 {
		t.Fatalf("resetIn without rate = %v", got)
	}
	if !(&sliding{}).oldest().IsZero() {
		t.Fatal("oldest of an empty window")
	}
	if unitRank("other") != 0 {
		t.Fatal("unitRank default")
	}
}

func TestMemoryMisc(t *testing.T) {
	m := NewMemory()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m.maxEv = 2
	for range 3 {
		m.pushEvent(Event{})
	}
	if len(m.events) != 2 {
		t.Fatalf("events after trim = %d", len(m.events))
	}

	// Expired conversations are purged once the table is large.
	old := time.Now().Add(-time.Hour)
	for i := range 100001 {
		m.attempts[time.Duration(i).String()] = attempt{n: 1, exp: old}
	}
	if n, err := m.Attempts(context.Background(), "c", "s", time.Minute); err != nil || n != 1 {
		t.Fatalf("attempts: %d %v", n, err)
	}
	if len(m.attempts) != 1 {
		t.Fatalf("expired attempts not purged: %d", len(m.attempts))
	}
}

func TestMemoryPeekFixedWindowExpired(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	r := &entityRec{}
	r.tpmWin = window{start: t0, count: 10}
	var rl core.RateLimitState
	r.peekTokens(config.Limits{TPM: 100, Algorithm: "fixed_window"}, t0.Add(2*time.Minute), "rpm", &rl)
	if rl.TokensRemaining != 100 {
		t.Fatalf("expired window must start empty: %+v", rl)
	}
}

func TestMemoryLimitEdges(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	one := func(l config.Limits) Subject {
		return Subject{Entity: Entity{Kind: KindKey, ID: "k"}, Limits: l}
	}

	// Cached tokens above the estimate never make the reservation negative.
	m := NewMemory()
	d, err := m.Check(ctx, CheckRequest{Subjects: []Subject{one(config.Limits{TPM: 10, ExcludeCacheRead: true})}, EstPromptTokens: 5, CachedTokens: 50, Now: now})
	if err != nil || !d.Allowed {
		t.Fatalf("exclude_cache_read: %+v %v", d, err)
	}

	// input tokens per minute discount the cache read as well.
	m = NewMemory()
	d, err = m.Check(ctx, CheckRequest{Subjects: []Subject{one(config.Limits{ITPM: 100, ExcludeCacheRead: true})}, EstPromptTokens: 150, CachedTokens: 80, Now: now})
	if err != nil || !d.Allowed {
		t.Fatalf("itpm with cache: %+v %v", d, err)
	}

	// Fixed window tpm denial.
	m = NewMemory()
	d, err = m.Check(ctx, CheckRequest{Subjects: []Subject{one(config.Limits{TPM: 10, Algorithm: "fixed_window"})}, EstPromptTokens: 20, Now: now})
	if err != nil || d.Allowed || d.LimitName != "tpm" {
		t.Fatalf("fixed window tpm: %+v %v", d, err)
	}

	// Ramp without a window uses 15 minutes.
	m = NewMemory()
	d, err = m.Check(ctx, CheckRequest{Subjects: []Subject{one(config.Limits{Ramp: &config.Ramp{ThresholdTPM: 1, MaxIncreasePct: 10}})}, EstPromptTokens: 5, Now: now})
	if err != nil || !d.Allowed {
		t.Fatalf("ramp default window: %+v %v", d, err)
	}
}

func TestPromoterObserveDefaultTimeout(t *testing.T) {
	c := promoteConfig(t, promoteYAML)
	sw := &fakeSwapper{swapped: true}
	p := &Promoter{Store: sw, Config: func() *config.Config { return c }, Log: slog.New(slog.DiscardHandler)}
	p.Observe(Entity{Kind: KindAccount, ID: "acme"}, config.PromotionUsage{Requests: 1}) // nothing due
	p.Observe(Entity{Kind: KindAccount, ID: "acme"}, config.PromotionUsage{Requests: 3, PurchasedUSD: 5})
	p.Wait()
	if sw.n() != 1 {
		t.Fatalf("promotion not written: %d", sw.n())
	}
}

func TestKickPoliciesWhenFull(t *testing.T) {
	v := &Valkey{}
	v.pw.kick = make(chan struct{}, 1)
	v.kickPolicies()
	v.kickPolicies() // channel already holds a kick: dropped
	if len(v.pw.kick) != 1 {
		t.Fatal("kick must coalesce")
	}
}

func TestInstanceName(t *testing.T) {
	if instanceName("p", "h", nil) != "p" || instanceName("", "h", nil) != "h" ||
		instanceName("", "", nil) != "wwz" || instanceName("", "h", errors.New("x")) != "wwz" {
		t.Fatal("instanceName")
	}
	_ = defaultInstance()
}

func TestMustPanics(t *testing.T) {
	for name, fn := range map[string]func(){
		"lua":  func() { mustReadLua("nao-existe") },
		"yaml": func() { mustMarshalYAML(make(chan int)) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s: no panic", name)
				}
			}()
			fn()
		}()
	}
}

func internalStore(t *testing.T, h fv.Handler, lf bool) (*Valkey, *fv.Server) {
	t.Helper()
	srv := fv.New(t, h)
	cfg := config.StoreConfig{Kind: "valkey", Addr: []string{srv.Addr}, ReadTimeout: 2 * time.Second, Prefix: "wwz", EventStream: true, LocalFirst: lf}
	v, err := NewValkey(context.Background(), cfg, ValkeyOptions{Heartbeat: -1, SyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v, srv
}

func TestValkeyBuildPlainCommand(t *testing.T) {
	v, _ := internalStore(t, nil, false)
	if cmd := v.build(call{cmd: []string{"PING"}}, false); len(cmd.Commands()) != 1 || cmd.Commands()[0] != "PING" {
		t.Fatalf("plain command: %v", cmd.Commands())
	}
}

func TestValkeyPeekTokensFixedWindowAndFailure(t *testing.T) {
	fail := true
	var seen []string
	v, _ := internalStore(t, func(r fv.Request) string {
		if r.Name == "EVALSHA#8" {
			seen = append(seen, r.Args...)
			if fail {
				return fv.Err("ERR peek")
			}
			return fv.Ints(0, 0, 0, 100, 90, 1000, 0, 0, 0, 0)
		}
		return ""
	}, false)
	s := Subject{Entity: Entity{Kind: KindKey, ID: "k"}, Limits: config.Limits{TPM: 100, Algorithm: "fixed_window"}}
	var st core.RateLimitState
	v.peekTokens(context.Background(), s, time.Now(), &st)
	if st.Present {
		t.Fatal("a failed peek must leave the state alone")
	}
	fail = false
	v.peekTokens(context.Background(), s, time.Now(), &st)
	if !st.Present || st.TokensRemaining != 90 || seen[len(seen)-1] != "1" {
		t.Fatalf("peek: %+v %v", st, seen)
	}
}

func TestLocalFirstSyncPrunesIdleEntities(t *testing.T) {
	v, _ := internalStore(t, func(r fv.Request) string {
		if r.Name == "EVALSHA#7" {
			return fv.Ints(-1, -1, 0, 0, 0, 0)
		}
		return ""
	}, true)
	e := Entity{Kind: KindKey, ID: "k"}
	sub := Subject{Entity: e, Limits: config.Limits{RPS: 100}}
	ctx := context.Background()
	if d, err := v.Check(ctx, CheckRequest{Subjects: []Subject{sub}, Now: time.Now()}); err != nil || !d.Allowed {
		t.Fatalf("local check: %+v %v", d, err)
	}
	v.lf.sync(ctx) // sends the consumption and zeroes it
	v.lf.mu.Lock()
	v.lf.ents[e].last = time.Now().Add(-time.Hour)
	v.lf.mu.Unlock()
	v.lf.sync(ctx)
	v.lf.mu.Lock()
	n := len(v.lf.ents)
	v.lf.mu.Unlock()
	if n != 0 {
		t.Fatalf("idle entity not pruned: %d", n)
	}
}
