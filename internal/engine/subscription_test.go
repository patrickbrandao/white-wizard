package engine_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/valkey-io/valkey-go"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// subscriptionCfg: the anthropic driver without oauth_key, a keys[] entry
// whose value is an OAuth-looking token (a key like any other, ADR 0074)
// in an account with a quota_usd of $5 in 5 h and $30 a week, a key
// without quota and answers of $1 (20 output tokens at $50 000 per
// million).
const subscriptionCfg = `
server: {seed: 3}
profiles:
  latency: {zero: {}}
  generation:
    text: {mode: lorem, output_tokens: {dist: fixed, value: 8}}
models:
  - {id: claude-sonnet-5-5, latency: zero, generation: text, pricing: {input_per_m: 0, output_per_m: 50000}}
accounts:
  - {id: acct-q, limits: {quota_usd: {monthly_usd: 100, rolling_5h_pct: 5, weekly_pct: 30}}}
keys:
  - {key: sk-ant-oat01-literal-subscription, label: sub, account: acct-q}
  - {key: sk-ant-oat01-literal-noquota, label: free}
  - {key: sk-ant-api03-plain, label: api, account: acct-q}
`

const subscriptionBody = `{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`

// subscriptionEngine is an engine over cfg and store.
func subscriptionEngine(t *testing.T, cfg string, store limits.Store) *engine.Engine {
	t.Helper()
	c, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return engine.New(c, store, scenario.NewRegistry(), engine.NewMetrics(metrics.New(1000), nil), log)
}

// subscriptionCall sends a Messages request with hdr to the anthropic
// driver.
func subscriptionCall(t *testing.T, e *engine.Engine, kind driver.Kind, path string, hdr map[string]string) *core.MemWriter {
	t.Helper()
	drv, err := driver.New("anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := driver.MapHeaders{"anthropic-version": "2023-06-01"}
	for k, v := range hdr {
		h[k] = v
	}
	w := core.NewMemWriter()
	e.Handle(context.Background(), engine.Input{Driver: drv, Kind: kind, Method: "POST", Path: path, Headers: h, Body: []byte(subscriptionBody), ClientIP: "127.0.0.1"}, w)
	return w
}

func bearer(key string) map[string]string { return map[string]string{"authorization": "Bearer " + key} }

// quotaRun sends Messages with the subscription's token until the quota
// refuses one: the 5 h use of each 200 and the status, claim and
// retry-after of the 429.
func quotaRun(t *testing.T, store limits.Store) []string {
	t.Helper()
	e := subscriptionEngine(t, subscriptionCfg, store)
	var out []string
	for range 10 {
		w := subscriptionCall(t, e, driver.KindMessages, "/v1/messages", bearer("sk-ant-oat01-literal-subscription"))
		h := w.Headers()
		if w.StatusCode() != 200 {
			retry, _ := strconv.Atoi(h["retry-after"])
			out = append(out, strconv.Itoa(w.StatusCode())+" "+h["anthropic-ratelimit-unified-status"]+" "+h["anthropic-ratelimit-unified-representative-claim"]+" "+strconv.FormatBool(retry > 0 && retry <= 5*3600))
			return out
		}
		out = append(out, h["anthropic-ratelimit-unified-5h-utilization"]+"/"+h["anthropic-ratelimit-unified-7d-utilization"])
	}
	t.Fatalf("the quota never refused: %v", out)
	return nil
}

// TestSubscriptionQuotaStores: the anthropic-ratelimit-unified-* of a
// subscription's token grow with the account's quota_usd debit and the
// 429 is rejected with the 5 h claim, the same on store: memory and store:
// valkey (spec 006 16.2.1, ADR 0074).
func TestSubscriptionQuotaStores(t *testing.T) {
	mem := quotaRun(t, limits.NewMemory())
	if len(mem) < 3 || mem[0] != "0/0" || mem[len(mem)-1] != "429 rejected five_hour true" {
		t.Fatalf("memory: %v", mem)
	}
	prev := -1.0
	for _, u := range mem[:len(mem)-1] {
		five, _, _ := strings.Cut(u, "/")
		f, err := strconv.ParseFloat(five, 64)
		if err != nil || f <= prev || f >= 1 {
			t.Fatalf("memory: the 5 h use does not grow with the spend: %v", mem)
		}
		prev = f
	}
	if got := quotaRun(t, testValkeyStore(t)); !slices.Equal(got, mem) {
		t.Errorf("valkey: %v, memory %v", got, mem)
	}
}

// testValkeyStore is a Valkey store of scripts/verify.sh's test Valkey
// under a random prefix (removed at the end), or skips. The database is
// WWZ_TEST_VALKEY_DB−3, shared with internal/panel/api, which also never
// flushes it.
func testValkeyStore(t *testing.T) limits.Store {
	t.Helper()
	if os.Getenv("WWZ_TEST_VALKEY") != "local" {
		t.Skip("Valkey de teste ausente: rode `make verify-quick` (ou exporte WWZ_TEST_VALKEY=local WWZ_TEST_VALKEY_ADDR=127.0.0.1:16379)")
	}
	addr := os.Getenv("WWZ_TEST_VALKEY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	db := 15
	if s := os.Getenv("WWZ_TEST_VALKEY_DB"); s != "" {
		db, _ = strconv.Atoi(s)
	}
	if db -= 3; db <= 0 {
		t.Skip("sem banco de teste livre abaixo de WWZ_TEST_VALKEY_DB")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	prefix := "wwzeng" + hex.EncodeToString(b)
	ctx := context.Background()
	st, err := limits.NewValkey(ctx, config.StoreConfig{Addr: []string{addr}, Prefix: prefix}, limits.ValkeyOptions{DB: db, Heartbeat: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		raw, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}, SelectDB: db, ForceSingleClient: true, DisableCache: true})
		if err != nil {
			return
		}
		defer raw.Close()
		keys, _ := raw.Do(ctx, raw.B().Keys().Pattern(prefix+":*").Build()).AsStrSlice()
		if len(keys) > 0 {
			_ = raw.Do(ctx, raw.B().Del().Key(keys...).Build()).Error()
		}
	})
	return st
}

// TestSubscriptionTokenChecks: where the engine asks the driver about a
// token (driver.TokenChecker) and stores the quota view
// (driver.SubscriptionReporter): an OAuth-looking key of keys[] is a key;
// one outside keys[] is the invalid token, any other unknown key keeps the
// engine's 401; with server.auth: none every Bearer passes and an OAuth one
// gets the unified headers without a quota; an API key and count_tokens
// get none.
func TestSubscriptionTokenChecks(t *testing.T) {
	e := subscriptionEngine(t, subscriptionCfg, limits.NewMemory())
	msg := func(w *core.MemWriter) string { return strconv.Itoa(w.StatusCode()) + " " + string(w.Body()) }
	if w := subscriptionCall(t, e, driver.KindMessages, "/v1/messages", bearer("sk-ant-oat01-unknown")); w.StatusCode() != 401 || !strings.Contains(msg(w), "OAuth access token is invalid.") {
		t.Errorf("unknown token: %s", msg(w))
	}
	if w := subscriptionCall(t, e, driver.KindMessages, "/v1/messages", bearer("sk-unknown")); w.StatusCode() != 401 || !strings.Contains(msg(w), "invalid x-api-key") {
		t.Errorf("unknown key: %s", msg(w))
	}
	w := subscriptionCall(t, e, driver.KindMessages, "/v1/messages", bearer("sk-ant-oat01-literal-noquota"))
	if w.StatusCode() != 200 || w.Headers()["anthropic-ratelimit-unified-5h-utilization"] != "0" || w.Headers()["anthropic-ratelimit-requests-limit"] != "" {
		t.Errorf("key without quota: %s %v", msg(w), w.Headers())
	}
	w = subscriptionCall(t, e, driver.KindMessages, "/v1/messages", map[string]string{"x-api-key": "sk-ant-api03-plain"})
	if w.StatusCode() != 200 || w.Headers()["anthropic-ratelimit-unified-status"] != "" || w.Headers()["anthropic-ratelimit-requests-limit"] == "" {
		t.Errorf("API key: %s %v", msg(w), w.Headers())
	}
	w = subscriptionCall(t, e, driver.KindCountTokens, "/v1/messages/count_tokens", bearer("sk-ant-oat01-literal-subscription"))
	if w.StatusCode() != 200 || w.Headers()["anthropic-ratelimit-unified-status"] != "" {
		t.Errorf("count_tokens: %s %v", msg(w), w.Headers())
	}
	open := subscriptionEngine(t, strings.Replace(subscriptionCfg, "server: {seed: 3}", "server: {seed: 3, auth: none}", 1), limits.NewMemory())
	w = subscriptionCall(t, open, driver.KindMessages, "/v1/messages", bearer("sk-ant-oat01-anything"))
	if w.StatusCode() != 200 || w.Headers()["anthropic-ratelimit-unified-status"] != "allowed" {
		t.Errorf("auth none: %s %v", msg(w), w.Headers())
	}
}
