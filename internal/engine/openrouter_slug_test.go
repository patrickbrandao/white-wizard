package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// ADR 0038: the openrouter driver resolves the model field like the real
// API (task 163): the slug of the id or of an alias, or the name after the
// author, in any case; it answers with the slug and keeps models[].id in
// metrics and records (an alias counts under the id, ADR 0040).
const slugCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: wwz-fast, aliases: [fast-alias], latency: fast}
  - {id: anthropic/claude-x, latency: fast}
keys:
  - {key: sk-or-v1-slug, label: slug}
`

var slugKey = map[string]string{"Authorization": "Bearer sk-or-v1-slug"}

// sseModels returns the model field of every data: frame of an SSE body.
func sseModels(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		if msg, ok := m["message"].(map[string]any); ok { // Anthropic message_start
			m = msg
		}
		if v, ok := m["model"].(string); ok {
			out = append(out, v)
		}
	}
	return out
}

func TestOpenRouterSlugChat(t *testing.T) {
	e := newEngine(t, slugCfg)
	for _, tc := range []struct{ asked, want, label string }{
		{"white-wizard/wwz-fast", "white-wizard/wwz-fast", "wwz-fast"},
		{"wwz-fast", "white-wizard/wwz-fast", "wwz-fast"},
		{"fast-alias", "white-wizard/wwz-fast", "wwz-fast"},
		{"white-wizard/fast-alias", "white-wizard/wwz-fast", "wwz-fast"},
		{"anthropic/claude-x", "anthropic/claude-x", "anthropic/claude-x"},
		{"claude-x", "anthropic/claude-x", "anthropic/claude-x"},
		{"WWZ-Fast", "white-wizard/wwz-fast", "wwz-fast"},
		{"White-Wizard/WWZ-FAST", "white-wizard/wwz-fast", "wwz-fast"},
		{"Anthropic/Claude-X", "anthropic/claude-x", "anthropic/claude-x"},
	} {
		for _, stream := range []bool{false, true} {
			body := `{"model":"` + tc.asked + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			if stream {
				body = strings.Replace(body, `"max_tokens"`, `"stream":true,"max_tokens"`, 1)
			}
			w := call(t, e, "openrouter", "/api/v1/chat/completions", slugKey, body)
			if w.StatusCode() != 200 {
				t.Fatalf("%s stream=%v: %d %s", tc.asked, stream, w.StatusCode(), w.Body())
			}
			if stream {
				models := sseModels(t, string(w.Body()))
				if len(models) == 0 {
					t.Fatalf("%s: no chunk with model: %s", tc.asked, w.Body())
				}
				for _, m := range models {
					if m != tc.want {
						t.Errorf("%s stream: chunk model %q, want %q", tc.asked, m, tc.want)
					}
				}
			} else {
				var got struct{ Model string }
				if err := json.Unmarshal(w.Body(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Model != tc.want {
					t.Errorf("%s: model %q, want %q", tc.asked, got.Model, tc.want)
				}
			}
			if rec := e.Recorder.Last(1, nil); len(rec) != 1 || rec[0].Model != tc.label {
				t.Errorf("%s: recorded model %+v, want %q", tc.asked, rec, tc.label)
			}
		}
		if n := e.M.Requests.Value("openrouter", tc.label, "chat", "200"); n < 2 {
			t.Errorf("%s: wwz_requests_total{model=%q} = %d, want >= 2", tc.asked, tc.label, n)
		}
		if n := e.M.Requests.Value("openrouter", tc.want, "chat", "200"); tc.want != tc.label && n != 0 {
			t.Errorf("%s: slug %q became a metric label", tc.asked, tc.want)
		}
	}
}

func TestOpenRouterSlugMessages(t *testing.T) {
	e := newEngine(t, slugCfg)
	h := slugKey
	for _, stream := range []string{"", `"stream":true,`} {
		body := `{"model":"white-wizard/wwz-fast",` + stream + `"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
		w := call(t, e, "openrouter", "/api/v1/messages", h, body)
		if w.StatusCode() != 200 {
			t.Fatalf("stream=%q: %d %s", stream, w.StatusCode(), w.Body())
		}
		var models []string
		if stream == "" {
			var got struct{ Model string }
			if err := json.Unmarshal(w.Body(), &got); err != nil {
				t.Fatal(err)
			}
			models = []string{got.Model}
		} else {
			models = sseModels(t, string(w.Body()))
		}
		if len(models) == 0 {
			t.Fatalf("no model in %s", w.Body())
		}
		for _, m := range models {
			if m != "white-wizard/wwz-fast" {
				t.Errorf("stream=%q: model %q", stream, m)
			}
		}
	}
}

// TestOpenRouterSlugUnknown: a name that resolves to no model, with or
// without an author (or with another author), is the real API's 400.
func TestOpenRouterSlugUnknown(t *testing.T) {
	e := newEngine(t, slugCfg)
	for _, m := range []string{"white-wizard/nope", "openai/wwz-fast", "nope", "x/claude-x", "wwz"} {
		w := call(t, e, "openrouter", "/api/v1/chat/completions", slugKey, `{"model":"`+m+`","messages":[{"role":"user","content":"hi"}]}`)
		want := `{"error":{"message":"` + m + ` is not a valid model ID","code":400},"user_id":"user_wwz"}`
		if w.StatusCode() != 400 || string(w.Body()) != want {
			t.Errorf("%s: %d %s, want 400 %s", m, w.StatusCode(), w.Body(), want)
		}
	}
	w := call(t, e, "openrouter", "/api/v1/messages", slugKey, `{"model":"nope","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	var got struct {
		Type  string
		Error struct {
			Type, Message string
			ErrorType     string `json:"error_type"`
		}
		RequestID string `json:"request_id"`
	}
	if w.StatusCode() != 400 || json.Unmarshal(w.Body(), &got) != nil || got.Type != "error" ||
		got.Error.Type != "invalid_request_error" || got.Error.Message != "nope is not a valid model ID" ||
		got.Error.ErrorType != "invalid_request" || got.RequestID == "" {
		t.Errorf("messages: %d %s", w.StatusCode(), w.Body())
	}
}

// TestSlugOnlyOnOpenRouter: other drivers neither strip the prefix nor
// rename the model.
func TestSlugOnlyOnOpenRouter(t *testing.T) {
	e := newEngine(t, slugCfg)
	h := map[string]string{"Authorization": "Bearer sk-or-v1-slug"}
	w := call(t, e, "openai", "/v1/chat/completions", h, `{"model":"white-wizard/wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 404 {
		t.Fatalf("openai with slug: %d %s", w.StatusCode(), w.Body())
	}
	w = call(t, e, "openai", "/v1/chat/completions", h, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(string(w.Body()), `"model": "wwz-fast"`) {
		t.Fatalf("openai model: %s", w.Body())
	}
}

func getOpenRouterModel(t *testing.T, id string) *core.MemWriter {
	t.Helper()
	e := newEngine(t, slugCfg)
	drv, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := core.NewMemWriter()
	e.Handle(context.Background(), engineInput(drv, id), w)
	return w
}

func engineInput(drv driver.Driver, id string) engine.Input {
	return engine.Input{
		Driver: drv, Kind: driver.KindModel, Method: "GET", Path: "/api/v1/models/" + id,
		Headers: driver.MapHeaders{"authorization": "Bearer sk-or-v1-slug"}, PathParam: id,
	}
}

// TestOpenRouterGetModelBySlug: /api/v1/models/{...} serves no model (ADR
// 0046); it answers like the real API answers an unknown route.
func TestOpenRouterGetModelBySlug(t *testing.T) {
	for _, id := range []string{"white-wizard/wwz-fast", "wwz-fast", "anthropic/claude-x", "white-wizard/nope", "fast-alias"} {
		w := getOpenRouterModel(t, id)
		if w.StatusCode() != 404 || string(w.Body()) != `{"error":{"message":"Not Found","code":404}}` {
			t.Errorf("GET models/%s: %d %s, want 404 Not Found", id, w.StatusCode(), w.Body())
		}
	}
}

// TestOpenRouterGetModelSingularRoute: GET /api/v1/model/{author}/{slug}
// matches only the full slug, in any case, as the real route (ADR 0040,
// checked against the real API in task 163).
func TestOpenRouterGetModelSingularRoute(t *testing.T) {
	e := newEngine(t, slugCfg)
	drv, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"white-wizard/wwz-fast":   "white-wizard/wwz-fast",
		"anthropic/claude-x":      "anthropic/claude-x",
		"Anthropic/Claude-X":      "anthropic/claude-x",
		"wwz-fast":                "Not Found",
		"white-wizard/wwz-fast/x": "Not Found",
		"white-wizard/fast-alias": "Model not found: white-wizard/fast-alias",
		"white-wizard/nope":       "Model not found: white-wizard/nope",
		"x/claude-x":              "Model not found: x/claude-x",
	} {
		in := engineInput(drv, id)
		in.Path = "/api/v1/model/" + id
		w := core.NewMemWriter()
		e.Handle(context.Background(), in, w)
		if strings.Contains(want, " ") || !strings.Contains(want, "/") {
			if body := `{"error":{"message":"` + want + `","code":404}}`; w.StatusCode() != 404 || string(w.Body()) != body {
				t.Errorf("GET model/%s: %d %s, want 404 %s", id, w.StatusCode(), w.Body(), body)
			}
			continue
		}
		var got struct{ Data struct{ ID string } }
		if w.StatusCode() != 200 || json.Unmarshal(w.Body(), &got) != nil || got.Data.ID != want {
			t.Errorf("GET model/%s: %d %s, want id %q", id, w.StatusCode(), w.Body(), want)
		}
	}
}
