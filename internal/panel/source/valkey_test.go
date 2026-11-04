package source

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	panelcfg "github.com/patrickbrandao/white-wizard/internal/panel/config"
)

// testDB returns the test Valkey of scripts/verify.sh (WWZ_TEST_VALKEY=local,
// WWZ_TEST_VALKEY_ADDR, WWZ_TEST_VALKEY_DB) or skips. go test runs packages
// in parallel and FLUSHes: internal/limits uses WWZ_TEST_VALKEY_DB, cmd/wwz
// the one below, this package the one below that (default 13).
func testDB(t *testing.T) (string, int) {
	t.Helper()
	if os.Getenv("WWZ_TEST_VALKEY") == "" {
		t.Skip("Valkey de teste ausente: rode `make verify-quick` (ou exporte WWZ_TEST_VALKEY=local WWZ_TEST_VALKEY_ADDR=127.0.0.1:16379)")
	}
	addr := os.Getenv("WWZ_TEST_VALKEY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	db := 15
	if s := os.Getenv("WWZ_TEST_VALKEY_DB"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n == 0 {
			t.Fatalf("WWZ_TEST_VALKEY_DB=%q: os testes nunca usam o banco 0", s)
		}
		db = n
	}
	db -= 2
	if db <= 0 {
		t.Skip("sem banco de teste livre abaixo de WWZ_TEST_VALKEY_DB")
	}
	return addr, db
}

type env struct {
	addr string
	db   int
	raw  valkey.Client
	src  *Valkey
}

// newEnv flushes the test database and opens a Valkey source on it.
func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvPrefix(t, "wwz")
}

// newEnvPrefix is newEnv with store.prefix = prefix. Pub/Sub channels are per
// server, not per database: tests that SUBSCRIBE use a prefix of their own so
// that P:cfg:changed published by other packages running in parallel against
// the same Valkey (internal/limits, cmd/wwz) never reaches them.
func newEnvPrefix(t *testing.T, prefix string) *env {
	t.Helper()
	addr, db := testDB(t)
	raw, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}, SelectDB: db, ForceSingleClient: true, DisableCache: true})
	if err != nil {
		t.Fatalf("valkey: %v", err)
	}
	t.Cleanup(raw.Close)
	if err := raw.Do(context.Background(), raw.B().Flushdb().Build()).Error(); err != nil {
		t.Fatalf("FLUSHDB: %v", err)
	}
	src, err := NewValkey(panelcfg.Valkey{Addr: []string{addr}, Prefix: prefix}, ValkeyOptions{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Close)
	return &env{addr: addr, db: db, raw: raw, src: src}
}

// limitsStore opens the real wwz store (internal/limits) on the same database.
func (e *env) limitsStore(t *testing.T, instance string, hb time.Duration) *limits.Valkey {
	t.Helper()
	cfg := config.StoreConfig{
		Kind: "valkey", Addr: []string{e.addr}, ReadTimeout: 5 * time.Second, OnDown: "local",
		Prefix: "wwz", EventStream: true, EventMaxLen: 100000, EventSample: 1,
	}
	v, err := limits.NewValkey(context.Background(), cfg, limits.ValkeyOptions{DB: e.db, Instance: instance, Version: "9.9.9", Addr: "10.1.1.1:9000", Heartbeat: hb})
	if err != nil {
		t.Fatalf("NewValkey: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func (e *env) do(t *testing.T, cmd ...string) valkey.ValkeyResult {
	t.Helper()
	r := e.raw.Do(context.Background(), e.raw.B().Arbitrary(cmd...).Build())
	if err := r.Error(); err != nil && !valkey.IsValkeyNil(err) {
		t.Fatalf("%v: %v", cmd, err)
	}
	return r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewValkeyOff(t *testing.T) {
	if _, err := NewValkey(panelcfg.Valkey{}, ValkeyOptions{}); !errors.Is(err, ErrValkeyOff) {
		t.Fatalf("err = %v, want ErrValkeyOff", err)
	}
	v, err := NewValkey(panelcfg.Valkey{Addr: []string{"x:1"}, Cluster: true}, ValkeyOptions{})
	must(t, err)
	if v.Prefix() != "wwz" || v.opt.ForceSingleClient || !strings.HasPrefix(v.opt.ClientName, "wwz-panel:") || v.timeout != 2*time.Second {
		t.Fatalf("defaults: %+v", v)
	}
	if HTTPStatus(ErrValkeyOff) != 409 {
		t.Fatal("ErrValkeyOff must be 409")
	}
}

// TestValkeyDown covers the lazy dial: a Valkey that is down makes every
// call fail (502), the dial is retried at most once per second and nothing
// panics.
func TestValkeyDown(t *testing.T) {
	v, err := NewValkey(panelcfg.Valkey{Addr: []string{"127.0.0.1:1"}, Prefix: "p"}, ValkeyOptions{Timeout: 200 * time.Millisecond})
	must(t, err)
	defer v.Close()
	ctx := context.Background()
	err1 := v.Ping(ctx)
	if err1 == nil || !strings.Contains(err1.Error(), "127.0.0.1:1") {
		t.Fatalf("Ping = %v", err1)
	}
	if err2 := v.Ping(ctx); err2 != err1 {
		t.Fatalf("second Ping within 1 s must reuse the dial error: %v", err2)
	}
	e := limits.Entity{Kind: limits.KindKey, ID: "k"}
	checks := []error{}
	_, err = v.Entity(ctx, e)
	checks = append(checks, err)
	_, err = v.Entities(ctx, "", 0)
	checks = append(checks, err)
	_, err = v.Limits(ctx, e, config.Limits{RPM: 1})
	checks = append(checks, err)
	_, err = v.Instances(ctx)
	checks = append(checks, err)
	_, err = v.Policies(ctx)
	checks = append(checks, err)
	_, err = v.ReplaceTiers(ctx, 0, nil, nil)
	checks = append(checks, err)
	_, err = v.StreamLag(ctx, Group)
	checks = append(checks, err)
	for i, err := range checks {
		if err == nil {
			t.Fatalf("call %d: no error with Valkey down", i)
		}
		if HTTPStatus(err) != 502 {
			t.Fatalf("call %d: status %d", i, HTTPStatus(err))
		}
	}
}

func TestEscapeID(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.1": "10.0.0.1",
		"a:b{c}":   "a%3Ab%7Bc%7D",
		"::1":      "%3A%3A1",
	} {
		if got := escapeID(in); got != want {
			t.Errorf("escapeID(%q) = %q, want %q", in, got, want)
		}
	}
	if got := escapeID(strings.Repeat("é", 100)); len(got) != 33 || got[0] != '~' {
		t.Errorf("long id: %q", got)
	}
}

// TestEntitiesMatchLimitsStore writes entities with the real wwz store and
// checks that the panel reads the same state from the key schema.
func TestEntitiesMatchLimitsStore(t *testing.T) {
	e := newEnv(t)
	lv := e.limitsStore(t, "inst-a", -1)
	ctx := context.Background()
	must(t, e.src.Ping(ctx))
	blocked := limits.Entity{Kind: limits.KindKey, ID: "sk:a{b} c"}
	banned := limits.Entity{Kind: limits.KindKey, ID: "banned"}
	both := limits.Entity{Kind: limits.KindUser, ID: "both"}
	debt := limits.Entity{Kind: limits.KindAccount, ID: "debt"}
	expired := limits.Entity{Kind: limits.KindIP, ID: "::1"}
	long := limits.Entity{Kind: limits.KindProject, ID: strings.Repeat("p", 300)}
	must(t, lv.Block(ctx, blocked, "too fast", time.Hour, "admin"))
	must(t, lv.Ban(ctx, banned, "abuse", "panel:ana"))
	must(t, lv.Block(ctx, both, "x", 0, "admin"))
	must(t, lv.Ban(ctx, both, "y", "admin"))
	must(t, lv.SetCredits(ctx, debt, -2.5))
	if _, err := lv.AddCredits(ctx, long, 3.25); err != nil {
		t.Fatal(err)
	}
	must(t, lv.Block(ctx, expired, "", time.Millisecond, "abuse"))
	if _, err := lv.Strike(ctx, long, "x", time.Hour); err != nil {
		t.Fatal(err)
	}
	must(t, lv.Finish(ctx, limits.FinishRequest{
		Subjects: []limits.Subject{{Entity: long}}, RequestID: "r1", CostMicroUSD: 1_500_000, Status: 200, Driver: "openai", Model: "m",
	}))
	e.do(t, "HSET", "wwz:ent:{key:sk%3Aa%7Bb%7D%20c}", "tier", "tier-4")
	// Stale index member without ent: skipped (the panel does not repair).
	e.do(t, "ZADD", "wwz:idx:ent", "1", "key:ghost")
	time.Sleep(5 * time.Millisecond)

	all := []limits.Entity{blocked, banned, both, debt, expired, long, {Kind: limits.KindKey, ID: "nobody"}}
	for _, ent := range all {
		got, err := e.src.Entity(ctx, ent)
		must(t, err)
		want, err := lv.Get(ctx, ent)
		must(t, err)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Entity(%v)\n got %+v\nwant %+v", ent, got, want)
		}
	}
	if st, _ := e.src.Entity(ctx, blocked); st.Tier != "tier-4" || st.Status != limits.StatusSuspended {
		t.Errorf("blocked = %+v", st)
	}
	if st, _ := e.src.Entity(ctx, long); st.SpendUSD != 1.5 || st.Strikes != 1 || st.PurchasedUSD != 3.25 {
		t.Errorf("long = %+v", st)
	}
	for _, status := range []limits.Status{"", limits.StatusActive, limits.StatusSuspended, limits.StatusBanned, limits.StatusLimited} {
		for _, lim := range []int{0, 2} {
			got, err := e.src.Entities(ctx, status, lim)
			must(t, err)
			want, err := lv.List(ctx, status, lim)
			must(t, err)
			if want == nil {
				want = []limits.EntityState{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Entities(%q, %d)\n got %+v\nwant %+v", status, lim, got, want)
			}
		}
	}
	if got, _ := e.src.Entities(ctx, limits.StatusBanned, 0); len(got) != 2 {
		t.Errorf("banned = %d entities, want 2", len(got))
	}
	// The panel never writes ent: (only cfg:policies).
	if n, _ := e.do(t, "EXISTS", "wwz:ent:{key:nobody}").AsInt64(); n != 0 {
		t.Error("Entity must not create ent:")
	}
}

func TestInstances(t *testing.T) {
	e := newEnv(t)
	_ = e.limitsStore(t, "inst-a", time.Hour) // beats once at start
	ctx := context.Background()
	now := time.Now().UnixMilli()
	nowS := strconv.FormatInt(now, 10)
	e.do(t, "ZADD", "wwz:idx:inst", nowS, "ghost")
	e.do(t, "ZADD", "wwz:idx:inst", strconv.FormatInt(now-60_000, 10), "old")
	e.do(t, "HSET", "wwz:inst:{old}", "id", "old")
	e.do(t, "ZADD", "wwz:idx:inst", nowS, "b x")
	e.do(t, "HSET", "wwz:inst:{b%20x}", "version", "1", "beat", nowS, "rps", "12.5", "conns", "3", "inflight", "2", "store", "open")
	got, err := e.src.Instances(ctx)
	must(t, err)
	if len(got) != 2 {
		t.Fatalf("instances = %+v", got)
	}
	if got[0].ID != "b x" || got[0].RPS != 12.5 || got[0].Conns != 3 || got[0].InFlight != 2 || got[0].Store != "open" || got[0].Beat.UnixMilli() != now {
		t.Errorf("b x = %+v", got[0])
	}
	a := got[1]
	if a.ID != "inst-a" || a.Version != "9.9.9" || a.Addr != "10.1.1.1:9000" || a.Store != "ok" || a.Started.IsZero() || time.Since(a.Beat) > time.Minute {
		t.Errorf("inst-a = %+v", a)
	}
	e.do(t, "DEL", "wwz:idx:inst")
	if got, err := e.src.Instances(ctx); err != nil || len(got) != 0 || got == nil {
		t.Errorf("empty = %v, %v", got, err)
	}
	e.do(t, "SET", "wwz:idx:inst", "x")
	if _, err := e.src.Instances(ctx); err == nil {
		t.Error("wrong type idx:inst: want error")
	}
	e.do(t, "DEL", "wwz:idx:inst")
	e.do(t, "ZADD", "wwz:idx:inst", nowS, "bad")
	e.do(t, "SET", "wwz:inst:{bad}", "x")
	if _, err := e.src.Instances(ctx); err == nil {
		t.Error("wrong type inst: want error")
	}
}

func TestWrongTypes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ent := limits.Entity{Kind: limits.KindKey, ID: "k"}
	e.do(t, "SET", "wwz:ent:{key:k}", "x")
	if _, err := e.src.Entity(ctx, ent); err == nil {
		t.Error("ent: string: want error")
	}
	e.do(t, "DEL", "wwz:ent:{key:k}")
	e.do(t, "SET", "wwz:win:{key:k}", "x")
	if _, err := e.src.Entity(ctx, ent); err == nil {
		t.Error("win: string: want error")
	}
	e.do(t, "ZADD", "wwz:idx:ent", "1", "key:k")
	if _, err := e.src.Entities(ctx, "", 0); err == nil {
		t.Error("Entities with bad win: want error")
	}
	e.do(t, "SET", "wwz:idx:ent", "x")
	if _, err := e.src.Entities(ctx, "", 0); err == nil {
		t.Error("idx:ent string: want error")
	}
	if _, err := e.src.Limits(ctx, ent, config.Limits{RPM: 1}); err == nil {
		t.Error("Limits with bad win: want error")
	}
	e.do(t, "DEL", "wwz:win:{key:k}")
	e.do(t, "HSET", "wwz:conc:{key:k}", "a", "b")
	if _, err := e.src.Limits(ctx, ent, config.Limits{RPM: 1}); err == nil {
		t.Error("Limits with bad conc: want error")
	}
	e.do(t, "SET", "wwz:cfg:policies", "x")
	if _, err := e.src.Policies(ctx); err == nil {
		t.Error("Policies string: want error")
	}
	if _, err := e.src.ReplaceTiers(ctx, 0, nil, nil); err == nil {
		t.Error("ReplaceTiers on string: want error")
	}
	e.do(t, "SET", "wwz:ev:stream", "x")
	if _, err := e.src.StreamLag(ctx, Group); err == nil {
		t.Error("StreamLag on string: want error")
	}
}

// TestLimitsFromStore reads live limits after real Check/Finish calls.
func TestLimitsFromStore(t *testing.T) {
	e := newEnv(t)
	lv := e.limitsStore(t, "inst-a", -1)
	ctx := context.Background()
	ent := limits.Entity{Kind: limits.KindKey, ID: "live"}
	lim := config.Limits{
		RPS: 10, Burst: 20, RPM: 60, TPM: 1000, ITPM: 500, OTPM: 400, RPD: 100, TPD: 10000,
		Concurrency: 5, SpendMonthlyUSD: 10, QuotaUSD: &config.QuotaUSD{MonthlyUSD: 100, Rolling5hPct: 10, WeeklyPct: 30},
	}
	subj := []limits.Subject{{Entity: ent, Limits: lim}}
	d, err := lv.Check(ctx, limits.CheckRequest{Subjects: subj, EstPromptTokens: 100, EstOutputTokens: 50})
	must(t, err)
	if !d.Allowed {
		t.Fatalf("check denied: %+v", d)
	}
	got, err := e.src.Limits(ctx, ent, lim)
	must(t, err)
	byName := map[string]LimitState{}
	for _, s := range got {
		byName[s.Name] = s
	}
	near := func(name string, remaining, tol float64) {
		t.Helper()
		s, ok := byName[name]
		if !ok {
			t.Fatalf("%s missing in %+v", name, got)
		}
		if s.Remaining < remaining-tol || s.Remaining > remaining+tol {
			t.Errorf("%s remaining = %v, want %v±%v (%+v)", name, s.Remaining, remaining, tol, s)
		}
		if s.Remaining < s.Limit && s.Name != "concurrency" && s.Reset.IsZero() {
			t.Errorf("%s: consumed but no reset", name)
		}
	}
	near("rps", 19, 1)
	near("rpm", 59, 0.5)
	near("tpm", 850, 5)
	near("itpm", 400, 5)
	near("otpm", 400, 0.01)
	near("rpd", 99, 0)
	near("tpd", 9850, 0)
	near("concurrency", 4, 0)
	near("spend_monthly", 10, 0)
	if len(got) != 12 {
		t.Errorf("limits = %d, want 12: %+v", len(got), got)
	}
	must(t, lv.Finish(ctx, limits.FinishRequest{
		Subjects: subj, RequestID: "r", EstOutputTokens: 50, ActualOutput: 20, CostMicroUSD: 2_000_000, Status: 200,
	}))
	got, err = e.src.Limits(ctx, ent, lim)
	must(t, err)
	byName = map[string]LimitState{}
	for _, s := range got {
		byName[s.Name] = s
	}
	near("concurrency", 5, 0)
	near("spend_monthly", 8, 1e-9)
	near("quota_5h", 8, 1e-9)
	near("quota_weekly", 28, 1e-9)
	near("quota_monthly", 98, 1e-9)
	near("otpm", 380, 1)

	fixed := limits.Entity{Kind: limits.KindKey, ID: "fixed"}
	flim := config.Limits{RPM: 3, TPM: 1000, Algorithm: "fixed_window"}
	_, err = lv.Check(ctx, limits.CheckRequest{Subjects: []limits.Subject{{Entity: fixed, Limits: flim}}, EstPromptTokens: 10})
	must(t, err)
	got, err = e.src.Limits(ctx, fixed, flim)
	must(t, err)
	if len(got) != 2 || got[0].Name != "rpm" || got[0].Used != 1 || got[0].Remaining != 2 || got[0].Window != "minute" ||
		got[1].Name != "tpm" || got[1].Used != 10 || got[0].Reset.IsZero() {
		t.Errorf("fixed = %+v", got)
	}
}

func TestLiveLimitsUnit(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	nowMS := now.UnixMilli()
	s := func(n int64) string { return strconv.FormatInt(n, 10) }
	// Nothing consumed: every limit full, no reset.
	lim := config.Limits{
		RPS: 5, RPM: 60, RPD: 10, TPD: 10, Concurrency: 2, SpendMonthlyUSD: 1, Algorithm: "fixed_window",
		QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10, Rolling5hPct: 0, WeeklyPct: 50},
	}
	out := liveLimits(map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{"junk": "1"}, 0, lim, now)
	for _, st := range out {
		if st.Remaining != st.Limit || !st.Reset.IsZero() || st.Used != 0 {
			t.Errorf("fresh %s = %+v", st.Name, st)
		}
	}
	if out[0].Name != "rps" || out[0].Limit != 5 {
		t.Errorf("rps burst defaults to rps: %+v", out[0])
	}
	// Bucket whose cap/rate changed: tokens cut at the new cap.
	tb := map[string]string{"rpm:t": "100", "rpm:ts": s(nowMS), "rpm:cap": "120", "rpm:rate": "2"}
	out = liveLimits(tb, nil, nil, nil, 0, config.Limits{RPM: 60}, now)
	if out[0].Remaining != 60 {
		t.Errorf("cap change: %+v", out[0])
	}
	// Bucket refilled since ts, capped.
	tb = map[string]string{"rpm:t": "0", "rpm:ts": s(nowMS - 30_000), "rpm:cap": "60", "rpm:rate": "1"}
	out = liveLimits(tb, nil, nil, nil, 0, config.Limits{RPM: 60}, now)
	if out[0].Remaining != 30 || out[0].Reset != now.Add(30*time.Second) {
		t.Errorf("refill: %+v", out[0])
	}
	// Stale windows: fixed minute and day restart, month from a previous month.
	win := map[string]string{
		"rpm:s": s(nowMS - 120_000), "rpm:c": "50", "rpd:s": s(nowMS - 2*dayMS), "rpd:c": "9",
		"spend:s": s(monthStart(now).AddDate(0, -1, 0).UnixMilli()), "spend:c": "900000",
	}
	out = liveLimits(nil, win, nil, nil, 0, config.Limits{RPM: 60, RPD: 10, SpendMonthlyUSD: 1, Algorithm: "fixed_window"}, now)
	for _, st := range out {
		if st.Used != 0 {
			t.Errorf("stale %s = %+v", st.Name, st)
		}
	}
	// Live sliding buckets: sum and reset from the oldest live one.
	q5 := map[string]string{s(nowMS - 4*hourMS): "1000000", s(nowMS - 6*hourMS): "5000000", "x": "1"}
	out = liveLimits(nil, nil, q5, nil, 0, config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 100, Rolling5hPct: 10}}, now)
	if out[0].Name != "quota_5h" || out[0].Used != 1 || out[0].Limit != 10 || out[0].Reset.UnixMilli() != nowMS+hourMS {
		t.Errorf("q5h = %+v", out[0])
	}
	if out[1].Name != "quota_monthly" || len(out) != 2 {
		t.Errorf("quota without weekly = %+v", out)
	}
	if atoi("1.9") != 1 || atoi("x") != 0 || !fromMS("0").IsZero() {
		t.Error("number helpers")
	}
}

func TestPolicies(t *testing.T) {
	// Channel of its own (see newEnvPrefix): another package publishing on
	// wwz:cfg:changed made this test read {"version":3} (task 147).
	pfx := "wwzt" + strconv.FormatInt(time.Now().UnixNano(), 36)
	e := newEnvPrefix(t, pfx)
	channel := pfx + ":cfg:changed"
	policies := pfx + ":cfg:policies"
	ctx := context.Background()
	p, err := e.src.Policies(ctx)
	must(t, err)
	if p.Version != 0 || len(p.Tiers) != 0 {
		t.Fatalf("empty policies = %+v", p)
	}
	// Subscriber on cfg:changed.
	msgs := make(chan string, 16)
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = e.raw.Receive(sctx, e.raw.B().Subscribe().Channel(channel).Build(), func(m valkey.PubSubMessage) {
			select {
			case msgs <- m.Message:
			default: // never block the receiver
			}
		})
	}()
	for i := 0; ; i++ {
		res, _ := e.raw.Do(ctx, e.raw.B().PubsubNumsub().Channel(channel).Build()).AsIntMap()
		if res[channel] == 1 {
			break
		}
		if i > 200 {
			t.Fatal("subscriber not ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.do(t, "HSET", policies, "ent:key:sk:1", "gold")

	n, err := e.src.ReplaceTiers(ctx, 0, map[string]json.RawMessage{
		"gold":   json.RawMessage(`{ "rpm": 600 }`),
		"silver": json.RawMessage(`{"rpm":60}`),
	}, nil)
	must(t, err)
	if n != 1 {
		t.Fatalf("version = %d", n)
	}
	// Belt and braces: anything but the expected version is ignored until
	// the deadline.
	deadline := time.After(2 * time.Second)
	for got := ""; got != `{"version":1}`; {
		select {
		case got = <-msgs:
			if got != `{"version":1}` {
				t.Logf("ignoring foreign cfg:changed %q", got)
			}
		case <-deadline:
			t.Fatal(`no cfg:changed {"version":1}`)
		}
	}
	p, err = e.src.Policies(ctx)
	must(t, err)
	if p.Version != 1 || string(p.Tiers["gold"]) != `{"rpm":600}` || string(p.Tiers["silver"]) != `{"rpm":60}` ||
		p.Entities[limits.Entity{Kind: limits.KindKey, ID: "sk:1"}] != "gold" {
		t.Fatalf("policies = %+v", p)
	}
	// Stale version → conflict, nothing written.
	if _, err := e.src.ReplaceTiers(ctx, 0, nil, nil); !errors.Is(err, ErrPolicyConflict) || HTTPStatus(err) != 409 {
		t.Fatalf("stale version: %v", err)
	}
	// check refuses (e.g. removing a tier cited by ent:*).
	refuse := errors.New("tier citado")
	_, err = e.src.ReplaceTiers(ctx, 1, map[string]json.RawMessage{"silver": json.RawMessage(`{"rpm":60}`)}, func(cur Policies) error {
		if _, ok := cur.Tiers["gold"]; !ok {
			t.Error("check sees current tiers")
		}
		return refuse
	})
	if !errors.Is(err, refuse) {
		t.Fatalf("check error = %v", err)
	}
	// Remove gold, change silver, add bronze.
	n, err = e.src.ReplaceTiers(ctx, 1, map[string]json.RawMessage{
		"silver": json.RawMessage(`{"rpm":61}`), "bronze": json.RawMessage(`{"rpm":6}`),
	}, func(Policies) error { return nil })
	must(t, err)
	p, _ = e.src.Policies(ctx)
	if n != 2 || p.Version != 2 || len(p.Tiers) != 2 || string(p.Tiers["silver"]) != `{"rpm":61}` || len(p.Entities) != 1 {
		t.Fatalf("after replace: n=%d %+v", n, p)
	}
	// Unchanged set still bumps the version.
	n, err = e.src.ReplaceTiers(ctx, 2, p.Tiers, nil)
	must(t, err)
	if n != 3 {
		t.Fatalf("unchanged: %d", n)
	}
	// Another writer between WATCH and EXEC → conflict.
	_, err = e.src.ReplaceTiers(ctx, 3, nil, func(Policies) error {
		e.do(t, "HINCRBY", policies, "version", "1")
		return nil
	})
	if !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("concurrent writer: %v", err)
	}
	// Invalid input → 400 before touching Valkey.
	for _, tiers := range []map[string]json.RawMessage{
		{"": json.RawMessage(`{}`)},
		{"a b": json.RawMessage(`{}`)},
		{strings.Repeat("a", 65): json.RawMessage(`{}`)},
		{"a:b": json.RawMessage(`{}`)}, // ':' is the tier:<name> separator (ADR 0031)
		{"pró": json.RawMessage(`{}`)},
		{"ok": json.RawMessage(`[1]`)},
		{"ok": json.RawMessage(`null`)},
	} {
		if _, err := e.src.ReplaceTiers(ctx, 4, tiers, nil); !errors.Is(err, ErrInvalidPolicy) || HTTPStatus(err) != 400 {
			t.Errorf("tiers %v: %v", tiers, err)
		}
	}
	p, _ = e.src.Policies(ctx)
	if p.Version != 4 || len(p.Entities) != 1 {
		t.Fatalf("final = %+v", p)
	}
	// A version that is not an integer reads as 0 and makes HINCRBY fail
	// inside EXEC.
	e.do(t, "HSET", policies, "version", "abc")
	if _, err := e.src.ReplaceTiers(ctx, 0, nil, nil); err == nil || errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("bad version field: %v", err)
	}
}

func TestPublishErr(t *testing.T) {
	if publishErr(3, nil) != nil {
		t.Fatal("nil")
	}
	if err := publishErr(3, errors.New("x")); err == nil || !strings.Contains(err.Error(), "versão 3") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamLag(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if n, err := e.src.StreamLag(ctx, Group); err != nil || n != 0 {
		t.Fatalf("no stream: %d %v", n, err)
	}
	e.do(t, "XGROUP", "CREATE", "wwz:ev:stream", "other", "$", "MKSTREAM")
	if n, err := e.src.StreamLag(ctx, Group); err != nil || n != 0 {
		t.Fatalf("no group: %d %v", n, err)
	}
	e.do(t, "XGROUP", "CREATE", "wwz:ev:stream", Group, "$")
	for range 3 {
		e.do(t, "XADD", "wwz:ev:stream", "*", "d", "{}")
	}
	if n, err := e.src.StreamLag(ctx, Group); err != nil || n != 3 {
		t.Fatalf("lag = %d %v", n, err)
	}
	// Deleting an entry inside the unread range makes the lag unknown.
	ids, _ := e.do(t, "XRANGE", "wwz:ev:stream", "-", "+").AsXRange()
	e.do(t, "XDEL", "wwz:ev:stream", ids[1].ID)
	if n, err := e.src.StreamLag(ctx, Group); err != nil || n != -1 {
		t.Fatalf("unknown lag = %d %v", n, err)
	}
}
