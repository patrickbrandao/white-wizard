package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// ADR 0040: metrics, events, logs and records carry models[].id; the name
// asked for (alias, -scenario- suffix) only feeds the scenarios.
const labelCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: lbl-model, aliases: [lbl-alias], latency: fast}
keys:
  - {key: k-label, label: label}
`

var labelKey = map[string]string{"Authorization": "Bearer k-label"}

const fileTools = `"tools":[
  {"type":"function","function":{"name":"read_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}},
  {"type":"function","function":{"name":"write_file","parameters":{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}}}}},
  {"type":"function","function":{"name":"edit_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}},
  {"type":"function","function":{"name":"delete_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}}]`

func TestAliasCountsUnderID(t *testing.T) {
	e := newEngine(t, labelCfg)
	e.M.MirrorVLLM()
	for _, stream := range []string{"", `"stream":true,`} {
		body := `{"model":"lbl-alias",` + stream + `"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		w := call(t, e, "openai", "/v1/chat/completions", labelKey, body)
		if w.StatusCode() != 200 {
			t.Fatalf("stream=%q: %d %s", stream, w.StatusCode(), w.Body())
		}
		if rec := e.Recorder.Last(1, nil); len(rec) != 1 || rec[0].Model != "lbl-model" {
			t.Errorf("stream=%q: recorded %+v, want lbl-model", stream, rec)
		}
	}
	if n := e.M.Requests.Value("openai", "lbl-model", "chat", "200"); n != 2 {
		t.Errorf("wwz_requests_total{model=lbl-model} = %d, want 2", n)
	}
	if n := e.M.Requests.Value("openai", "lbl-alias", "chat", "200"); n != 0 {
		t.Errorf("alias became a label: %d", n)
	}
	var buf bytes.Buffer
	e.M.Registry.Write(&buf)
	if out := buf.String(); strings.Contains(out, "lbl-alias") {
		t.Errorf("alias in metrics output:\n%s", out)
	} else if !strings.Contains(out, `vllm:time_to_first_token_seconds_count{model_name="lbl-model"}`) {
		t.Errorf("vllm mirror without model_name=lbl-model:\n%s", out)
	}
	evs, err := e.Store.Events(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Type != "request" || ev.Model != "lbl-model" {
			t.Errorf("event %+v, want request with model lbl-model", ev)
		}
	}
}

func TestScenarioByModelSuffix(t *testing.T) {
	e := newEngine(t, labelCfg)
	for _, asked := range []string{"lbl-model-scenario-file-agent-flow", "lbl-alias-scenario-file-agent-flow"} {
		body := `{"model":"` + asked + `","messages":[{"role":"user","content":"do the file task"}],` + fileTools + `}`
		w := call(t, e, "openai", "/v1/chat/completions", labelKey, body)
		if w.StatusCode() != 200 {
			t.Fatalf("%s: %d %s", asked, w.StatusCode(), w.Body())
		}
		if got := w.Headers()["x-wwz-scenario"]; got != "file-agent-flow" {
			t.Errorf("%s: x-wwz-scenario %q", asked, got)
		}
		var resp struct{ Model string }
		if err := json.Unmarshal(w.Body(), &resp); err != nil || resp.Model == "" {
			t.Fatalf("%s: body %s (%v)", asked, w.Body(), err)
		}
		if rec := e.Recorder.Last(1, nil); len(rec) != 1 || rec[0].Model != "lbl-model" || rec[0].Scenario != "file-agent-flow" {
			t.Errorf("%s: recorded %+v", asked, rec)
		}
	}
	if n := e.M.Requests.Value("openai", "lbl-model", "chat", "200"); n != 2 {
		t.Errorf("wwz_requests_total{model=lbl-model} = %d, want 2", n)
	}
	// The suffix names a scenario that does not exist: the error of today.
	w := call(t, e, "openai", "/v1/chat/completions", labelKey, `{"model":"lbl-model-scenario-nope","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 400 || !strings.Contains(string(w.Body()), "unknown scenario") {
		t.Errorf("unknown scenario: %d %s", w.StatusCode(), w.Body())
	}
	// A base that is not a model is still 404.
	w = call(t, e, "openai", "/v1/chat/completions", labelKey, `{"model":"nope-scenario-file-agent-flow","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 404 {
		t.Errorf("unknown base: %d %s", w.StatusCode(), w.Body())
	}
}

func TestOpenRouterSlugWithScenarioSuffix(t *testing.T) {
	e := newEngine(t, slugCfg)
	body := `{"model":"white-wizard/wwz-fast-scenario-file-agent-flow","messages":[{"role":"user","content":"do the file task"}],` + fileTools + `}`
	w := call(t, e, "openrouter", "/api/v1/chat/completions", slugKey, body)
	if w.StatusCode() != 200 || w.Headers()["x-wwz-scenario"] != "file-agent-flow" {
		t.Fatalf("%d %v %s", w.StatusCode(), w.Headers(), w.Body())
	}
	if n := e.M.Requests.Value("openrouter", "wwz-fast", "chat", "200"); n != 1 {
		t.Errorf("wwz_requests_total{model=wwz-fast} = %d, want 1", n)
	}
}

func TestUnknownModelHasEmptyLabel(t *testing.T) {
	e := newEngine(t, labelCfg)
	const raw = "client-made-up-name"
	w := call(t, e, "openai", "/v1/chat/completions", labelKey, `{"model":"`+raw+`","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 404 {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
	if n := e.M.Requests.Value("openai", "", "chat", "404"); n != 1 {
		t.Errorf(`wwz_requests_total{model=""} = %d, want 1`, n)
	}
	var buf bytes.Buffer
	e.M.Registry.Write(&buf)
	if strings.Contains(buf.String(), raw) {
		t.Errorf("client text became a series:\n%s", buf.String())
	}
	// The record keeps the name asked for, for debugging.
	if rec := e.Recorder.Last(1, nil); len(rec) != 1 || rec[0].Model != raw {
		t.Errorf("recorded %+v, want %q", rec, raw)
	}
	// GET /models/{id} of an unknown id: same rule.
	e2 := newEngine(t, slugCfg)
	drv, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	gw := core.NewMemWriter()
	e2.Handle(context.Background(), engineInput(drv, raw), gw)
	if gw.StatusCode() != 404 {
		t.Fatalf("GET unknown: %d %s", gw.StatusCode(), gw.Body())
	}
	if n := e2.M.Requests.Value("openrouter", "", "model", "404"); n != 1 {
		t.Errorf(`GET 404 wwz_requests_total{model=""} = %d, want 1`, n)
	}
	buf.Reset()
	e2.M.Registry.Write(&buf)
	if strings.Contains(buf.String(), raw) {
		t.Errorf("GET: client text became a series:\n%s", buf.String())
	}
}

// TestKeyWithoutModelAccessLabelsID: a model that resolves but the key may
// not use keeps its id (bounded) in the 404 series.
func TestKeyWithoutModelAccessLabelsID(t *testing.T) {
	e := newEngine(t, labelCfg+`  - {key: k-other, label: other, models: [wwz-fast]}
`)
	w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer k-other"}, `{"model":"lbl-alias","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 404 {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
	if n := e.M.Requests.Value("openai", "lbl-model", "chat", "404"); n != 1 {
		t.Errorf("wwz_requests_total{model=lbl-model,404} = %d, want 1", n)
	}
}
