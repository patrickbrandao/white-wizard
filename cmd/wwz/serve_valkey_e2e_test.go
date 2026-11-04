package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// testValkeyDB connects to the test Valkey exported by scripts/verify.sh, on
// the database below WWZ_TEST_VALKEY_DB (internal/limits FLUSHes its own in
// parallel), flushes it and points `wwz serve` at it. Skips without Valkey.
func testValkeyDB(t *testing.T) (string, valkey.Client) {
	t.Helper()
	if os.Getenv("WWZ_TEST_VALKEY") == "" {
		t.Skip("Valkey de teste ausente: suba com `docker compose up -d valkey` e rode `make verify-quick`")
	}
	addr := os.Getenv("WWZ_TEST_VALKEY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	db := 15
	if s := os.Getenv("WWZ_TEST_VALKEY_DB"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n == 0 {
			t.Fatalf("WWZ_TEST_VALKEY_DB=%q: tests never use database 0", s)
		}
		db = n
	}
	db--
	if db <= 0 {
		t.Skip("no spare test database below WWZ_TEST_VALKEY_DB")
	}
	old := valkeyDB
	valkeyDB = db
	t.Cleanup(func() { valkeyDB = old })
	v, err := limits.NewValkey(context.Background(), config.StoreConfig{Addr: []string{addr}, ReadTimeout: time.Second}, limits.ValkeyOptions{DB: db, Heartbeat: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	c := v.Client()
	if err := c.Do(context.Background(), c.B().Arbitrary("FLUSHDB").Build()).Error(); err != nil {
		t.Fatal(err)
	}
	return addr, c
}

// keySchema is cache/valkey/README.md, sections 1 and 2: every key `wwz serve`
// writes must match one row, with the Valkey type of that row.
var keySchema = []struct {
	re  *regexp.Regexp
	typ string
}{
	{regexp.MustCompile(`^wwz:ent:\{(ip|key|user|project|account):[A-Za-z0-9._~@+=/%-]+\}$`), "hash"},
	{regexp.MustCompile(`^wwz:(tb|win|q5h|qwk|ramp):\{(ip|key|user|project|account):[A-Za-z0-9._~@+=/%-]+\}$`), "hash"},
	{regexp.MustCompile(`^wwz:conc:\{(ip|key|user|project|account):[A-Za-z0-9._~@+=/%-]+\}$`), "string"},
	{regexp.MustCompile(`^wwz:strikes:\{(ip|key|user|project|account):[A-Za-z0-9._~@+=/%-]+\}$`), "zset"},
	{regexp.MustCompile(`^wwz:idx:ent$`), "zset"},
	{regexp.MustCompile(`^wwz:idx:status:(banned|suspended|limited)$`), "zset"},
	{regexp.MustCompile(`^wwz:att:\{conv:[A-Za-z0-9._~@+=/%-]+\}$`), "hash"},
	{regexp.MustCompile(`^wwz:inst:\{[A-Za-z0-9._~@+=/%-]+\}$`), "hash"},
	{regexp.MustCompile(`^wwz:idx:inst$`), "zset"},
	{regexp.MustCompile(`^wwz:cfg:policies$`), "hash"},
	{regexp.MustCompile(`^wwz:ev:stream$`), "stream"},
}

// asyncAPIEvent lists the properties of the Event schema in
// eventbus/valkey/asyncapi.yaml (additionalProperties: false).
var asyncAPIEvent = map[string]bool{
	"t": true, "type": true, "instance": true, "request_id": true, "test_id": true, "driver": true, "endpoint": true,
	"model": true, "entity": true, "key": true, "account": true, "ip": true, "status": true, "error": true, "sample": true, "fields": true,
}

// asyncAPIFields lists the known properties of Event.fields in
// eventbus/valkey/asyncapi.yaml 0.5.0 (billing_source and allowance_usd come
// from the opencode driver, ADR 0072). The schema admits others; wwz writes
// only these.
var asyncAPIFields = map[string]bool{
	"duration_ms": true, "prompt_tokens": true, "output_tokens": true, "reasoning_tokens": true, "cached_tokens": true,
	"cost_usd": true, "billing_source": true, "allowance_usd": true, "tool_calls": true, "input_breakdown": true,
	"limit": true, "denied": true, "reason": true, "until": true, "by": true, "strikes": true,
}

// TestServeValkeyKeysEventsAndHeaders drives a real `wwz serve` with
// store: valkey and event_stream on (phase 2 exit, task 017): 429 headers per
// dialect, keys written vs cache/valkey/README.md and ev:stream entries vs
// eventbus/valkey/asyncapi.yaml.
func TestServeValkeyKeysEventsAndHeaders(t *testing.T) {
	addr, c := testValkeyDB(t)
	ctx := context.Background()
	cleanEnv(t)
	t.Setenv("WWZ_VALKEY_ADDR", addr)
	path := filepath.Join(t.TempDir(), "wwz.yaml")
	writeFile(t, path, `
server: {auth: keys, seed: 7}
store: {read_timeout: 1s, event_stream: true}
keys:
  - {key: sk-e2e-oai, label: e2e-oai, account: org-e2e, limits: {rpm: 1, tpm: 100000}}
  - {key: sk-e2e-ant, label: "e2e:ant", limits: {rpm: 1, itpm: 50000, otpm: 20000}}
`)
	in := startServe(t, "-config", path)

	oai := map[string]string{"Authorization": "Bearer sk-e2e-oai", "Content-Type": "application/json", "x-test-id": "t017"}
	ant := map[string]string{"x-api-key": "sk-e2e-ant", "anthropic-version": "2023-06-01", "Content-Type": "application/json"}
	oaiBody := `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"tool_choice":"required",` +
		`"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}}]}`
	antBody := `{"model":"wwz-fast","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	t.Run("OpenAI429Headers", func(t *testing.T) {
		code, h, body := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), oai, oaiBody)
		if code != 200 || h.Get("x-ratelimit-limit-requests") != "1" || h.Get("x-ratelimit-limit-tokens") != "100000" {
			t.Fatalf("first: %d %v %s", code, h, body)
		}
		code, h, body = do(t, "POST", url(in.a.Data, "/v1/chat/completions"), oai, oaiBody)
		if code != 429 {
			t.Fatalf("second: %d %s", code, body)
		}
		if h.Get("x-ratelimit-limit-requests") != "1" || h.Get("x-ratelimit-remaining-requests") != "0" {
			t.Errorf("429 x-ratelimit-*-requests: %v", h)
		}
		if _, err := time.ParseDuration(h.Get("x-ratelimit-reset-requests")); err != nil {
			t.Errorf("x-ratelimit-reset-requests %q: %v", h.Get("x-ratelimit-reset-requests"), err)
		}
		if ra, err := strconv.Atoi(h.Get("retry-after")); err != nil || ra < 1 || ra > 60 {
			t.Errorf("retry-after %q", h.Get("retry-after"))
		}
		var e struct {
			Error struct{ Type, Code string }
		}
		if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error.Code != "rate_limit_exceeded" || e.Error.Type != "requests" {
			t.Errorf("429 body: %s", body)
		}
	})

	t.Run("Anthropic429Headers", func(t *testing.T) {
		code, h, body := do(t, "POST", url(in.a.Data, "/v1/messages"), ant, antBody)
		if code != 200 || h.Get("anthropic-ratelimit-requests-limit") != "1" || h.Get("anthropic-ratelimit-input-tokens-limit") != "50000" || h.Get("anthropic-ratelimit-output-tokens-limit") != "20000" {
			t.Fatalf("first: %d %v %s", code, h, body)
		}
		code, h, body = do(t, "POST", url(in.a.Data, "/v1/messages"), ant, antBody)
		if code != 429 {
			t.Fatalf("second: %d %s", code, body)
		}
		if h.Get("anthropic-ratelimit-requests-limit") != "1" || h.Get("anthropic-ratelimit-requests-remaining") != "0" {
			t.Errorf("429 anthropic-ratelimit-requests-*: %v", h)
		}
		if _, err := time.Parse(time.RFC3339, h.Get("anthropic-ratelimit-requests-reset")); err != nil {
			t.Errorf("anthropic-ratelimit-requests-reset %q: %v", h.Get("anthropic-ratelimit-requests-reset"), err)
		}
		if ra, err := strconv.Atoi(h.Get("retry-after")); err != nil || ra < 1 || ra > 60 {
			t.Errorf("retry-after %q", h.Get("retry-after"))
		}
		if !strings.Contains(body, `"type":"rate_limit_error"`) {
			t.Errorf("429 body: %s", body)
		}
	})

	// Admin status changes on the key with an escaped id ("e2e:ant").
	for _, op := range []struct{ path, body string }{
		{"/admin/entities/key/e2e%3Aant/block", `{"duration":"60s","reason":"t017"}`},
		{"/admin/entities/key/e2e%3Aant/unblock", ``},
		{"/admin/entities/key/e2e%3Aant/ban", `{"reason":"t017"}`},
		{"/admin/entities/key/e2e%3Aant/unban", ``},
	} {
		if code, _, body := do(t, "POST", url(in.a.Admin, op.path), map[string]string{"Content-Type": "application/json"}, op.body); code != 200 {
			t.Fatalf("%s: %d %s", op.path, code, body)
		}
	}

	t.Run("KeysMatchREADME", func(t *testing.T) {
		var keys []string
		cursor := uint64(0)
		for {
			e, err := c.Do(ctx, c.B().Scan().Cursor(cursor).Count(1000).Build()).AsScanEntry()
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, e.Elements...)
			if cursor = e.Cursor; cursor == 0 {
				break
			}
		}
		seen := map[string]bool{}
		for _, k := range keys {
			seen[k] = true
			typ, err := c.Do(ctx, c.B().Type().Key(k).Build()).ToString()
			if err != nil {
				t.Fatal(err)
			}
			ok := false
			for _, row := range keySchema {
				if row.re.MatchString(k) {
					ok = true
					if typ != row.typ {
						t.Errorf("key %s: type %s, README says %s", k, typ, row.typ)
					}
				}
			}
			if !ok {
				t.Errorf("key %s (%s) not in cache/valkey/README.md", k, typ)
			}
		}
		for _, k := range []string{
			"wwz:ent:{key:e2e-oai}", "wwz:tb:{key:e2e-oai}", "wwz:ent:{key:e2e%3Aant}", "wwz:tb:{key:e2e%3Aant}",
			"wwz:idx:ent", "wwz:ev:stream",
		} {
			if !seen[k] {
				t.Errorf("expected key %s missing; got %v", k, keys)
			}
		}
		id, err := c.Do(ctx, c.B().Hget().Key("wwz:ent:{key:e2e%3Aant}").Field("id").Build()).ToString()
		if err != nil || id != "e2e:ant" {
			t.Errorf("ent id field = %q, %v; want the raw id", id, err)
		}
		if ttl, err := c.Do(ctx, c.B().Pttl().Key("wwz:ent:{key:e2e-oai}").Build()).AsInt64(); err != nil || ttl < 29*24*3600*1000 || ttl > 30*24*3600*1000 {
			t.Errorf("ent pttl = %d, %v; want ~30 d", ttl, err)
		}
		if ttl, err := c.Do(ctx, c.B().Pttl().Key("wwz:tb:{key:e2e-oai}").Build()).AsInt64(); err != nil || ttl <= 0 || ttl > 120000 {
			t.Errorf("tb pttl = %d, %v; want max(120 s, 2 x burst/rps)", ttl, err)
		}
		if n, err := c.Do(ctx, c.B().Zscore().Key("wwz:idx:ent").Member("key:e2e:ant").Build()).AsFloat64(); err != nil || n <= 0 {
			t.Errorf("idx:ent member key:e2e:ant (raw id) score = %v, %v", n, err)
		}
	})

	t.Run("EventsMatchAsyncAPI", func(t *testing.T) {
		entries, err := c.Do(ctx, c.B().Xrange().Key("wwz:ev:stream").Start("-").End("+").Build()).AsXRange()
		if err != nil {
			t.Fatal(err)
		}
		types := map[string]int{}
		enums := map[string]bool{"request": true, "error": true, "block": true, "unblock": true, "ban": true, "unban": true, "strike": true}
		kinds := map[string]bool{"ip": true, "key": true, "user": true, "project": true, "account": true}
		for _, e := range entries {
			if len(e.FieldValues) != 1 || e.FieldValues["d"] == "" {
				t.Errorf("entry %s: want a single field d, got %v", e.ID, e.FieldValues)
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(e.FieldValues["d"]), &ev); err != nil {
				t.Errorf("entry %s: %v", e.ID, err)
				continue
			}
			for k := range ev {
				if !asyncAPIEvent[k] {
					t.Errorf("entry %s: property %q not in asyncapi Event", e.ID, k)
				}
			}
			typ, _ := ev["type"].(string)
			types[typ]++
			if !enums[typ] {
				t.Errorf("entry %s: type %q", e.ID, typ)
			}
			if s, _ := ev["instance"].(string); s == "" {
				t.Errorf("entry %s: instance missing", e.ID)
			}
			if s, _ := ev["t"].(string); s == "" {
				t.Errorf("entry %s: t missing", e.ID)
			} else if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
				t.Errorf("entry %s: t %q: %v", e.ID, s, err)
			}
			ent, _ := ev["entity"].(map[string]any)
			if k, _ := ent["kind"].(string); !kinds[k] {
				t.Errorf("entry %s: entity %v", e.ID, ev["entity"])
			}
			f, _ := ev["fields"].(map[string]any)
			for k := range f {
				if !asyncAPIFields[k] {
					t.Errorf("entry %s: fields.%s not in asyncapi Event.fields", e.ID, k)
				}
			}
			switch typ {
			case "request", "error":
				if s, _ := ev["request_id"].(string); s == "" || ev["driver"] == nil || ev["status"] == nil {
					t.Errorf("entry %s: request without request_id/driver/status: %v", e.ID, ev)
				}
				for _, k := range []string{"duration_ms", "prompt_tokens", "output_tokens"} {
					if _, ok := f[k]; !ok {
						t.Errorf("entry %s: fields.%s missing: %v", e.ID, k, f)
					}
				}
				// task 066: entity is the key, and key/ip are explicit.
				if k, _ := ev["key"].(string); k != "e2e-oai" && k != "e2e:ant" {
					t.Errorf("entry %s: key %v, want the key label", e.ID, ev["key"])
				} else if ent["kind"] != "key" || ent["id"] != k {
					t.Errorf("entry %s: entity %v, want the key %q", e.ID, ent, k)
				}
				if s, _ := ev["ip"].(string); s == "" {
					t.Errorf("entry %s: ip missing: %v", e.ID, ev)
				}
				// task 075: account and the usage fields of the panel.
				if typ == "request" {
					usageFields(t, e.ID, ev, f)
				}
			case "block":
				if f["reason"] != "t017" || f["by"] != "admin" {
					t.Errorf("entry %s: block fields %v", e.ID, f)
				}
				if s, _ := f["until"].(string); s == "" {
					t.Errorf("entry %s: block until %v", e.ID, f["until"])
				} else if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
					t.Errorf("entry %s: block until %q: %v", e.ID, s, err)
				}
			case "ban":
				if f["reason"] != "t017" || f["by"] != "admin" {
					t.Errorf("entry %s: ban fields %v", e.ID, f)
				}
			}
		}
		// The 429s above are not required as "error" events here: limit
		// denials skip Store.Finish (gap reported by task 017).
		if types["request"] != 2 || types["block"] != 1 || types["unblock"] != 1 || types["ban"] != 1 || types["unban"] != 1 {
			t.Errorf("event types = %v; want 2 request, 1 each of block, unblock, ban, unban", types)
		}
	})
}

// metric returns the value of an unlabelled series in the /metrics text.
// usageFields checks the fields the panel's usage screen reads from a served
// request event (asyncapi 0.3.0, task 075): account at the top level,
// reasoning_tokens and cached_tokens as integers, cost_usd as a positive
// number and, for the OpenAI request with tool_choice required, tool_calls as
// the list of names.
func usageFields(t *testing.T, id string, ev, f map[string]any) {
	t.Helper()
	wantAcct := map[string]string{"e2e-oai": "org-e2e", "e2e:ant": "acct-e2e:ant"}[ev["key"].(string)]
	if ev["account"] != wantAcct {
		t.Errorf("entry %s: account %v, want %q", id, ev["account"], wantAcct)
	}
	for _, k := range []string{"reasoning_tokens", "cached_tokens"} {
		if n, ok := f[k].(float64); !ok || n < 0 || n != float64(int64(n)) {
			t.Errorf("entry %s: fields.%s = %v, want an integer", id, k, f[k])
		}
	}
	if c, _ := f["cost_usd"].(float64); c <= 0 {
		t.Errorf("entry %s: fields.cost_usd = %v", id, f["cost_usd"])
	}
	if ev["key"] != "e2e-oai" {
		return
	}
	tools, _ := f["tool_calls"].([]any)
	if len(tools) == 0 {
		t.Errorf("entry %s: fields.tool_calls = %v", id, f["tool_calls"])
	}
	for _, n := range tools {
		if n != "read_file" {
			t.Errorf("entry %s: fields.tool_calls = %v, want only read_file", id, tools)
		}
	}
}

func metric(t *testing.T, admin, name string) string {
	t.Helper()
	_, _, body := do(t, "GET", url(admin, "/metrics"), nil, "")
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			return v
		}
	}
	return ""
}

func waitMetric(t *testing.T, admin, name, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for metric(t, admin, name) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s = %q, want %q", name, metric(t, admin, name), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestServeValkeyPolicies: cfg:policies read at boot, a tier assigned on one
// instance applied by the other through cfg:changed, and an invalid version
// refused and counted (ADR 0025, task 052).
func TestServeValkeyPolicies(t *testing.T) {
	addr, c := testValkeyDB(t)
	ctx := context.Background()
	if err := c.Do(ctx, c.B().Hset().Key("wwz:cfg:policies").FieldValue().FieldValue("tier:gold", `{"rpm": 1}`).FieldValue("version", "1").Build()).Error(); err != nil {
		t.Fatal(err)
	}
	cleanEnv(t)
	t.Setenv("WWZ_VALKEY_ADDR", addr)
	path := filepath.Join(t.TempDir(), "wwz.yaml")
	writeFile(t, path, `
server: {auth: keys, seed: 5}
store: {read_timeout: 1s}
keys:
  - {key: sk-pol-1, label: pol-1}
  - {key: sk-pol-2, label: pol-2}
`)
	a := startServe(t, "-config", path)
	b := startServe(t, "-config", path)
	for _, in := range []*instance{a, b} {
		if v := metric(t, in.a.Admin, "wwz_policies_version"); v != "1" {
			t.Fatalf("policies version at boot = %q", v)
		}
	}
	code, _, body := do(t, "POST", url(a.a.Admin, "/admin/entities/key/pol-1/tier"), map[string]string{"Content-Type": "application/json"}, `{"tier":"gold"}`)
	if code != 200 || !strings.Contains(body, `"tier":"gold"`) {
		t.Fatalf("tier: %d %s", code, body)
	}
	waitMetric(t, b.a.Admin, "wwz_policies_version", "2")
	req := `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`
	call := func(in *instance, key string) int {
		t.Helper()
		code, _, body := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"}, req)
		if code != 200 && code != 429 {
			t.Fatalf("%s: %d %s", key, code, body)
		}
		return code
	}
	if c1, c2 := call(b, "sk-pol-1"), call(b, "sk-pol-1"); c1 != 200 || c2 != 429 {
		t.Fatalf("gold tier on the other instance: %d then %d", c1, c2)
	}
	if c1, c2 := call(b, "sk-pol-2"), call(b, "sk-pol-2"); c1 != 200 || c2 != 200 {
		t.Fatalf("pol-2 has no tier override: %d then %d", c1, c2)
	}

	// The panel publishes an invalid tier: refused and counted everywhere.
	res := c.DoMulti(ctx,
		c.B().Multi().Build(),
		c.B().Hset().Key("wwz:cfg:policies").FieldValue().FieldValue("tier:gold", `{"rpm": -1}`).Build(),
		c.B().Hincrby().Key("wwz:cfg:policies").Field("version").Increment(1).Build(),
		c.B().Exec().Build(),
	)
	if err := res[3].Error(); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, c.B().Publish().Channel("wwz:cfg:changed").Message(`{"version": 3}`).Build()).Error(); err != nil {
		t.Fatal(err)
	}
	for _, in := range []*instance{a, b} {
		waitMetric(t, in.a.Admin, "wwz_policies_errors_total", "1")
		if v := metric(t, in.a.Admin, "wwz_policies_version"); v != "2" {
			t.Fatalf("version after a refused one = %q", v)
		}
	}
	if !strings.Contains(b.logs.String(), "policies refused") {
		t.Fatalf("logs:\n%s", b.logs)
	}
}

// TestServeValkeyPreviousResponseAcrossInstances (spec 004, task 385): two
// `wwz serve` on the same Valkey; instance A stores a response and instance B
// chains it with previous_response_id. The recorded call
// (previous_response_id fixture) counts 27 input tokens for "Repeat it." on
// top of "Say hi in two words." answered "Hello there!".
func TestServeValkeyPreviousResponseAcrossInstances(t *testing.T) {
	addr, _ := testValkeyDB(t)
	cleanEnv(t)
	t.Setenv("WWZ_VALKEY_ADDR", addr)
	path := filepath.Join(t.TempDir(), "wwz.yaml")
	writeFile(t, path, `
server: {auth: keys, seed: 3}
store: {read_timeout: 1s}
keys:
  - {key: sk-resp, label: resp}
models:
  - {id: gpt-4o-mini, version: gpt-4o-mini-2024-07-18, protocols: [openai], latency: fast}
`)
	a := startServe(t, "-config", path)
	b := startServe(t, "-config", path)
	h := map[string]string{"Authorization": "Bearer sk-resp", "Content-Type": "application/json", "x-wwz-response": "Hello there!"}

	code, _, body := do(t, "POST", url(a.a.Data, "/v1/responses"), h, `{"model":"gpt-4o-mini","input":"Say hi in two words.","max_output_tokens":16,"store":true}`)
	var first struct{ ID string }
	if err := json.Unmarshal([]byte(body), &first); code != 200 || err != nil || !strings.HasPrefix(first.ID, "resp_") {
		t.Fatalf("A: %d %s", code, body)
	}
	code, _, body = do(t, "POST", url(b.a.Data, "/v1/responses"), h, `{"model":"gpt-4o-mini","previous_response_id":"`+first.ID+`","input":"Repeat it.","max_output_tokens":16,"store":false}`)
	var second struct {
		PreviousResponseID string `json:"previous_response_id"`
		Usage              struct {
			InputTokens int `json:"input_tokens"`
		}
	}
	if err := json.Unmarshal([]byte(body), &second); code != 200 || err != nil || second.PreviousResponseID != first.ID || second.Usage.InputTokens != 27 {
		t.Fatalf("B: %d (input_tokens %d) %s", code, second.Usage.InputTokens, body)
	}
	// An id nobody stored is still the real 400 on both instances.
	code, _, body = do(t, "POST", url(b.a.Data, "/v1/responses"), h, `{"model":"gpt-4o-mini","previous_response_id":"resp_nope","input":"x"}`)
	if code != 400 || !strings.Contains(body, "previous_response_not_found") {
		t.Fatalf("unknown previous_response_id: %d %s", code, body)
	}
}

// TestServeValkeyTierAndResetAcrossKeysAndInstances (spec 012, task 385): a
// manual tier set through key A (account acme) on instance a limits key B of
// the same account on instance b; an entity reset through the admin HTTP does
// not zero the other entities.
func TestServeValkeyTierAndResetAcrossKeysAndInstances(t *testing.T) {
	addr, _ := testValkeyDB(t)
	cleanEnv(t)
	t.Setenv("WWZ_VALKEY_ADDR", addr)
	path := filepath.Join(t.TempDir(), "wwz.yaml")
	writeFile(t, path, `
server: {auth: keys, seed: 5}
store: {read_timeout: 1s}
tiers:
  gold: {limits: {rpm: 1}}
keys:
  - {key: sk-ta, label: tier-a, account: acme}
  - {key: sk-tb, label: tier-b, account: acme}
  - {key: sk-ra, label: reset-a, limits: {rpm: 1}}
  - {key: sk-rb, label: reset-b, limits: {rpm: 1}}
`)
	a := startServe(t, "-config", path)
	b := startServe(t, "-config", path)
	call := func(in *instance, key string) int {
		t.Helper()
		code, _, body := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"},
			`{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
		if code != 200 && code != 429 {
			t.Fatalf("%s: %d %s", key, code, body)
		}
		return code
	}
	jsonH := map[string]string{"Content-Type": "application/json"}

	t.Run("TierThroughKeyAppliesToAccountKeys", func(t *testing.T) {
		if c1, c2 := call(b, "sk-tb"), call(b, "sk-tb"); c1 != 200 || c2 != 200 {
			t.Fatalf("default tier has no limit: %d then %d", c1, c2)
		}
		code, _, body := do(t, "POST", url(a.a.Admin, "/admin/entities/key/tier-a/tier"), jsonH, `{"tier":"gold"}`)
		if code != 200 || !strings.Contains(body, `"tier":"gold"`) {
			t.Fatalf("tier: %d %s", code, body)
		}
		deadline := time.Now().Add(5 * time.Second)
		for metric(t, b.a.Admin, "wwz_policies_version") == "" || metric(t, b.a.Admin, "wwz_policies_version") == "0" {
			if time.Now().After(deadline) {
				t.Fatalf("instance b never applied the policies version: %q", metric(t, b.a.Admin, "wwz_policies_version"))
			}
			time.Sleep(20 * time.Millisecond)
		}
		if c1, c2 := call(b, "sk-tb"), call(b, "sk-tb"); c1 != 200 || c2 != 429 {
			t.Fatalf("key B (same account) on instance b after the tier: %d then %d, want 200 then 429", c1, c2)
		}
		if c := call(a, "sk-ta"); c != 429 {
			t.Fatalf("key A shares the account bucket: %d, want 429", c)
		}
	})

	t.Run("ResetIsolatedToTheEntity", func(t *testing.T) {
		if c := call(a, "sk-ra"); c != 200 {
			t.Fatalf("reset-a first: %d", c)
		}
		if c := call(b, "sk-rb"); c != 200 {
			t.Fatalf("reset-b first: %d", c)
		}
		if c := call(b, "sk-ra"); c != 429 {
			t.Fatalf("reset-a second: %d", c)
		}
		if c := call(a, "sk-rb"); c != 429 {
			t.Fatalf("reset-b second: %d", c)
		}
		code, _, body := do(t, "POST", url(b.a.Admin, "/admin/entities/key/reset-a/reset"), jsonH, ``)
		if code != 200 {
			t.Fatalf("reset: %d %s", code, body)
		}
		if c := call(a, "sk-ra"); c != 200 {
			t.Fatalf("reset-a after its reset: %d, want 200", c)
		}
		if c := call(b, "sk-rb"); c != 429 {
			t.Fatalf("reset-b after the reset of reset-a: %d, want 429 (untouched)", c)
		}
		if c := call(a, "sk-tb"); c != 429 {
			t.Fatalf("account acme after the reset of reset-a: %d, want 429 (untouched)", c)
		}
	})
}
