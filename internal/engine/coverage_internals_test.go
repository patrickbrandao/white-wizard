package engine

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

func covDriver(t *testing.T, name string) driver.Driver {
	t.Helper()
	d, err := driver.New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCoverageMergeLatency(t *testing.T) {
	d := &config.Dist{Kind: "fixed", Value: time.Second}
	over := config.LatencyProfile{
		Queue: d, TTFT: d, ITL: d, ReasoningITL: d, ToolCallDelay: d, ToolArgsITL: d, FinishDelay: d,
		LoadFactor: 2, ChunkTokens: 4, Pacing: &config.Pacing{}, Clock: "wall",
	}
	got := mergeLatency(config.LatencyProfile{}, over)
	if got.Queue != d || got.TTFT != d || got.ITL != d || got.ReasoningITL != d || got.ToolCallDelay != d ||
		got.ToolArgsITL != d || got.FinishDelay != d || got.LoadFactor != 2 || got.ChunkTokens != 4 ||
		got.Pacing == nil || got.Clock != "wall" {
		t.Fatalf("merge: %+v", got)
	}
	keep := mergeLatency(config.LatencyProfile{LoadFactor: 3, Clock: "x"}, config.LatencyProfile{})
	if keep.LoadFactor != 3 || keep.Clock != "x" {
		t.Fatal("empty override must keep base")
	}
}

func TestCoverageAuthenticate(t *testing.T) {
	cfg, err := config.Parse([]byte(`
keys:
  - {key: ok}
  - {key: ban, status: banned}
  - {key: sus, status: suspended}
  - {key: exp, status: expired}
  - {key: old, expires_at: "2000-01-01T00:00:00Z"}
  - {key: future, expires_at: "2999-01-01T00:00:00Z"}
  - {key: bad, expires_at: "not a date"}
  - {key: anth, driver: anthropic}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{}
	drv := covDriver(t, "openai")
	for key, kind := range map[string]core.ErrorKind{
		"": core.ErrInvalidAPIKey, "nokey": core.ErrInvalidAPIKey, "ban": core.ErrAccountBanned,
		"sus": core.ErrAccountSuspended, "exp": core.ErrInvalidAPIKey, "old": core.ErrInvalidAPIKey,
		"anth": core.ErrInvalidAPIKey,
	} {
		k, er := e.authenticate(cfg, drv, driver.Credential{Key: key})
		if er == nil || er.Kind != kind || k != nil {
			t.Errorf("%q: %v %+v", key, k, er)
		}
	}
	for _, key := range []string{"ok", "future", "bad"} {
		if k, er := e.authenticate(cfg, drv, driver.Credential{Key: key}); er != nil || k == nil {
			t.Errorf("%q: %+v", key, er)
		}
	}
	cfg.Server.Auth = "none"
	if k, er := e.authenticate(cfg, drv, driver.Credential{Key: "ok"}); er != nil || k == nil {
		t.Fatal("none + known key")
	}
	if k, er := e.authenticate(cfg, drv, driver.Credential{Key: "ghost"}); er != nil || k != nil {
		t.Fatal("none + unknown key")
	}
	if k, er := e.authenticate(cfg, drv, driver.Credential{}); er != nil || k != nil {
		t.Fatal("none + no key")
	}
}

func TestCoverageMissingError(t *testing.T) {
	if er := missingError(driver.Credential{Missing: []string{"x-opencode-session"}}); er.Kind != core.ErrMissingSession {
		t.Fatal("session")
	}
	if er := missingError(driver.Credential{Missing: []string{"anthropic-version"}}); er.Message != "anthropic-version: header is required" {
		t.Fatal(er.Message)
	}
}

func TestCoverageResolveModelWrappers(t *testing.T) {
	cfg, _ := config.Parse(nil)
	drv := covDriver(t, "openai")
	id := cfg.Models[0].ID
	if m, er := ResolveModel(cfg, drv, id); m == nil || er != nil {
		t.Fatal("known model")
	}
	if m, er := ResolveModel(cfg, drv, "nope"); m != nil || er != nil {
		t.Fatal("unknown model is no error")
	}
	if m, name, er := resolveModel(cfg, drv, id+"-scenario-x"); m == nil || er != nil || name != id+"-scenario-x" {
		t.Fatalf("scenario suffix: %v %q", m, name)
	}
	if m, _, er := resolveModel(cfg, drv, "nope-scenario-x"); m != nil || er != nil {
		t.Fatal("suffix on unknown model")
	}
	or := covDriver(t, "openrouter")
	slug := or.(driver.ModelNamer).ModelName(&cfg.Models[0])
	if _, _, er := resolveModel(cfg, or, slug+":free"); er == nil || er.Status != 404 {
		t.Fatalf("variant miss: %+v", er)
	}
}

func TestCoverageStatusOf(t *testing.T) {
	e := &Engine{}
	want := map[core.ErrorKind]int{
		core.ErrInvalidRequest: 400, core.ErrContextLength: 400, core.ErrInvalidAPIKey: 401,
		core.ErrPermission: 403, core.ErrAccountBanned: 403, core.ErrModelNotFound: 404,
		core.ErrRequestTooLarge: 413, core.ErrRateLimit: 429, core.ErrSpendLimit: 429,
		core.ErrBadGateway: 502, core.ErrOverloaded: 503, core.ErrGatewayTimeout: 504,
		core.ErrServerError: 500,
	}
	for k, s := range want {
		if got := e.statusOf(&core.Error{Kind: k}); got != s {
			t.Errorf("%v: %d want %d", k, got, s)
		}
	}
	if e.statusOf(&core.Error{Kind: core.ErrServerError, Status: 418}) != 418 {
		t.Fatal("own status wins")
	}
}

func TestCoverageSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), 0) || !sleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("sleep completes")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("cancelled sleep")
	}
}

func TestCoverageDenialError(t *testing.T) {
	st := func(f func(*core.RateLimitState)) core.RateLimitState {
		s := core.RateLimitState{Present: true}
		f(&s)
		return s
	}
	cases := []struct {
		name string
		dec  limits.Decision
		want string
	}{
		{"rpm", limits.Decision{Kind: core.ErrRateLimit, LimitName: "rpm", State: st(func(s *core.RateLimitState) { s.RequestsLimit, s.RequestsRemaining = 10, 0 })}, "rpm"},
		{"tpm", limits.Decision{Kind: core.ErrRateLimit, LimitName: "tpm", State: st(func(s *core.RateLimitState) { s.TokensLimit, s.TokensRemaining = 100, 5 })}, "tpm"},
		{"itpm", limits.Decision{Kind: core.ErrRateLimit, LimitName: "itpm", State: st(func(s *core.RateLimitState) { s.InputTokensLimit, s.InputTokensRemain = 100, 5 })}, "itpm"},
		{"otpm", limits.Decision{Kind: core.ErrRateLimit, LimitName: "otpm", State: st(func(s *core.RateLimitState) { s.OutputTokensLimit, s.OutputTokensRem = 100, 5 })}, "otpm"},
	}
	for _, c := range cases {
		er := denialError(c.dec, 10, 20)
		if er.Extra["limit_value"] == nil || er.Extra["limit_requested"] == nil {
			t.Errorf("%s: %+v", c.name, er.Extra)
		}
	}
	if er := denialError(limits.Decision{Kind: core.ErrRateLimit, LimitName: "rpm", State: st(func(*core.RateLimitState) {})}, 1, 1); er.Extra["limit_value"] != nil {
		t.Fatal("no limit, no numbers")
	}
	if er := denialError(limits.Decision{Kind: core.ErrRateLimit}, 1, 1); er.Extra["limit_value"] != nil {
		t.Fatal("no state")
	}
	fm := denialError(limits.Decision{Kind: core.ErrRateLimit, Entity: limits.Entity{Kind: limits.KindFreeModels}}, 1, 1)
	if fm.Extra["free_models"] != true {
		t.Fatal("free models flag")
	}
	if er := denialError(limits.Decision{Kind: core.ErrAccountBanned, Message: "operator text"}, 1, 1); er.Message != "operator text" {
		t.Fatal("ban reason passes through")
	}
	if er := denialError(limits.Decision{Kind: core.ErrAccountBanned, Message: abuseReason + " x"}, 1, 1); er.Message != "" {
		t.Fatal("internal reason hidden")
	}
}

func TestCoverageRequestHelpers(t *testing.T) {
	drv := covDriver(t, "openai")
	r := &request{in: Input{Driver: drv, Kind: driver.KindChat}, req: &core.Request{RequestID: "rid", Endpoint: core.EndpointChat, Model: "asked"}, rng: generate.NewRNG(1)}
	if r.eventModel() != "asked" || r.modelLabel() != "" {
		t.Fatal("unresolved model")
	}
	r.model = &config.ModelConfig{ID: "mid"}
	if r.eventModel() != "mid" || r.modelLabel() != "mid" {
		t.Fatal("resolved model")
	}
	if id := r.toolCallID(3); id == "" {
		t.Fatal("tool call id without minter rng")
	}
	r.idRNG = generate.NewRNG(2)
	if id := r.toolCallID(0); id == "" {
		t.Fatal("tool call id with minter")
	}
	an := &request{in: Input{Driver: covDriver(t, "anthropic")}, req: &core.Request{}, rng: generate.NewRNG(1)}
	if id := an.toolCallID(1); id == "" {
		t.Fatal("anthropic tool id")
	}
	// A driver that mints no ids falls back to call_<short>_<i>.
	nm := &request{in: Input{Driver: noMint{drv}}, req: &core.Request{}, rng: generate.NewRNG(1), idRNG: generate.NewRNG(2)}
	if id := nm.toolCallID(2); len(id) < 6 || id[:5] != "call_" {
		t.Fatalf("fallback id %q", id)
	}
	e := &Engine{}
	if id := e.newID(); id == "" || id == e.newID() {
		t.Fatal("newID unique")
	}
}

// noMint hides driver.IDMinter of the wrapped driver.
type noMint struct{ driver.Driver }

func TestCoverageAddDiagHeaders(t *testing.T) {
	e := &Engine{podName: "pod"}
	r := &request{
		in:  Input{Driver: covDriver(t, "openai")},
		req: &core.Request{RequestID: "rid"},
		scn: &scenario.Resolved{Scenario: "s", Step: "st", Attempt: 2},
	}
	er := &core.Error{}
	e.addDiagHeaders(r, er)
	if er.Extra["header:x-wwz-pod"] != "pod" || er.Extra["header:x-wwz-scenario"] != "s" || er.Extra["header:x-wwz-attempt"] != "2" {
		t.Fatalf("%+v", er.Extra)
	}
	// Reuse of a shared error: pre-set Extra and no breakdown clears the header.
	er2 := &core.Error{Extra: map[string]any{"header:" + inputBreakdownHeader: "stale"}}
	e.addDiagHeaders(r, er2)
	if _, ok := er2.Extra["header:"+inputBreakdownHeader]; ok {
		t.Fatal("stale breakdown must be dropped")
	}
}

func TestCoverageBalancesAndFailAndFinish(t *testing.T) {
	cfg, _ := config.Parse([]byte("keys:\n  - {key: k1, account: a1}\n"))
	e := New(cfg, limits.NewMemory(), nil, NewMetrics(newReg(), nil), nil)
	if k, a := e.balances(context.Background(), cfg, nil); k != nil || a != nil {
		t.Fatal("no key")
	}
	if k, a := e.balances(context.Background(), cfg, cfg.FindKey("k1")); k == nil && a == nil {
		t.Log("store has no state yet")
	}
	e.Log = discardLog()
	e.Store = failingGet{e.Store}
	if k, a := e.balances(context.Background(), cfg, cfg.FindKey("k1")); k != nil || a != nil {
		t.Fatal("store error yields nil balances")
	}

	// failAndFinish: gateway timeout delay and slow headers delay.
	drv := covDriver(t, "openai")
	e2 := New(cfg, limits.NewMemory(), nil, NewMetrics(newReg(), nil), discardLog())
	r := &request{in: Input{Driver: drv}, req: &core.Request{}, cfg: cfg, rng: generate.NewRNG(1), t0: time.Now(), rl: core.RateLimitState{}}
	r.errs.TimeoutDelay = time.Millisecond
	w := core.NewMemWriter()
	e2.failAndFinish(context.Background(), r, w, &core.Error{Kind: core.ErrGatewayTimeout})
	w = core.NewMemWriter()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	e2.failAndFinish(cancelled, r, w, &core.Error{Kind: core.ErrServerError, Transport: core.TransportSlowHeaders})
	w = core.NewMemWriter()
	e2.failAndFinish(context.Background(), r, w, &core.Error{Kind: core.ErrServerError, Transport: core.TransportSlowHeaders, TransportDelay: time.Millisecond})
}

func TestCoverageSubjectsAndFreeModels(t *testing.T) {
	cfg, err := config.Parse([]byte(`
keys:
  - {key: k1, account: a1, project: p1}
models:
  - id: "gpt-4o-mini:free"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(cfg, limits.NewMemory(), nil, NewMetrics(newReg(), nil), discardLog())
	key := cfg.FindKey("k1")
	oc := covDriver(t, "opencode")
	req := &core.Request{ClientIP: "1.2.3.4", Metadata: map[string]string{"user_id": "u9"}}
	subs := e.subjects(cfg, oc, key, req, &cfg.Models[0])
	if len(subs) == 0 {
		t.Fatal("subjects")
	}
	req2 := &core.Request{Metadata: map[string]string{"user": "u1"}}
	e.subjects(cfg, covDriver(t, "openai"), key, req2, &cfg.Models[0])

	or := covDriver(t, "openrouter")
	ctx := context.Background()
	if _, ok := e.freeModelsSubject(ctx, cfg, covDriver(t, "openai"), key, &cfg.Models[0]); ok {
		t.Fatal("openai has no free-model caps")
	}
	if _, ok := e.freeModelsSubject(ctx, cfg, or, nil, &cfg.Models[0]); ok {
		t.Fatal("no key")
	}
	if _, ok := e.freeModelsSubject(ctx, cfg, or, &config.KeyConfig{Key: "ghost"}, &cfg.Models[0]); ok {
		t.Log("key without account")
	}
	if sub, ok := e.freeModelsSubject(ctx, cfg, or, key, &cfg.Models[0]); !ok || sub.Entity.Kind != limits.KindFreeModels {
		t.Fatalf("free subject: %+v %v", sub, ok)
	}
}

func newReg() *metrics.Registry { return metrics.New(100) }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
