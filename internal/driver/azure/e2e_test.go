package azure

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

// e2eConfig mounts azure at /azure and at the root (the paths are Azure's
// own), with a deployment reachable by id and by alias.
const e2eConfig = `
server: {listen: "127.0.0.1:0", auth: keys, seed: 7}
store: {kind: memory}
logging: {level: error}
drivers:
  - {name: azure, prefix: /azure}
  - {name: azure}
keys:
  - {key: az-key}
  - {key: az-rl, models: [m-rl], limits: {rpm: 1}}
profiles:
  latency: {zero: {}}
  generation:
    text: {mode: lorem, output_tokens: {dist: fixed, value: 12}}
models:
  - id: gpt-4o-sim
    version: gpt-4o-2024-08-06
    aliases: [chat-prod]
    latency: zero
    generation: text
  - id: m-rl
    latency: zero
    generation: text
`

// startWWZ serves cfg in this process and returns its base URL.
func startWWZ(t *testing.T, cfg string) string {
	t.Helper()
	c, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := engine.NewMetrics(metrics.New(c.Metrics.MaxSeries), c.Metrics.Buckets)
	eng := engine.New(c, limits.NewMemory(), scenario.NewRegistry(), m, log)
	srv, err := server.New(eng, server.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	return "http://" + ln.Addr().String()
}

type reply struct {
	status int
	header http.Header
	body   string
}

func (r reply) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("body %q: %v", r.body, err)
	}
	return m
}

func do(t *testing.T, method, url string, hdr map[string]string, body string) reply {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return reply{res.StatusCode, res.Header, string(b)}
}

func key(k string) map[string]string {
	return map[string]string{"api-key": k, "content-type": "application/json"}
}

const hello = `{"messages":[{"role":"user","content":"hello"}]}`

func errorCode(t *testing.T, r reply) any {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	return e["code"]
}

func TestE2EDeploymentChat(t *testing.T) {
	base := startWWZ(t, e2eConfig)
	for _, p := range []string{"/azure", ""} {
		r := do(t, "POST", base+p+"/openai/deployments/chat-prod/chat/completions?api-version=2024-10-21", key("az-key"), hello)
		if r.status != 200 {
			t.Fatalf("%q: status %d: %s", p, r.status, r.body)
		}
		m := r.json(t)
		if m["model"] != "gpt-4o-2024-08-06" {
			t.Errorf("model = %v, want the version, not the deployment", m["model"])
		}
		if !strings.HasPrefix(m["id"].(string), "chatcmpl-") {
			t.Errorf("id = %v", m["id"])
		}
		if _, ok := m["prompt_filter_results"]; !ok {
			t.Errorf("no prompt_filter_results: %s", r.body)
		}
		if len(r.header.Get("apim-request-id")) != 36 {
			t.Errorf("apim-request-id = %q", r.header.Get("apim-request-id"))
		}
		if r.header.Get("x-request-id") != "" {
			t.Errorf("x-request-id = %q, Azure sends apim-request-id", r.header.Get("x-request-id"))
		}
	}
}

func TestE2EDeploymentStream(t *testing.T) {
	base := startWWZ(t, e2eConfig)
	r := do(t, "POST", base+"/azure/openai/deployments/gpt-4o-sim/chat/completions?api-version=2025-04-01-preview", key("az-key"),
		`{"messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`)
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	frames := strings.Split(strings.TrimSpace(r.body), "\n\n")
	if !strings.HasPrefix(frames[0], `data: {"id":"","object":"","created":0,"model":"","prompt_filter_results":[`) {
		t.Errorf("first frame %q, want the prompt filter event", frames[0])
	}
	if frames[len(frames)-1] != "data: [DONE]" {
		t.Errorf("last frame %q", frames[len(frames)-1])
	}
	if !strings.Contains(frames[1], `"role":"assistant"`) || !strings.Contains(frames[1], `"content_filter_results":{}`) {
		t.Errorf("role chunk %q", frames[1])
	}
	if !strings.Contains(frames[2], `"content_filter_results":{"hate":`) {
		t.Errorf("text chunk %q without the completion results", frames[2])
	}
}

func TestE2ECompletions(t *testing.T) {
	base := startWWZ(t, e2eConfig)
	r := do(t, "POST", base+"/azure/openai/deployments/gpt-4o-sim/completions?api-version=2024-10-21", key("az-key"), `{"prompt":"hi","max_tokens":5}`)
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	m := r.json(t)
	if m["object"] != "text_completion" || !strings.HasPrefix(m["id"].(string), "cmpl-") {
		t.Errorf("body %s", r.body)
	}
	r = do(t, "POST", base+"/azure/openai/deployments/gpt-4o-sim/completions?api-version=2024-10-21", key("az-key"), `{"prompt":"hi","max_tokens":5,"stream":true}`)
	if r.status != 200 || !strings.Contains(r.body, `"id":"cmpl-`) {
		t.Errorf("stream: %d %s", r.status, r.body)
	}
}

func TestE2EV1(t *testing.T) {
	base := startWWZ(t, e2eConfig)
	bearer := map[string]string{"authorization": "Bearer az-key", "content-type": "application/json"}
	r := do(t, "POST", base+"/azure/openai/v1/chat/completions", bearer, `{"model":"chat-prod","messages":[{"role":"user","content":"hi"}]}`)
	if r.status != 200 || r.json(t)["model"] != "gpt-4o-2024-08-06" {
		t.Fatalf("v1 chat: %d %s", r.status, r.body)
	}
	r = do(t, "POST", base+"/azure/openai/v1/chat/completions?api-version=preview", bearer, `{"model":"gpt-4o-sim","messages":[{"role":"user","content":"hi"}]}`)
	if r.status != 200 {
		t.Fatalf("v1 preview: %d %s", r.status, r.body)
	}
	r = do(t, "POST", base+"/azure/openai/v1/responses", bearer, `{"model":"chat-prod","input":"hi"}`)
	if r.status != 200 {
		t.Fatalf("v1 responses: %d %s", r.status, r.body)
	}
	if m := r.json(t); m["object"] != "response" || !strings.HasPrefix(m["id"].(string), "resp_") {
		t.Errorf("responses body %s", r.body)
	}
	r = do(t, "POST", base+"/azure/openai/v1/responses", bearer, `{"model":"chat-prod","input":"hi","stream":true}`)
	if r.status != 200 || !strings.Contains(r.body, "response.completed") {
		t.Errorf("responses stream: %d %s", r.status, r.body)
	}
	r = do(t, "GET", base+"/azure/openai/v1/models", bearer, "")
	if r.status != 200 || r.json(t)["object"] != "list" {
		t.Errorf("v1 models: %d %s", r.status, r.body)
	}
}

func TestE2EErrors(t *testing.T) {
	base := startWWZ(t, e2eConfig)
	dep := base + "/azure/openai/deployments/"
	cases := []struct {
		name, url string
		hdr       map[string]string
		body      string
		status    int
		code      any
	}{
		{"no api-version", dep + "gpt-4o-sim/chat/completions", key("az-key"), hello, 404, "404"},
		{"unknown api-version", dep + "gpt-4o-sim/chat/completions?api-version=2019-01-01", key("az-key"), hello, 404, "404"},
		{"bad v1 api-version", base + "/azure/openai/v1/chat/completions?api-version=2024-10-21", key("az-key"), `{"model":"gpt-4o-sim","messages":[{"role":"user","content":"hi"}]}`, 404, "404"},
		{"embeddings", dep + "gpt-4o-sim/embeddings?api-version=2024-10-21", key("az-key"), `{"input":"x"}`, 404, "404"},
		{"unknown deployment", dep + "nope/chat/completions?api-version=2024-10-21", key("az-key"), hello, 404, "DeploymentNotFound"},
		{"bad key", dep + "gpt-4o-sim/chat/completions?api-version=2024-10-21", key("wrong"), hello, 401, "401"},
		{"no key", dep + "gpt-4o-sim/chat/completions?api-version=2024-10-21", map[string]string{"content-type": "application/json"}, hello, 401, "401"},
		{"unknown field", dep + "gpt-4o-sim/chat/completions?api-version=2024-10-21", key("az-key"), `{"messages":[{"role":"user","content":"hi"}],"foo":1}`, 400, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := do(t, "POST", c.url, c.hdr, c.body)
			if r.status != c.status {
				t.Fatalf("status %d, want %d: %s", r.status, c.status, r.body)
			}
			if got := errorCode(t, r); got != c.code {
				t.Errorf("code %v, want %v: %s", got, c.code, r.body)
			}
		})
	}
}

func TestE2ERateLimit(t *testing.T) {
	base := startWWZ(t, e2eConfig)
	url := base + "/azure/openai/deployments/m-rl/chat/completions?api-version=2024-10-21"
	var r reply
	for range 3 {
		if r = do(t, "POST", url, key("az-rl"), hello); r.status == 429 {
			break
		}
	}
	if r.status != 429 {
		t.Fatalf("status %d, want 429", r.status)
	}
	if errorCode(t, r) != "429" || r.header.Get("retry-after-ms") == "" || r.header.Get("retry-after") != "" {
		t.Errorf("429: headers %v body %s", r.header, r.body)
	}
	if !strings.Contains(r.body, "ChatCompletions_Create Operation under Azure OpenAI API version 2024-10-21 have exceeded call rate limit") {
		t.Errorf("429 text: %s", r.body)
	}
	if r.header.Get("x-ratelimit-limit-requests") != "1" {
		t.Errorf("x-ratelimit-limit-requests = %q", r.header.Get("x-ratelimit-limit-requests"))
	}
}

func TestE2EModels(t *testing.T) {
	base := startWWZ(t, e2eConfig)
	r := do(t, "GET", base+"/azure/openai/models?api-version=2024-10-21", key("az-key"), "")
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	data, _ := r.json(t)["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data %v", data)
	}
	r = do(t, "GET", base+"/azure/openai/models/gpt-4o-sim?api-version=2024-10-21", key("az-key"), "")
	if r.status != 200 || r.json(t)["id"] != "gpt-4o-sim" {
		t.Errorf("get: %d %s", r.status, r.body)
	}
	r = do(t, "GET", base+"/azure/openai/models/nope?api-version=2024-10-21", key("az-key"), "")
	if r.status != 404 {
		t.Errorf("get unknown: %d %s", r.status, r.body)
	}
}
