package azure

import (
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// TestDeploymentLimits: RPM derived from TPM per model family (spec 006
// 16.6) and the 1 s / 10 s evaluation window.
func TestDeploymentLimits(t *testing.T) {
	d := &Driver{}
	cases := []struct {
		model   config.ModelConfig
		in      config.Limits
		rpm     int
		window  time.Duration
		comment string
	}{
		{config.ModelConfig{ID: "gpt-4o-sim"}, config.Limits{TPM: 100000, RPM: 5, RPS: 3, Burst: 9}, 600, time.Second, "chat: 6 per 1000, tier rpm and rps dropped"},
		{config.ModelConfig{ID: "x", Version: "o3-mini-2025-01-31"}, config.Limits{TPM: 100000}, 10, 10 * time.Second, "o3-mini: 1 per 10000"},
		{config.ModelConfig{ID: "o1-mini"}, config.Limits{TPM: 600000}, 60, time.Second, "o1-mini"},
		{config.ModelConfig{ID: "o3-pro"}, config.Limits{TPM: 10000}, 1, 10 * time.Second, "o3-pro"},
		{config.ModelConfig{ID: "o4-mini"}, config.Limits{TPM: 30000}, 30, 10 * time.Second, "o4-mini: 1 per 1000"},
		{config.ModelConfig{ID: "o3"}, config.Limits{TPM: 1000}, 1, 10 * time.Second, "o3"},
		{config.ModelConfig{ID: "o1"}, config.Limits{TPM: 600000}, 100, time.Second, "o1: 1 per 6000"},
		{config.ModelConfig{ID: "gpt-4o-sim"}, config.Limits{TPM: 10}, 1, 10 * time.Second, "at least 1"},
		{config.ModelConfig{ID: "gpt-4o-sim"}, config.Limits{RPM: 120}, 120, time.Second, "no tpm: tier rpm stays"},
		{config.ModelConfig{ID: "gpt-4o-sim"}, config.Limits{RPS: 5}, 0, 0, "no tpm, no rpm: no window"},
	}
	for _, c := range cases {
		l, w := d.DeploymentLimits(&c.model, c.in)
		if l.RPM != c.rpm || w != c.window || l.RPS != 0 || l.Burst != 0 || l.TPM != c.in.TPM {
			t.Errorf("%s: rpm %d window %v rps %d burst %d, want rpm %d window %v", c.comment, l.RPM, w, l.RPS, l.Burst, c.rpm, c.window)
		}
	}
}

// TestResponseFilters: content_filters of a Responses object, the prompt
// always, the completion only with text.
func TestResponseFilters(t *testing.T) {
	msg := func(parts ...any) oai.Obj { return oai.O("type", "message", "content", parts) }
	cases := []struct {
		output []any
		n      int
	}{
		{nil, 1},
		{[]any{oai.O("type", "function_call")}, 1},
		{[]any{msg(oai.O("type", "output_text", "text", ""))}, 1},
		{[]any{msg(oai.O("type", "output_text", "text", "hi"))}, 2},
		{[]any{msg(oai.O("type", "refusal", "refusal", "no"))}, 2},
	}
	for i, c := range cases {
		o := shape(oai.PartResponse, oai.O("id", "resp_1", "output", c.output))
		v, _ := o.Get("content_filters")
		list, _ := v.([]any)
		if len(list) != c.n {
			t.Errorf("case %d: %d entries, want %d", i, len(list), c.n)
			continue
		}
		p := list[0].(oai.Obj)
		if st, _ := p.Get("source_type"); st != "prompt" {
			t.Errorf("case %d: first entry %v", i, p)
		}
		if c.n == 2 {
			cpl := list[1].(oai.Obj)
			res, _ := cpl.Get("content_filter_results")
			if st, _ := cpl.Get("source_type"); st != "completion" || !hasKey(res.(oai.Obj), "protected_material_text") {
				t.Errorf("case %d: completion entry %v", i, cpl)
			}
		}
	}
}

func hasKey(o oai.Obj, k string) bool {
	_, ok := o.Get(k)
	return ok
}

// TestModelsVersionOK: the api-version of the models routes.
func TestModelsVersionOK(t *testing.T) {
	cases := []struct {
		path, query string
		ok          bool
	}{
		{"", "", true},
		{"/openai/models", "", false},
		{"/openai/models", "api-version=2024-10-21", true},
		{"/openai/models/gpt-4o-sim", "api-version=bogus", false},
		{"/openai/v1/models", "", true},
		{"/openai/v1/models", "api-version=preview", true},
		{"/openai/v1/models", "api-version=2024-10-21", false},
	}
	for _, c := range cases {
		req := &core.Request{Headers: map[string]string{"x-wwz-path": c.path, driver.QueryHeader: c.query}}
		if got := modelsVersionOK(req); got != c.ok {
			t.Errorf("%s?%s: %v, want %v", c.path, c.query, got, c.ok)
		}
	}
	if !modelsVersionOK(nil) {
		t.Error("nil request")
	}
}

func TestE2EModelsAPIVersion(t *testing.T) {
	base := startWWZ(t, e2eConfig)
	for _, c := range []struct {
		url    string
		status int
	}{
		{"/azure/openai/models", 404},
		{"/azure/openai/models?api-version=2019-01-01", 404},
		{"/azure/openai/models/gpt-4o-sim", 404},
		{"/azure/openai/models?api-version=2025-04-01-preview", 200},
		{"/azure/openai/v1/models?api-version=2024-10-21", 404},
		{"/azure/openai/v1/models/gpt-4o-sim?api-version=v1", 200},
	} {
		r := do(t, "GET", base+c.url, key("az-key"), "")
		if r.status != c.status {
			t.Errorf("%s: status %d, want %d: %s", c.url, r.status, c.status, r.body)
			continue
		}
		if c.status == 404 && (errorCode(t, r) != "404" || r.header.Get("apim-request-id") == "") {
			t.Errorf("%s: 404 %v %s", c.url, r.header, r.body)
		}
	}
	// The key comes first.
	if r := do(t, "GET", base+"/azure/openai/models", key("wrong"), ""); r.status != 401 {
		t.Errorf("bad key without api-version: %d %s", r.status, r.body)
	}
}

func TestE2EResponsesValidation(t *testing.T) {
	base := startWWZ(t, e2eConfig)
	url := base + "/azure/openai/v1/responses"
	cases := []struct {
		name, query, body string
		status            int
		code, param       any
	}{
		{"bad json", "", `{"model":`, 400, "invalid_json", nil},
		{"messages", "", `{"model":"chat-prod","messages":[]}`, 400, "unsupported_parameter", nil},
		{"no model", "", `{"input":"hi"}`, 400, "missing_required_parameter", "model"},
		{"no input", "", `{"model":"chat-prod"}`, 400, "missing_required_parameter", "input"},
		{"unknown field", "?api-version=preview", `{"model":"chat-prod","input":"hi","foo":1}`, 400, "unknown_parameter", "foo"},
		{"max_output_tokens", "", `{"model":"chat-prod","input":"hi","max_output_tokens":8}`, 400, "integer_below_min_value", "max_output_tokens"},
		{"strict tool", "", `{"model":"chat-prod","input":"hi","tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object","properties":{"a":{}}}}]}`, 400, "invalid_function_parameters", "tools[0].parameters"},
		{"unknown deployment", "", `{"model":"nope","input":"hi"}`, 404, "DeploymentNotFound", nil},
		{"conversation", "", `{"model":"chat-prod","input":"hi","conversation":"conv_1"}`, 404, "404", nil},
		{"prompt", "", `{"model":"chat-prod","input":"hi","prompt":{"id":"p"}}`, 400, "unsupported_parameter", "prompt"},
		{"bad api-version before the body", "?api-version=2024-10-21", `{"model":`, 404, "404", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := do(t, "POST", url+c.query, key("az-key"), c.body)
			if r.status != c.status {
				t.Fatalf("status %d, want %d: %s", r.status, c.status, r.body)
			}
			e, _ := r.json(t)["error"].(map[string]any)
			if e["code"] != c.code {
				t.Errorf("code %v, want %v: %s", e["code"], c.code, r.body)
			}
			if c.param != nil && e["param"] != c.param {
				t.Errorf("param %v, want %v: %s", e["param"], c.param, r.body)
			}
			if r.header.Get("apim-request-id") == "" {
				t.Errorf("no apim-request-id: %v", r.header)
			}
		})
	}
}

func TestE2EResponsesContentFilters(t *testing.T) {
	base := startWWZ(t, e2eConfig)
	r := do(t, "POST", base+"/azure/openai/v1/responses", key("az-key"), `{"model":"chat-prod","input":"hi"}`)
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	cf, _ := r.json(t)["content_filters"].([]any)
	if len(cf) != 2 {
		t.Fatalf("content_filters %v", cf)
	}
	p, _ := cf[0].(map[string]any)
	res, _ := p["content_filter_results"].(map[string]any)
	if p["source_type"] != "prompt" || p["blocked"] != false || res["jailbreak"] == nil {
		t.Errorf("prompt entry %v", p)
	}
	if c, _ := cf[1].(map[string]any); c["source_type"] != "completion" {
		t.Errorf("completion entry %v", c)
	}
	r = do(t, "POST", base+"/azure/openai/v1/responses", key("az-key"), `{"model":"chat-prod","input":"hi","stream":true}`)
	i := strings.Index(r.body, "event: response.completed")
	if r.status != 200 || i < 0 || !strings.Contains(r.body[i:], `"content_filters":[{"source_type":"prompt"`) {
		t.Errorf("stream: %d %s", r.status, r.body)
	}
	if strings.Contains(r.body[:max(i, 0)], "content_filters") {
		t.Errorf("content_filters before the closing event: %s", r.body)
	}
}

// deploymentConfig: an account whose tier gives 1000 TPM per deployment,
// so 6 RPM evaluated over 10 s (one request).
const deploymentConfig = `
server: {listen: "127.0.0.1:0", auth: keys, seed: 7}
store: {kind: memory}
logging: {level: error}
drivers:
  - {name: azure, prefix: /azure}
tiers:
  s0: {limits: {tpm: 1000, rpm: 500, spend_monthly_usd: 100}}
keys:
  - {key: az-a, label: a, account: contoso, tier: s0}
  - {key: az-b, label: b, account: contoso}
profiles:
  latency: {zero: {}}
  generation:
    text: {mode: lorem, output_tokens: {dist: fixed, value: 12}}
models:
  - {id: dep-one, latency: zero, generation: text}
  - {id: dep-two, latency: zero, generation: text}
`

func TestE2EDeploymentLimit(t *testing.T) {
	base := startWWZ(t, deploymentConfig)
	chat := func(k, dep string) reply {
		return do(t, "POST", base+"/azure/openai/deployments/"+dep+"/chat/completions?api-version=2024-10-21", key(k), `{"max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`)
	}
	r := chat("az-a", "dep-one")
	if r.status != 200 {
		t.Fatalf("first: %d %s", r.status, r.body)
	}
	if r.header.Get("x-ratelimit-limit-requests") != "6" || r.header.Get("x-ratelimit-limit-tokens") != "1000" {
		t.Errorf("headers: %v", r.header)
	}
	// Another key of the same account, same deployment: the window holds
	// one request.
	r = chat("az-b", "dep-one")
	if r.status != 429 || !strings.Contains(r.body, "have exceeded call rate limit") || r.header.Get("retry-after-ms") == "" {
		t.Fatalf("second: %d %v %s", r.status, r.header, r.body)
	}
	if r.header.Get("x-ratelimit-limit-requests") != "6" {
		t.Errorf("429 x-ratelimit-limit-requests = %q", r.header.Get("x-ratelimit-limit-requests"))
	}
	// Another deployment of the account has its own limit.
	if r = chat("az-a", "dep-two"); r.status != 200 {
		t.Errorf("other deployment: %d %s", r.status, r.body)
	}
}
