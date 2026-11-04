package server

import (
	"strings"
	"testing"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/gemini"
)

// Task 281 (ADR 0062, spec 006 16.3): OpenCode serves every model of its
// catalog on every route, whatever models[].protocols; the direct drivers
// keep refusing a model outside their dialect. The Gemini route exists
// only under /inference and /inference/go.
func TestOpenCodeEveryModelOnEveryRoute(t *testing.T) {
	h := start(t, fastCfg+`
models:
  - {id: deepseek-v4.1-flash, protocols: [openai], drivers: [opencode, openai], latency: fast}
drivers:
  - {name: opencode}
  - {name: anthropic, prefix: /anthropic}
  - {name: gemini, prefix: /gemini}
`, nil)
	oc := map[string]string{"Authorization": "Bearer wwz-key-01", "x-opencode-session": "ses_1"}
	const (
		chat     = `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
		messages = `{"model":"deepseek-v4.1-flash","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		gen      = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":8}}`
	)
	for _, c := range []struct {
		path, body string
		want       int
		has        string
	}{
		{"/inference/go/openai/v1/chat/completions", chat, 200, `"chat.completion"`},
		{"/inference/go/anthropic/v1/messages", messages, 200, `"type":"message"`},
		{"/zen/go/v1/messages", messages, 200, `"type":"message"`},
		{"/inference/go/google/v1beta/models/deepseek-v4.1-flash:generateContent", gen, 200, `"candidates"`},
		{"/inference/google/v1beta/models/deepseek-v4.1-flash:generateContent", gen, 200, `"candidates"`},
		// No Gemini route under /zen (ADR 0061).
		{"/zen/go/google/v1beta/models/deepseek-v4.1-flash:generateContent", gen, 404, ""},
		{"/zen/google/v1beta/models/deepseek-v4.1-flash:generateContent", gen, 404, ""},
	} {
		resp := h.do("POST", c.path, oc, c.body)
		if resp.StatusCode() != c.want || !strings.Contains(string(resp.Body()), c.has) {
			t.Errorf("POST %s: %d %s, want %d with %s", c.path, resp.StatusCode(), resp.Body(), c.want, c.has)
		}
	}
	// The Gemini route answers a model outside Vertex in the translated
	// form: no thoughtSignature (gen_gpt_model).
	resp := h.do("POST", "/inference/go/google/v1beta/models/deepseek-v4.1-flash:generateContent", oc, gen)
	if strings.Contains(string(resp.Body()), "thoughtSignature") {
		t.Errorf("translated form expected: %s", resp.Body())
	}
	// The direct drivers still refuse the dialect the model does not speak.
	if resp := h.do("POST", "/anthropic/v1/messages", map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}, messages); resp.StatusCode() != 404 {
		t.Errorf("anthropic: %d %s", resp.StatusCode(), resp.Body())
	}
	if resp := h.do("POST", "/gemini/v1beta/models/deepseek-v4.1-flash:generateContent", map[string]string{"x-goog-api-key": "wwz-key-01"}, gen); resp.StatusCode() != 404 {
		t.Errorf("gemini: %d %s", resp.StatusCode(), resp.Body())
	}
}

// Task 281: OpenCode answers an unknown model on Chat with a 400 and the
// metrics count the 400 the client got, not the kind's 404.
func TestOpenCodeUnknownModelCounted400(t *testing.T) {
	h := start(t, fastCfg+`
models:
  - {id: deepseek-v4.1-flash, latency: fast}
drivers: [{name: opencode}]
`, nil)
	oc := map[string]string{"Authorization": "Bearer wwz-key-01", "x-opencode-session": "ses_1"}
	resp := h.do("POST", "/inference/go/openai/v1/chat/completions", oc, `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode() != 400 || !strings.Contains(string(resp.Body()), "Model is unavailable.") {
		t.Fatalf("%d %s", resp.StatusCode(), resp.Body())
	}
	if n := h.eng.M.Requests.Value("opencode", "", "chat", "400"); n != 1 {
		t.Errorf("400 counted %v times", n)
	}
	if n := h.eng.M.Requests.Value("opencode", "", "chat", "404"); n != 0 {
		t.Errorf("404 counted %v times", n)
	}
}

// Task 281: OpenRouter's :free caps hold through the server, where the
// driver is wrapped by the validator (driver.As).
func TestOpenRouterFreeModelsThroughServer(t *testing.T) {
	h := start(t, fastCfg+`
models:
  - {id: "meta-llama/llama-3.3-70b-instruct:free", protocols: [openai], latency: fast}
  - {id: meta-llama/llama-3.3-70b-instruct, protocols: [openai], latency: fast}
keys:
  - {key: sk-or-v1-poor, label: poor, account: poor}
drivers:
  - {name: openrouter, prefix: /openrouter}
`, nil)
	auth := map[string]string{"Authorization": "Bearer sk-or-v1-poor"}
	body := `{"model":"meta-llama/llama-3.3-70b-instruct:free","messages":[{"role":"user","content":"hi"}]}`
	for i := range 20 {
		if resp := h.do("POST", "/openrouter/api/v1/chat/completions", auth, body); resp.StatusCode() != 200 {
			t.Fatalf("free request %d: %d %s", i+1, resp.StatusCode(), resp.Body())
		}
	}
	resp := h.do("POST", "/openrouter/api/v1/chat/completions", auth, body)
	if resp.StatusCode() != 429 || !strings.Contains(string(resp.Body()), "free-models-per-min") {
		t.Fatalf("21st free request: %d %s", resp.StatusCode(), resp.Body())
	}
}
