package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// flaky wraps a store and fails selected operations on demand. It exposes
// only the optional interfaces it declares itself (CreditAdder, TierSetter).
type flaky struct {
	limits.Store
	getFrom  int // fail Get from the Nth call on (1-based); 0 = never
	gets     int
	blockErr bool
	nilList  bool
}

func (f *flaky) Get(ctx context.Context, e limits.Entity) (limits.EntityState, error) {
	f.gets++
	if f.getFrom > 0 && f.gets >= f.getFrom {
		return limits.EntityState{}, errors.New("get boom")
	}
	return f.Store.Get(ctx, e)
}

func (f *flaky) Block(ctx context.Context, e limits.Entity, reason string, d time.Duration, by string) error {
	if f.blockErr {
		return errors.New("block boom")
	}
	return f.Store.Block(ctx, e, reason, d, by)
}

func (f *flaky) List(ctx context.Context, st limits.Status, n int) ([]limits.EntityState, error) {
	if f.nilList {
		return nil, nil
	}
	return f.Store.List(ctx, st, n)
}

func (f *flaky) AddCredits(context.Context, limits.Entity, float64) (limits.EntityState, error) {
	return limits.EntityState{}, errors.New("add boom")
}

func (f *flaky) SetTier(ctx context.Context, e limits.Entity, tier string) error {
	return f.Store.(limits.TierSetter).SetTier(ctx, e, tier)
}

type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("read boom") }

func serve(s *Server, req *http.Request) resp {
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return resp{w.Code, w.Header(), w.Body.Bytes()}
}

func TestNewDefaultsAndNilConfigToken(t *testing.T) {
	t.Setenv(TokenEnv, "")
	s, err := New(Options{Engine: newEngine(t, fastCfg)})
	if err != nil {
		t.Fatal(err)
	}
	if s.log == nil || s.TokenConfigured() {
		t.Fatalf("log %v token %v", s.log, s.TokenConfigured())
	}
	s.eng.SetConfig(nil)
	if s.effectiveToken() != "" {
		t.Fatal("nil config must mean no token")
	}
	if d := Document(nil); len(d) != 0 {
		t.Fatalf("Document(nil) = %v", d)
	}
}

func TestDocumentEmptyConfig(t *testing.T) {
	if d := Document(&config.Config{}); d == nil {
		t.Fatal("Document(empty) is nil")
	}
}

func TestDecodeJSONAndMediaType(t *testing.T) {
	var v map[string]any
	r := httptest.NewRequest("POST", "/", errBody{})
	if _, err := decodeJSON(r, &v); err == nil {
		t.Fatal("read error expected")
	}
	r = httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat(" ", maxBody+5)))
	if present, err := decodeJSON(r, &v); !present || err == nil {
		t.Fatalf("too large: %v %v", present, err)
	}
	r = httptest.NewRequest("POST", "/", strings.NewReader(`{} {}`))
	if _, err := decodeJSON(r, &v); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing: %v", err)
	}
	r = httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Content-Type", "Application/JSON; charset")
	if got := mediaType(r); got != "application/json; charset" {
		t.Fatalf("mediaType = %q", got)
	}
}

func TestConfigErrorPaths(t *testing.T) {
	boom := errors.New("check boom")
	s := newServer(t, fastCfg, func(o *Options) {
		o.Check = func(*config.Config) error { return boom }
	})
	if _, err := s.Patch([]byte(`{"server":{"seed":1}}`)); !errors.Is(err, boom) {
		t.Fatalf("Patch with failing Check: %v", err)
	}
	if err := s.Apply(s.eng.Config()); !errors.Is(err, boom) {
		t.Fatalf("Apply: %v", err)
	}
	if err := s.Apply(nil); err == nil {
		t.Fatal("nil config must fail")
	}

	s = newServer(t, fastCfg, func(o *Options) {
		o.Load = func() (*config.Config, error) { return nil, errors.New("load boom") }
	})
	if _, err := s.Reload(); err == nil {
		t.Fatal("Reload must fail when Load fails")
	}

	if _, err := s.Patch([]byte(`{} {}`)); err == nil {
		t.Fatal("trailing data must fail")
	}
	if _, err := s.Patch([]byte(`{`)); err == nil {
		t.Fatal("bad JSON must fail")
	}
	if _, err := s.Patch([]byte(`[]`)); err == nil {
		t.Fatal("non-object must fail")
	}

	r := serve(s, httptest.NewRequest("POST", "/admin/config", errBody{}))
	want(t, r, 400)
	r = serve(s, httptest.NewRequest("POST", "/admin/config", strings.NewReader(strings.Repeat(" ", maxBody+5))))
	want(t, r, 400)
}

func TestPatchNamedListEdgeCases(t *testing.T) {
	if _, err := patchNamedList("keys", "label", []any{"x"}, nil); err == nil {
		t.Fatal("non-object item must fail")
	}
	if _, err := patchNamedList("keys", "label", []any{map[string]any{}}, nil); err == nil {
		t.Fatal("item without id must fail")
	}
	out, err := patchNamedList("drivers", "mount", nil, map[string]any{"gone": nil, "plain": map[string]any{"driver": "openai"}})
	if err != nil || len(out) != 1 || out[0].(map[string]any)["name"] != "plain" {
		t.Fatalf("mount by name: %v %v", out, err)
	}
	got := mergePatch(map[string]any{"a": 1, "b": 2}, map[string]any{"a": nil}).(map[string]any)
	if _, ok := got["a"]; ok || got["b"] != 2 {
		t.Fatalf("mergePatch = %v", got)
	}
}

func TestJSONViaYAMLEdges(t *testing.T) {
	var out map[string]any
	if err := fromJSONViaYAML([]byte(`{`), &out); err == nil {
		t.Fatal("invalid JSON must fail")
	}
	if err := fromJSONViaYAML([]byte(`[1]`), &out); err == nil {
		t.Fatal("non-object must fail")
	}
	if !isZero(false) || isZero(true) || !isZero(uint64(0)) || !isZero(int64(0)) || !isZero(0) || !isZero(0.0) || isZero(struct{}{}) {
		t.Fatal("isZero")
	}
}

func TestScenarioPostErrors(t *testing.T) {
	s := newServer(t, fastCfg)
	req := httptest.NewRequest("POST", "/admin/scenarios", errBody{})
	req.Header.Set("Content-Type", "application/yaml")
	want(t, serve(s, req), 400)

	s.eng.Scenarios = nil
	r := do(t, s, "POST", "/admin/scenarios", "application/yaml", "name: x\n")
	want(t, r, 500)
	r = do(t, s, "GET", "/admin/scenarios", "", "")
	want(t, r, 200)
}

func TestEntityFromEmptyID(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.SetPathValue("tipo", "ip")
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	if _, ok := entityFrom(w, req); ok || w.Code != 400 {
		t.Fatalf("empty id: %v %d", ok, w.Code)
	}
}

func TestStoreFailurePaths(t *testing.T) {
	cfg := fastCfg + "tiers:\n  pro: {}\nkeys:\n  - {key: sk-a, label: a, account: acct-a}\n  - {key: sk-n, label: n}\n"
	s := newServer(t, cfg)
	mem := s.eng.Store
	s.eng.Config().Keys[1].Account = ""

	// List returning a nil slice still answers an empty array.
	s.eng.Store = &flaky{Store: mem, nilList: true}
	r := do(t, s, "GET", "/admin/entities", "", "")
	want(t, r, 200)
	if !strings.Contains(string(r.body), `"entities":[]`) {
		t.Fatalf("body %s", r.body)
	}

	// Credits: the store fails on AddCredits.
	s.eng.Store = &flaky{Store: mem}
	want(t, do(t, s, "POST", "/admin/entities/account/acct-a/credits", "application/json", `{"add_usd":1}`), 500)

	// Tier: a key without account, and a Get failure after SetTier.
	f := &flaky{Store: mem}
	s.eng.Store = f
	want(t, do(t, s, "POST", "/admin/entities/key/n/tier", "application/json", `{"tier":"pro"}`), 400)
	f.getFrom = 1
	want(t, do(t, s, "POST", "/admin/entities/account/acct-a/tier", "application/json", `{"tier":"pro"}`), 500)
	if keyByLabel(nil, "a") != nil {
		t.Fatal("nil config has no keys")
	}

	// Leak: Get after Ban, Block, Get after Block.
	s.eng.Store = &flaky{Store: mem, getFrom: 1}
	want(t, do(t, s, "POST", "/admin/leak", "application/json", `{"label":"a"}`), 500)
	s.eng.Store = &flaky{Store: mem, blockErr: true}
	want(t, do(t, s, "POST", "/admin/leak", "application/json", `{"label":"a"}`), 500)
	s.eng.Store = &flaky{Store: mem, getFrom: 2}
	want(t, do(t, s, "POST", "/admin/leak", "application/json", `{"label":"a"}`), 500)
}

func TestResetLimitsWithoutCounterResetter(t *testing.T) {
	s := newServer(t, fastCfg)
	s.eng.Store = failStore{Store: limits.NewMemory()}
	want(t, do(t, s, "POST", "/admin/reset", "application/json", `{"scope":["limits"]}`), 200)
}

func TestStatsParsingAndAggregation(t *testing.T) {
	s := newServer(t, fastCfg)
	s.eng.M = nil
	if s.scrape() != nil {
		t.Fatal("no metrics, no samples")
	}
	for _, line := range []string{
		`m{a} 1`, `m{a=1} 1`, `m{a="x} 1`, `m{a="x"`, `m{a="x"} nope`, `noval`,
	} {
		if _, ok := parseSample(line); ok {
			t.Errorf("%q must not parse", line)
		}
	}
	if sp, ok := parseSample(`m{a="x\"y",b="q"} 2`); !ok || sp.labels["a"] != `x"y` {
		t.Fatalf("escape: %+v %v", sp, ok)
	}

	smp := func(name string, v float64, kv ...string) sample {
		l := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			l[kv[i]] = kv[i+1]
		}
		return sample{name: name, labels: l, value: v}
	}
	tot, models := buildStats([]sample{
		smp("wwz_requests_total", 3, "status", "429", "driver", "openai", "model", "b"),
		smp("wwz_requests_total", 1, "status", "200", "driver", "openai", "model", "a"),
		smp("wwz_requests_total", 1, "status", "200", "driver", "anthropic", "model", "z"),
		smp("wwz_streams_open", 2),
		smp("wwz_errors_injected_total", 4, "kind", "k", "phase", "p"),
		smp("wwz_rate_limited_total", 5, "limit", "rpm"),
		smp("wwz_blocked_total", 6),
		smp("wwz_ttft_seconds_bucket", 1, "le", "bad", "driver", "openai", "model", "a"),
	})
	if tot.StreamsOpen != 2 || tot.ErrorsInjected != 4 || tot.ErrorsByKind["k"] != 4 || tot.RateLimited != 5 ||
		tot.RateLimitedByLimit["rpm"] != 5 || tot.Blocked != 6 {
		t.Fatalf("totals %+v", tot)
	}
	if len(models) != 3 || models[0].Driver != "anthropic" || models[1].Model != "a" || models[2].Model != "b" || models[2].Errors != 3 {
		t.Fatalf("models %+v", models)
	}
}

func TestStatsByKeySorted(t *testing.T) {
	s := newServer(t, fastCfg+"keys:\n  - {key: sk-b, label: b}\n  - {key: sk-a, label: a}\n")
	mem := s.eng.Store
	for _, id := range []string{"b", "a"} {
		if err := mem.Block(context.Background(), limits.Entity{Kind: limits.KindKey, ID: id}, "x", 0, "t"); err != nil {
			t.Fatal(err)
		}
	}
	r := do(t, s, "GET", "/admin/stats", "", "")
	want(t, r, 200)
	var st Stats
	r.json(t, &st)
	if len(st.ByKey) != 2 || st.ByKey[0].Key != "a" {
		t.Fatalf("by_key %+v", st.ByKey)
	}
}

func TestMustYAMLPanics(t *testing.T) {
	for name, fn := range map[string]func(){
		"marshal":   func() { mustMarshalYAML(make(chan int)) },
		"unmarshal": func() { var m map[string]any; mustUnmarshalYAML([]byte("a: [1"), &m) },
	} {
		func() {
			defer func() {
				if r, _ := recover().(string); !strings.HasPrefix(r, "admin: yaml.") {
					t.Fatalf("%s: panic = %q", name, r)
				}
			}()
			fn()
		}()
	}
}
