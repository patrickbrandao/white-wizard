package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// Task 254: OpenCode Go lists its models without a key (an invalid one is
// still a 401), answers GET /v1/models/{id} with 404 whatever the key, and
// GET /v1/usage reports the account's quota windows with the gateway's
// AuthError when the key is missing or invalid.

const openCodeUsageCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  generation:
    fixed: {mode: random, output_tokens: {dist: uniform, min: 100, max: 100}}
models:
  - {id: glm-5.2, protocols: [openai], latency: fast, generation: fixed, pricing: {input_per_m: 0.001, output_per_m: 15000}, catalogs: {opencode: {}, opencode-go: {allowance_factor: 1}}, capabilities: {reasoning: false}}
accounts:
  - {id: ws-go, limits: {quota_usd: {monthly_usd: 60, rolling_5h_pct: 20, weekly_pct: 50}}}
keys:
  - {key: oc_sk_go, label: go, account: ws-go}
  - {key: oc_sk_free, label: free}
`

func TestOpenCodeModelsWithoutKey(t *testing.T) {
	e := newEngine(t, openCodeUsageCfg)
	const path = "/inference/go/v1/models"
	for _, c := range []struct {
		name   string
		hdr    map[string]string
		status int
	}{
		{"no key", nil, 200},
		{"valid key", map[string]string{"Authorization": "Bearer oc_sk_go"}, 200},
		{"invalid key", map[string]string{"Authorization": "Bearer sk-invalid-wwz-drift"}, 401},
	} {
		w := call(t, e, "opencode", path, c.hdr, "")
		if w.StatusCode() != c.status {
			t.Errorf("%s: %d %s, want %d", c.name, w.StatusCode(), w.Body(), c.status)
		}
		if c.status == 200 && !strings.Contains(string(w.Body()), `"id":"glm-5.2"`) {
			t.Errorf("%s: listing without the model: %s", c.name, w.Body())
		}
		if c.status == 401 && string(w.Body()) != `{"error":{"type":"server_error","message":"Invalid credential"}}` {
			t.Errorf("%s: %s", c.name, w.Body())
		}
	}
	// GET /v1/models/{id}: 404 with an empty body, with or without a key.
	drv, err := driver.New("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"", "Bearer oc_sk_go", "Bearer sk-invalid"} {
		h := driver.MapHeaders{}
		if auth != "" {
			h["authorization"] = auth
		}
		w := core.NewMemWriter()
		e.Handle(context.Background(), engine.Input{Driver: drv, Kind: driver.KindModel, Method: "GET", Path: path + "/glm-5.2", PathParam: "glm-5.2", Headers: h, ClientIP: "127.0.0.1"}, w)
		if w.StatusCode() != 404 || len(w.Body()) != 0 {
			t.Errorf("model with %q: %d %s", auth, w.StatusCode(), w.Body())
		}
	}
}

func TestOpenCodeUsageQuota(t *testing.T) {
	e := newEngine(t, openCodeUsageCfg)
	const path = "/zen/go/v1/usage"
	usage := func(key string) (int, map[string]int) {
		t.Helper()
		var hdr map[string]string
		if key != "" {
			hdr = map[string]string{"Authorization": "Bearer " + key}
		}
		w := call(t, e, "opencode", path, hdr, "")
		if w.StatusCode() != 200 {
			if string(w.Body()) != `{"type":"error","error":{"type":"AuthError","message":"Unauthorized"}}` {
				t.Errorf("usage %q: %d %s", key, w.StatusCode(), w.Body())
			}
			return w.StatusCode(), nil
		}
		var out struct {
			Usage map[string]struct {
				Percent int `json:"percent"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(w.Body(), &out); err != nil {
			t.Fatalf("%s: %v", w.Body(), err)
		}
		pct := map[string]int{}
		for k, v := range out.Usage {
			pct[k] = v.Percent
		}
		return 200, pct
	}
	for _, key := range []string{"", "sk-invalid-wwz-drift"} {
		if status, _ := usage(key); status != 401 {
			t.Errorf("usage with %q: %d, want 401", key, status)
		}
	}
	if _, pct := usage("oc_sk_go"); pct["weekly"] != 0 || pct["monthly"] != 0 {
		t.Fatalf("fresh account: %v", pct)
	}
	// 100 output tokens at $15000/M: $1.50 per call, 4 calls = $6, i.e.
	// 50% of the $12 rolling 5 h window (task 296), 20% of the $30 week
	// and 10% of the $60 month.
	for range 4 {
		w := call(t, e, "opencode", "/zen/go/v1/chat/completions", map[string]string{"Authorization": "Bearer oc_sk_go"},
			`{"model":"glm-5.2","messages":[{"role":"user","content":"oi"}]}`)
		if w.StatusCode() != 200 {
			t.Fatalf("chat: %d %s", w.StatusCode(), w.Body())
		}
	}
	if _, pct := usage("oc_sk_go"); pct["weekly"] != 20 || pct["monthly"] != 10 || pct["rolling"] != 50 {
		t.Errorf("after $6: %v, want rolling 50, weekly 20, monthly 10", pct)
	}
	if _, pct := usage("oc_sk_free"); pct["weekly"] != 0 || pct["monthly"] != 0 {
		t.Errorf("key without account: %v", pct)
	}
}
