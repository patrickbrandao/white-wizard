package limits_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/limits/storetest"
)

// valkeyTarget is the test Valkey exported by scripts/verify.sh:
//   - WWZ_TEST_VALKEY=local: WWZ_TEST_VALKEY_ADDR (one address) and
//     WWZ_TEST_VALKEY_DB (default 15, never 0: tests FLUSHDB);
//   - WWZ_TEST_VALKEY=cluster: WWZ_TEST_VALKEY_ADDR with the masters
//     separated by commas, database 0, store.cluster: true and FLUSHALL on
//     every master before each test (spec 018, verify-heavy round cluster).
type valkeyTarget struct {
	addrs   []string
	db      int
	cluster bool
}

// valkeyTargetEnv reads the test Valkey from the environment or skips.
func valkeyTargetEnv(t testing.TB) valkeyTarget {
	t.Helper()
	mode := os.Getenv("WWZ_TEST_VALKEY")
	if mode == "" {
		t.Skip("Valkey de teste ausente: suba com `docker compose up -d valkey` e rode `make verify-quick` (ou exporte WWZ_TEST_VALKEY=local WWZ_TEST_VALKEY_ADDR=127.0.0.1:16379)")
	}
	addr := os.Getenv("WWZ_TEST_VALKEY_ADDR")
	if mode == "cluster" {
		var addrs []string
		for a := range strings.SplitSeq(addr, ",") {
			if a = strings.TrimSpace(a); a != "" {
				addrs = append(addrs, a)
			}
		}
		if len(addrs) == 0 {
			t.Fatal("WWZ_TEST_VALKEY=cluster needs WWZ_TEST_VALKEY_ADDR with the masters separated by commas")
		}
		return valkeyTarget{addrs: addrs, cluster: true}
	}
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	db := 15
	if s := os.Getenv("WWZ_TEST_VALKEY_DB"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("WWZ_TEST_VALKEY_DB=%q: %v", s, err)
		}
		db = n
	}
	if db == 0 {
		t.Fatal("WWZ_TEST_VALKEY_DB=0: tests never use database 0 (they FLUSHDB)")
	}
	return valkeyTarget{addrs: []string{addr}, db: db}
}

// config is the store configuration for the target.
func (tg valkeyTarget) config() config.StoreConfig {
	cfg := testStoreConfig(tg.addrs[0])
	cfg.Addr = tg.addrs
	cfg.Cluster = tg.cluster
	return cfg
}

// valkeyEnv returns the single standalone address and database, or skips:
// tests that put a TCP proxy in front of one address do not run against a
// cluster, whose clients follow the announced node addresses.
func valkeyEnv(t testing.TB) (string, int) {
	t.Helper()
	tg := valkeyTargetEnv(t)
	if tg.cluster {
		t.Skip("precisa de um Valkey standalone (proxy na frente de um endereço); coberto por WWZ_TEST_VALKEY=local")
	}
	return tg.addrs[0], tg.db
}

func testStoreConfig(addr string) config.StoreConfig {
	return config.StoreConfig{
		Kind:        "valkey",
		Addr:        []string{addr},
		ReadTimeout: 5 * time.Second, // generous: -race and 200 parallel callers
		OnDown:      "local",
		Prefix:      "wwz",
		EventStream: true,
		EventMaxLen: 1_000_000,
		EventSample: 1,
	}
}

type valkeyTweak func(*config.StoreConfig, *limits.ValkeyOptions)

// openValkey connects to the test database without flushing it.
func openValkey(t testing.TB, tweaks ...valkeyTweak) *limits.Valkey {
	t.Helper()
	tg := valkeyTargetEnv(t)
	cfg := tg.config()
	o := limits.ValkeyOptions{DB: tg.db, Instance: "test-1", Version: "test", Heartbeat: -1}
	for _, tw := range tweaks {
		tw(&cfg, &o)
	}
	v, err := limits.NewValkey(context.Background(), cfg, o)
	if err != nil {
		t.Fatalf("NewValkey: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

// masters returns the clients of the primary nodes (the only node in
// standalone).
func masters(t testing.TB, v *limits.Valkey) []valkey.Client {
	t.Helper()
	ctx := context.Background()
	var out []valkey.Client
	for addr, n := range v.Client().Nodes() {
		role, err := n.Do(ctx, n.B().Arbitrary("ROLE").Build()).ToArray()
		if err != nil || len(role) == 0 {
			t.Fatalf("ROLE on %s: %v", addr, err)
		}
		if r, _ := role[0].ToString(); r == "master" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		t.Fatal("no primary node")
	}
	return out
}

// onMasters runs a keyless command on every primary node.
func onMasters(t testing.TB, v *limits.Valkey, cmd ...string) {
	t.Helper()
	for _, n := range masters(t, v) {
		if err := n.Do(context.Background(), n.B().Arbitrary(cmd...).Build()).Error(); err != nil {
			t.Fatalf("%v: %v", cmd, err)
		}
	}
}

// keysMatching runs KEYS on every primary node.
func keysMatching(t testing.TB, v *limits.Valkey, pattern string) []string {
	t.Helper()
	var out []string
	for _, n := range masters(t, v) {
		ks, err := n.Do(context.Background(), n.B().Arbitrary("KEYS").Args(pattern).Build()).AsStrSlice()
		if err != nil {
			t.Fatalf("KEYS %s: %v", pattern, err)
		}
		out = append(out, ks...)
	}
	return out
}

// flush empties the test database (FLUSHDB) or, in cluster, every master
// (FLUSHALL on database 0).
func flush(t testing.TB, v *limits.Valkey) {
	t.Helper()
	if valkeyTargetEnv(t).cluster {
		onMasters(t, v, "FLUSHALL")
		return
	}
	c := v.Client()
	if err := c.Do(context.Background(), c.B().Arbitrary("FLUSHDB").Build()).Error(); err != nil {
		t.Fatalf("FLUSHDB: %v", err)
	}
}

// newValkeyStore is the storetest factory: a clean database per subtest.
func newValkeyStore(t testing.TB, tweaks ...valkeyTweak) *limits.Valkey {
	t.Helper()
	v := openValkey(t, tweaks...)
	flush(t, v)
	return v
}

func TestValkeyStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) limits.Store { return newValkeyStore(t) })
}

// TestValkeyStoreContractPerEntity runs the suite through check_entity and
// release, the store.cluster path, on the standalone server.
func TestValkeyStoreContractPerEntity(t *testing.T) {
	storetest.Run(t, func(t *testing.T) limits.Store {
		return newValkeyStore(t, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.PerEntity = true })
	})
}

// TestValkeyStoreContractLocalFirst runs the suite with local rps/rpm
// buckets. The sync is slowed down so the suite's injected clock does not
// mix with the wall clock of the sync (covered by TestValkeyLocalFirstSync).
func TestValkeyStoreContractLocalFirst(t *testing.T) {
	storetest.Run(t, func(t *testing.T) limits.Store {
		return newValkeyStore(t, func(c *config.StoreConfig, o *limits.ValkeyOptions) {
			c.LocalFirst = true
			o.SyncInterval = time.Hour
		})
	})
}

// TestValkeyStoreContractFallback runs the suite through the on_down circuit
// with Valkey up: the wrapper must be transparent.
func TestValkeyStoreContractFallback(t *testing.T) {
	tg := valkeyTargetEnv(t)
	storetest.Run(t, func(t *testing.T) limits.Store {
		cfg := tg.config()
		f := limits.OpenValkey(context.Background(), cfg, limits.ValkeyOptions{DB: tg.db, Heartbeat: -1}, limits.FallbackOptions{})
		t.Cleanup(func() { _ = f.Close() })
		if f.CircuitOpen() {
			t.Fatal("circuit open with Valkey up")
		}
		flush(t, f.Valkey())
		return f
	})
}

var (
	keyA  = limits.Entity{Kind: limits.KindKey, ID: "key-a"}
	keyB  = limits.Entity{Kind: limits.KindKey, ID: "key-b"}
	acctA = limits.Entity{Kind: limits.KindAccount, ID: "acct-a"}
)

func mustCheck(t *testing.T, s limits.Store, req limits.CheckRequest) limits.Decision {
	t.Helper()
	d, err := s.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return d
}

// Two stores on the same database share the limit (two wwz instances).
func TestValkeySharedAcrossInstances(t *testing.T) {
	a := newValkeyStore(t)
	b := openValkey(t, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.Instance = "test-2" })
	now := time.Now()
	req := limits.CheckRequest{Subjects: []limits.Subject{{Entity: keyA, Limits: config.Limits{RPM: 10}}}, Now: now}
	allowed := 0
	for i := range 30 {
		s := limits.Store(a)
		if i%2 == 1 {
			s = b
		}
		if mustCheck(t, s, req).Allowed {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("two instances allowed %d, want the shared 10", allowed)
	}
	// A ban by one instance applies on the other.
	if err := a.Ban(context.Background(), keyB, "leak", "test"); err != nil {
		t.Fatal(err)
	}
	d := mustCheck(t, b, limits.CheckRequest{Subjects: []limits.Subject{{Entity: keyB}}, Now: now})
	if d.Allowed || d.Kind != core.ErrAccountBanned || d.Message != "leak" {
		t.Fatalf("ban seen by the other instance: %+v", d)
	}
}

// In the cluster path a later denial releases what earlier entities took.
func TestValkeyPerEntityRelease(t *testing.T) {
	s := newValkeyStore(t, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.PerEntity = true })
	now := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	key := config.Limits{RPS: 2, RPM: 2, TPM: 1000, ITPM: 1000, RPD: 2, TPD: 1000, Concurrency: 2, Ramp: &config.Ramp{ThresholdTPM: 1_000_000, MaxIncreasePct: 50}}
	keyFixed := config.Limits{RPM: 2, TPM: 1000, Algorithm: "fixed_window"}
	acct := config.Limits{RPM: 1}
	both := limits.CheckRequest{Subjects: []limits.Subject{{Entity: keyA, Limits: key}, {Entity: keyB, Limits: keyFixed}, {Entity: acctA, Limits: acct}}, EstPromptTokens: 400, Now: now}
	if d := mustCheck(t, s, both); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	for range 3 {
		d := mustCheck(t, s, both)
		if d.Allowed || d.Entity != acctA || d.LimitName != "rpm" {
			t.Fatalf("account must deny: %+v", d)
		}
	}
	// Each key has exactly one request of its limits left.
	for _, sub := range []limits.Subject{{Entity: keyA, Limits: key}, {Entity: keyB, Limits: keyFixed}} {
		one := limits.CheckRequest{Subjects: []limits.Subject{sub}, EstPromptTokens: 400, Now: now}
		if d := mustCheck(t, s, one); !d.Allowed {
			t.Fatalf("%v: released capacity missing: %+v", sub.Entity, d)
		}
		if d := mustCheck(t, s, one); d.Allowed {
			t.Fatalf("%v: more than the limit after release", sub.Entity)
		}
	}
	if st, err := s.Get(context.Background(), keyA); err != nil || st.Requests != 2 {
		t.Fatalf("key requests = %d (%v), want 2 (released ones do not count)", st.Requests, err)
	}
}

// eventFields are the properties of the asyncapi Event schema
// (additionalProperties: false).
var eventFields = map[string]bool{"t": true, "type": true, "instance": true, "request_id": true, "test_id": true, "driver": true, "endpoint": true, "model": true, "entity": true, "key": true, "account": true, "ip": true, "status": true, "error": true, "sample": true, "fields": true}

// eventFieldProps are the known properties of fields in the asyncapi Event
// schema (0.5.0: billing_source and allowance_usd, ADR 0072). The schema
// admits others; the store writes only these.
var eventFieldProps = map[string]bool{"duration_ms": true, "prompt_tokens": true, "output_tokens": true, "reasoning_tokens": true, "cached_tokens": true, "cost_usd": true, "billing_source": true, "allowance_usd": true, "tool_calls": true, "input_breakdown": true, "limit": true, "denied": true, "reason": true, "until": true, "by": true, "strikes": true}

// rawEvents decodes ev:stream entries (field d) as generic JSON.
func rawEvents(t *testing.T, v *limits.Valkey) []map[string]any {
	t.Helper()
	c := v.Client()
	xs, err := c.Do(context.Background(), c.B().Arbitrary("XRANGE").Keys("wwz:ev:stream").Args("-", "+").Build()).AsXRange()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		if len(x.FieldValues) != 1 {
			t.Fatalf("entry %s has fields %v, want only d", x.ID, x.FieldValues)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(x.FieldValues["d"]), &m); err != nil {
			t.Fatalf("entry %s: %v", x.ID, err)
		}
		out = append(out, m)
	}
	return out
}

func TestValkeyEventStreamMatchesAsyncAPI(t *testing.T) {
	s := newValkeyStore(t)
	ctx := context.Background()
	now := time.Now()
	sub := []limits.Subject{{Entity: keyA}}
	quota := []limits.Subject{{Entity: keyA, Limits: config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10}}}}
	if err := s.Finish(ctx, limits.FinishRequest{Subjects: quota, RequestID: "req-1", TestID: "carga", Now: now, Status: 200, Model: "m", Driver: "openai", Endpoint: "chat", DurationMS: 12, ActualPrompt: 3, ActualOutput: 4, Account: "org", CostMicroUSD: 1500, QuotaSet: true, QuotaMicroUSD: 900, BillingSource: "go-plus", ToolCalls: []string{"a", "a"}, InputBreakdown: &limits.InputBreakdown{System: 1, User: 2, Total: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, limits.FinishRequest{Subjects: sub, RequestID: "req-2", Now: now, Status: 429, ErrorKind: core.ErrRateLimit, Endpoint: "responses"}); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Block(ctx, keyA, "r", time.Minute, "abuse"))
	must(s.Unblock(ctx, keyA))
	must(s.Ban(ctx, keyB, "r", "admin"))
	must(s.Unban(ctx, keyB))
	_, err := s.Strike(ctx, acctA, "r", time.Hour)
	must(err)
	evs := rawEvents(t, s)
	want := []string{"request", "error", "block", "unblock", "ban", "unban", "strike"}
	if len(evs) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(evs), len(want), evs)
	}
	kinds := map[string]bool{"ip": true, "key": true, "user": true, "project": true, "account": true}
	for i, ev := range evs {
		for k := range ev {
			if !eventFields[k] {
				t.Errorf("event %d: property %q not in the asyncapi schema", i, k)
			}
		}
		f, _ := ev["fields"].(map[string]any)
		for k := range f {
			if !eventFieldProps[k] {
				t.Errorf("event %d: fields.%s not in the asyncapi schema", i, k)
			}
		}
		for _, k := range []string{"t", "type", "instance"} {
			if _, ok := ev[k]; !ok {
				t.Errorf("event %d: required %q missing: %v", i, k, ev)
			}
		}
		if ev["type"] != want[i] {
			t.Errorf("event %d type = %v, want %s", i, ev["type"], want[i])
		}
		if ev["instance"] != "test-1" {
			t.Errorf("event %d instance = %v", i, ev["instance"])
		}
		if ts, _ := ev["t"].(string); ts == "" {
			t.Errorf("event %d: t not a string", i)
		} else if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
			t.Errorf("event %d: t %q not date-time: %v", i, ts, err)
		}
		ent, ok := ev["entity"].(map[string]any)
		if !ok || !kinds[fmt.Sprint(ent["kind"])] || ent["id"] == "" {
			t.Errorf("event %d entity = %v", i, ev["entity"])
		}
	}
	if evs[0]["request_id"] != "req-1" || evs[0]["test_id"] != "carga" || evs[0]["status"] != float64(200) || evs[0]["driver"] != "openai" {
		t.Errorf("request event: %v", evs[0])
	}
	f, _ := evs[0]["fields"].(map[string]any)
	if f["duration_ms"] != float64(12) || f["prompt_tokens"] != float64(3) || f["output_tokens"] != float64(4) || f["cost_usd"] != 0.0015 || fmt.Sprint(f["tool_calls"]) != "[a a]" || evs[0]["account"] != "org" {
		t.Errorf("request fields: %v", f)
	}
	if f["billing_source"] != "go-plus" || f["allowance_usd"] != 0.0009 {
		t.Errorf("billing fields: %v", f)
	}
	// input_breakdown is the asyncapi InputBreakdown object: the nine keys of
	// x-wwz-input-breakdown, all integers (ADR 0032).
	ib, _ := f["input_breakdown"].(map[string]any)
	wantIB := map[string]float64{"system": 1, "tool_schemas": 0, "user": 2, "assistant": 0, "tool_calls": 0, "tool_results": 0, "images": 0, "cached": 0, "total": 3}
	if len(ib) != len(wantIB) {
		t.Errorf("input_breakdown = %v, want keys %v", ib, wantIB)
	}
	for k, v := range wantIB {
		if ib[k] != v {
			t.Errorf("input_breakdown.%s = %v, want %v", k, ib[k], v)
		}
	}
	if evs[0]["endpoint"] != "chat" || evs[1]["endpoint"] != "responses" || evs[2]["endpoint"] != nil {
		t.Errorf("endpoint: request %v, error %v, block %v", evs[0]["endpoint"], evs[1]["endpoint"], evs[2]["endpoint"])
	}
	if evs[1]["error"] != "rate_limit" {
		t.Errorf("error event: %v", evs[1])
	}
	// An error that is not a denial carries cost_usd 0 and no input_breakdown.
	if f, _ := evs[1]["fields"].(map[string]any); f["cost_usd"] != 0.0 || f["input_breakdown"] != nil {
		t.Errorf("error event fields: %v", f)
	}
	if f, _ := evs[2]["fields"].(map[string]any); f["reason"] != "r" || f["by"] != "abuse" || f["until"] == nil {
		t.Errorf("block fields: %v", evs[2])
	}
	if f, _ := evs[6]["fields"].(map[string]any); f["strikes"] != float64(1) {
		t.Errorf("strike fields: %v", evs[6])
	}
	// Store.Events decodes the same entries, newest first.
	got, err := s.Events(ctx, 2)
	must(err)
	if len(got) != 2 || got[0].Type != "strike" || got[1].Type != "unban" || got[0].Instance != "test-1" {
		t.Fatalf("Events(2) = %+v", got)
	}
}

func TestValkeyEventSampleAndMaxLen(t *testing.T) {
	s := newValkeyStore(t, func(c *config.StoreConfig, _ *limits.ValkeyOptions) {
		c.EventSample = 0.5
		c.EventMaxLen = 50
	})
	ctx := context.Background()
	for range 600 {
		if err := s.Finish(ctx, limits.FinishRequest{Now: time.Now(), Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Ban(ctx, keyA, "r", "x"); err != nil {
		t.Fatal(err)
	}
	evs := rawEvents(t, s)
	if len(evs) >= 300 {
		t.Fatalf("stream has %d entries: MAXLEN ~ 50 not applied", len(evs))
	}
	last := evs[len(evs)-1]
	if last["type"] != "ban" || last["sample"] != nil {
		t.Fatalf("ban must never be sampled: %v", last)
	}
	for _, ev := range evs[:len(evs)-1] {
		if ev["sample"] != 0.5 {
			t.Fatalf("sampled request event without sample=0.5: %v", ev)
		}
	}
	// Count what was sent: XLEN is trimmed, so count with a fresh large stream.
	s2 := newValkeyStore(t, func(c *config.StoreConfig, _ *limits.ValkeyOptions) { c.EventSample = 0.5 })
	for range 400 {
		if err := s2.Finish(ctx, limits.FinishRequest{Now: time.Now(), Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(rawEvents(t, s2)); n < 120 || n > 280 {
		t.Fatalf("event_sample 0.5 kept %d of 400", n)
	}
}

func TestValkeyEventStreamOff(t *testing.T) {
	s := newValkeyStore(t, func(c *config.StoreConfig, _ *limits.ValkeyOptions) { c.EventStream = false })
	ctx := context.Background()
	if err := s.Finish(ctx, limits.FinishRequest{Now: time.Now(), Status: 200}); err != nil {
		t.Fatal(err)
	}
	c := s.Client()
	if n, err := c.Do(ctx, c.B().Arbitrary("EXISTS").Keys("wwz:ev:stream").Build()).AsInt64(); err != nil || n != 0 {
		t.Fatalf("stream written with event_stream false: %d %v", n, err)
	}
	evs, err := s.Events(ctx, 0)
	if err != nil || len(evs) != 1 || evs[0].Status != 200 {
		t.Fatalf("local ring events = %+v, %v", evs, err)
	}
	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if evs, _ := s.Events(ctx, 0); len(evs) != 0 {
		t.Fatalf("ring survived Reset: %+v", evs)
	}
}

// Keys follow cache/valkey/README.md: prefix, hash tag and escaped id.
func TestValkeyKeySchema(t *testing.T) {
	s := newValkeyStore(t, func(c *config.StoreConfig, _ *limits.ValkeyOptions) { c.Prefix = "wwzt" })
	ctx := context.Background()
	e := limits.Entity{Kind: limits.KindKey, ID: "a:b{c} d%"}
	ip := limits.Entity{Kind: limits.KindIP, ID: "::1"}
	req := limits.CheckRequest{Subjects: []limits.Subject{{Entity: ip, Limits: config.Limits{RPM: 5}}, {Entity: e, Limits: config.Limits{RPM: 5, Concurrency: 1}}}, Now: time.Now()}
	if d := mustCheck(t, s, req); !d.Allowed {
		t.Fatal(d)
	}
	c := s.Client()
	for _, k := range []string{"wwzt:ent:{key:a%3Ab%7Bc%7D%20d%25}", "wwzt:tb:{key:a%3Ab%7Bc%7D%20d%25}", "wwzt:conc:{key:a%3Ab%7Bc%7D%20d%25}", "wwzt:ent:{ip:%3A%3A1}"} {
		if n, err := c.Do(ctx, c.B().Arbitrary("EXISTS").Keys(k).Build()).AsInt64(); err != nil || n != 1 {
			t.Errorf("key %s missing (%d, %v)", k, n, err)
		}
	}
	h, err := c.Do(ctx, c.B().Arbitrary("HGETALL").Keys("wwzt:ent:{key:a%3Ab%7Bc%7D%20d%25}").Build()).AsStrMap()
	if err != nil || h["id"] != e.ID || h["kind"] != "key" || h["requests"] != "1" {
		t.Fatalf("ent hash = %v, %v", h, err)
	}
	ttl, _ := c.Do(ctx, c.B().Arbitrary("PTTL").Keys("wwzt:ent:{key:a%3Ab%7Bc%7D%20d%25}").Build()).AsInt64()
	if ttl < 29*24*3600*1000 || ttl > 30*24*3600*1000 {
		t.Fatalf("ent TTL = %d ms, want 30 d", ttl)
	}
	members, err := c.Do(ctx, c.B().Arbitrary("ZRANGE").Keys("wwzt:idx:ent").Args("0", "-1").Build()).AsStrSlice()
	if err != nil || len(members) != 2 {
		t.Fatalf("idx:ent = %v, %v", members, err)
	}
	// Long ids are hashed.
	long := limits.Entity{Kind: limits.KindUser, ID: string(make([]byte, 300))}
	if _, err := s.Strike(ctx, long, "r", time.Minute); err != nil {
		t.Fatal(err)
	}
	keys := keysMatching(t, s, "wwzt:strikes:{user:~*")
	if len(keys) != 1 || len(keys[0]) != len("wwzt:strikes:{user:~}")+32 {
		t.Fatalf("hashed long id keys = %v", keys)
	}
	st, err := s.Get(ctx, long)
	if err != nil || st.Strikes != 1 {
		t.Fatalf("Get(long) = %+v, %v", st, err)
	}
}

// After SCRIPT FLUSH the store reloads the scripts transparently.
func TestValkeyNoScriptRecovery(t *testing.T) {
	s := newValkeyStore(t)
	onMasters(t, s, "SCRIPT", "FLUSH")
	if d := mustCheck(t, s, limits.CheckRequest{Subjects: []limits.Subject{{Entity: keyA, Limits: config.Limits{RPM: 1}}}, Now: time.Now()}); !d.Allowed {
		t.Fatalf("after SCRIPT FLUSH: %+v", d)
	}
}

// List drops index members whose entity vanished or lost its mark.
func TestValkeyListRepairsIndex(t *testing.T) {
	s := newValkeyStore(t)
	ctx := context.Background()
	if err := s.Ban(ctx, keyA, "r", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Ban(ctx, keyB, "r", "x"); err != nil {
		t.Fatal(err)
	}
	c := s.Client()
	_ = c.Do(ctx, c.B().Arbitrary("DEL").Keys("wwz:ent:{key:key-a}").Build())
	got, err := s.List(ctx, limits.StatusBanned, 0)
	if err != nil || len(got) != 1 || got[0].Entity != keyB {
		t.Fatalf("List(banned) = %+v, %v", got, err)
	}
	if n, _ := c.Do(ctx, c.B().Arbitrary("ZCARD").Keys("wwz:idx:status:banned").Build()).AsInt64(); n != 1 {
		t.Fatalf("stale member kept in idx:status:banned (%d)", n)
	}
	// Negative balance: listed as limited.
	if err := s.SetCredits(ctx, acctA, -1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.List(ctx, limits.StatusLimited, 0); len(got) != 1 || got[0].Entity != acctA {
		t.Fatalf("List(limited) = %+v", got)
	}
}

// local_first: two instances share an rpm limit through sync_local.
func TestValkeyLocalFirstSync(t *testing.T) {
	tw := func(id string) valkeyTweak {
		return func(c *config.StoreConfig, o *limits.ValkeyOptions) {
			c.LocalFirst = true
			o.SyncInterval = 20 * time.Millisecond
			o.Heartbeat = 50 * time.Millisecond
			o.Instance = id
		}
	}
	a := newValkeyStore(t, tw("lf-a"))
	b := openValkey(t, tw("lf-b"))
	time.Sleep(150 * time.Millisecond) // both heartbeats seen: N = 2
	sub := []limits.Subject{{Entity: keyA, Limits: config.Limits{RPM: 20}}}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(400 * time.Millisecond)
	for _, s := range []limits.Store{a, b} {
		wg.Go(func() {
			for time.Now().Before(deadline) {
				d, err := s.Check(context.Background(), limits.CheckRequest{Subjects: sub, Now: time.Now()})
				if err != nil {
					t.Error(err)
					return
				}
				if d.Allowed {
					allowed.Add(1)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
	wg.Wait()
	// 20 shared plus the refill of 400 ms (20/60 per second) and at most one
	// sync interval of overshoot per instance.
	if n := allowed.Load(); n < 18 || n > 24 {
		t.Fatalf("local_first allowed %d across 2 instances, want ~20", n)
	}
	time.Sleep(60 * time.Millisecond)
	st, err := a.Get(context.Background(), keyA)
	if err != nil || st.Requests != allowed.Load() {
		t.Fatalf("requests synced = %d (%v), want %d", st.Requests, err, allowed.Load())
	}
	// A ban made elsewhere reaches the local path within a sync.
	if err := b.Ban(context.Background(), keyA, "r", "x"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	_ = mustCheck(t, a, limits.CheckRequest{Subjects: sub, Now: time.Now()}) // touches the entity
	time.Sleep(100 * time.Millisecond)
	if d := mustCheck(t, a, limits.CheckRequest{Subjects: sub, Now: time.Now()}); d.Allowed || d.Kind != core.ErrAccountBanned {
		t.Fatalf("ban not seen by the local path: %+v", d)
	}
}

func BenchmarkValkeyCheck(b *testing.B) {
	for _, bc := range []struct {
		name  string
		tweak valkeyTweak
	}{
		{"standalone", nil},
		{"per_entity", func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.PerEntity = true }},
		{"local_first", func(c *config.StoreConfig, _ *limits.ValkeyOptions) { c.LocalFirst = true }},
	} {
		b.Run(bc.name, func(b *testing.B) {
			var tw []valkeyTweak
			if bc.tweak != nil {
				tw = append(tw, bc.tweak)
			}
			s := newValkeyStore(b, tw...)
			subs := []limits.Subject{
				{Entity: limits.Entity{Kind: limits.KindIP, ID: "10.0.0.1"}},
				{Entity: keyA, Limits: config.Limits{RPM: 1 << 30}},
				{Entity: acctA, Limits: config.Limits{RPS: 1 << 30}},
			}
			b.ReportAllocs()
			b.SetParallelism(16) // many callers in flight, as under load
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := s.Check(context.Background(), limits.CheckRequest{Subjects: subs, Now: time.Now()}); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

// polRec records what a PolicySource watcher receives.
type polRec struct {
	mu   sync.Mutex
	got  []limits.Policies
	errs []error
}

func (r *polRec) add(p limits.Policies, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, p)
	r.errs = append(r.errs, err)
}

// find returns the last delivery of version v.
func (r *polRec) find(v int64) (limits.Policies, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.got) - 1; i >= 0; i-- {
		if r.got[i].Version == v {
			return r.got[i], true, r.errs[i]
		}
	}
	return limits.Policies{}, false, nil
}

func (r *polRec) has(v int64) bool {
	_, ok, _ := r.find(v)
	return ok
}

func (r *polRec) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// panelWrite changes cfg:policies like the panel (MULTI + HINCRBY) and
// publishes cfg:changed unless quiet.
func panelWrite(t *testing.T, v *limits.Valkey, quiet bool, field, value string) int64 {
	t.Helper()
	c := v.Client()
	ctx := context.Background()
	res := c.DoMulti(ctx,
		c.B().Multi().Build(),
		c.B().Hset().Key("wwz:cfg:policies").FieldValue().FieldValue(field, value).Build(),
		c.B().Hincrby().Key("wwz:cfg:policies").Field("version").Increment(1).Build(),
		c.B().Exec().Build(),
	)
	arr, err := res[3].ToArray()
	if err != nil || len(arr) != 2 {
		t.Fatalf("EXEC: %v %v", arr, err)
	}
	n, err := arr[1].AsInt64()
	if err != nil {
		t.Fatal(err)
	}
	if !quiet {
		if err := c.Do(ctx, c.B().Publish().Channel("wwz:cfg:changed").Message(fmt.Sprintf(`{"version": %d}`, n)).Build()).Error(); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// TestValkeyPoliciesAcrossInstances: a policy written by the panel (or by
// SetTier on another instance) reaches every instance through cfg:changed,
// and the subscription re-reads the hash after a reconnection (ADR 0025).
func TestValkeyPoliciesAcrossInstances(t *testing.T) {
	a := newValkeyStore(t)
	b := openValkey(t, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.Instance = "test-2" })
	rec := &polRec{}
	b.WatchPolicies(rec.add)
	waitFor(t, "first delivery on subscribe", func() bool { return rec.has(0) })

	v := panelWrite(t, a, false, "tier:pro", `{"rpm": 600, "queue_timeout": "2s"}`)
	waitFor(t, "panel version on the other instance", func() bool { return rec.has(v) })
	if p, _, err := rec.find(v); err != nil || p.Tiers["pro"].RPM != 600 || p.Tiers["pro"].QueueTimeout != 2*time.Second {
		t.Fatalf("panel policies: %+v %v", p, err)
	}

	ctx := context.Background()
	if err := a.SetTier(ctx, keyA, "pro"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "SetTier version on the other instance", func() bool { return rec.has(v + 1) })
	if p, _, _ := rec.find(v + 1); p.Entities[keyA] != "pro" || p.Tiers["pro"].RPM != 600 {
		t.Fatalf("SetTier policies: %+v", p)
	}
	st, err := b.Get(ctx, keyA)
	if err != nil || st.Tier != "pro" {
		t.Fatalf("tier on ent: %+v %v", st, err)
	}

	bad := panelWrite(t, a, false, "tier:bad", "{broken")
	waitFor(t, "invalid version", func() bool { return rec.has(bad) })
	if _, _, err := rec.find(bad); err == nil {
		t.Fatal("invalid policies delivered without error")
	}

	// A change nobody announced (no PUBLISH) is read on re-subscribe. It is
	// written before the subscription goes down: valkey-go re-subscribes
	// within milliseconds, so a write after CLIENT KILL races with the
	// re-read (in cluster the kill visits every master first; task 410).
	// The only path to b is the re-read: a's SetTier delivery is local.
	quiet := panelWrite(t, a, true, "tier:gold", `{"rpm": 1}`)
	onMasters(t, a, "CLIENT", "KILL", "TYPE", "pubsub")
	waitFor(t, "re-read after re-subscription", func() bool { return rec.has(quiet) })

	// Removing the tier deletes the field and still bumps the version.
	if err := a.SetTier(ctx, keyA, ""); err != nil {
		t.Fatal(err)
	}
	p, err := a.Policies(ctx)
	if p.Version != quiet+1 || len(p.Entities) != 0 || err == nil {
		t.Fatalf("after removal: %+v %v (tier:bad is still invalid)", p, err)
	}
}

func TestValkeyPoliciesErrors(t *testing.T) {
	v := newValkeyStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Policies(ctx); err == nil {
		t.Fatal("Policies with a canceled context")
	}
	if err := v.SetTier(ctx, keyA, "pro"); err == nil {
		t.Fatal("SetTier with a canceled context")
	}
	if err := v.ResetEntity(ctx, keyA, true, true); err == nil {
		t.Fatal("ResetEntity with a canceled context")
	}
	// The display write fails (ent is not a hash) after the policy write.
	c := v.Client()
	bg := context.Background()
	if err := c.Do(bg, c.B().Set().Key("wwz:ent:{key:key-a}").Value("x").Build()).Error(); err != nil {
		t.Fatal(err)
	}
	// Another instance still gets cfg:changed: PUBLISH goes out even when
	// the display write fails (and on its own, outside the keyed pipeline).
	other := openValkey(t, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.Instance = "test-2" })
	orec := &polRec{}
	other.WatchPolicies(orec.add)
	waitFor(t, "first delivery on subscribe", func() bool { return orec.has(0) })
	if err := v.SetTier(bg, keyA, "pro"); err == nil {
		t.Fatal("SetTier over a non-hash ent")
	}
	if p, err := v.Policies(bg); err != nil || p.Version != 1 || p.Entities[keyA] != "pro" {
		t.Fatalf("policy written before the display field: %+v %v", p, err)
	}
	waitFor(t, "published despite the display error", func() bool { return orec.has(1) })
	_ = v.Close()
	rec := &polRec{}
	v.WatchPolicies(rec.add) // closed: no subscription
	time.Sleep(50 * time.Millisecond)
	if rec.count() != 0 {
		t.Fatal("closed store delivered policies")
	}
}

// TestFallbackPolicies: admin operations need Valkey; the watcher starts as
// soon as the first connection succeeds.
func TestFallbackPolicies(t *testing.T) {
	ctx := context.Background()
	f, _ := downStore(t, "local")
	if _, err := f.Policies(ctx); !errors.Is(err, limits.ErrStoreDown) {
		t.Fatalf("Policies = %v", err)
	}
	if err := f.SetTier(ctx, keyA, "pro"); !errors.Is(err, limits.ErrStoreDown) {
		t.Fatalf("SetTier = %v", err)
	}
	if err := f.ResetEntity(ctx, keyA, true, false); !errors.Is(err, limits.ErrStoreDown) {
		t.Fatalf("ResetEntity = %v", err)
	}
	f.WatchPolicies(func(limits.Policies, error) {}) // stopped by Close

	tg := valkeyTargetEnv(t)
	cfg := tg.config()
	var tries atomic.Int32
	g := limits.NewFallback(ctx, cfg, func(ctx context.Context) (*limits.Valkey, error) {
		if tries.Add(1) == 1 {
			return nil, errors.New("down at boot")
		}
		return limits.NewValkey(ctx, cfg, limits.ValkeyOptions{DB: tg.db, Heartbeat: -1})
	}, limits.FallbackOptions{ProbeInterval: 20 * time.Millisecond})
	t.Cleanup(func() { _ = g.Close() })
	rec := &polRec{}
	g.WatchPolicies(rec.add)
	waitFor(t, "policies after the first connection", func() bool { return rec.count() > 0 })
}

// TestValkeyPoliciesResubscribe: the subscription connection goes away; the
// watcher re-subscribes and re-reads what changed in the meantime.
func TestValkeyPoliciesResubscribe(t *testing.T) {
	addr, _ := valkeyEnv(t)
	writer := newValkeyStore(t)
	p := newProxy(t, addr)
	v := openValkey(t, func(c *config.StoreConfig, _ *limits.ValkeyOptions) { c.Addr = []string{p.addr} })
	rec := &polRec{}
	v.WatchPolicies(rec.add)
	waitFor(t, "first delivery", func() bool { return rec.has(0) })
	p.stop()
	time.Sleep(100 * time.Millisecond)
	n := panelWrite(t, writer, true, "tier:pro", `{"rpm": 5}`)
	p.start(p.addr)
	deadline := time.Now().Add(10 * time.Second)
	for !rec.has(n) {
		if time.Now().After(deadline) {
			t.Fatalf("version %d not re-read after the outage", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestValkeyPoliciesClientClosed: when Receive gives up (client closed under
// it), the loop waits and retries until the store is closed.
func TestValkeyPoliciesClientClosed(t *testing.T) {
	v := newValkeyStore(t)
	rec := &polRec{}
	v.WatchPolicies(rec.add)
	waitFor(t, "first delivery", func() bool { return rec.has(0) })
	v.Client().Close()
	time.Sleep(100 * time.Millisecond)
	done := make(chan struct{})
	go func() { _ = v.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked by the subscription loop")
	}
}
