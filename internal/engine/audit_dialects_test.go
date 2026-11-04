package engine_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// Task 383 (auditoria do DONE, task 362): protocols ausente e vazio
// (spec 003), max_tokens por dialeto (spec 007) e modelo aposentado na
// ordem do real (spec 006).

const auditDialectsCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
  generation:
    long: {output_tokens: {dist: fixed, value: 400}}
    short: {output_tokens: {dist: fixed, value: 5}}
models:
  - {id: no-protocols, latency: fast}
  - {id: empty-protocols, protocols: [], latency: fast}
  - {id: long-model, latency: fast, generation: long}
  - {id: short-model, latency: fast, generation: short}
  - {id: retired-model, latency: fast, retires_at: "2020-01-01"}
keys:
  - {key: k-audit, label: audit}
`

var (
	auditOAI  = map[string]string{"Authorization": "Bearer k-audit"}
	auditAnth = map[string]string{"x-api-key": "k-audit", "anthropic-version": "2023-06-01"}
)

func auditGemini(t *testing.T, e *engine.Engine, model, key, body string) (int, []byte) {
	t.Helper()
	path := "/v1beta/models/" + model + ":generateContent"
	h := driver.MapHeaders{"x-goog-api-key": key, "x-wwz-path": path}
	w := handle(t, e, "gemini", engine.Input{Kind: driver.KindGenerate, Method: "POST", Path: path, Headers: h, Body: []byte(body)})
	return w.StatusCode(), w.Body()
}

// TestAuditProtocolsAbsentAndEmpty: spec 003 — a model without protocols or
// with protocols: [] is served in the three dialects and listed by the
// three drivers' /models.
func TestAuditProtocolsAbsentAndEmpty(t *testing.T) {
	e := newEngine(t, auditDialectsCfg)
	for _, id := range []string{"no-protocols", "empty-protocols"} {
		if w := call(t, e, "openai", "/v1/chat/completions", auditOAI, `{"model":"`+id+`","messages":[{"role":"user","content":"hi"}]}`); w.StatusCode() != 200 {
			t.Errorf("%s openai chat: %d %s", id, w.StatusCode(), w.Body())
		}
		if w := call(t, e, "anthropic", "/v1/messages", auditAnth, `{"model":"`+id+`","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`); w.StatusCode() != 200 {
			t.Errorf("%s anthropic messages: %d %s", id, w.StatusCode(), w.Body())
		}
		if status, body := auditGemini(t, e, id, "k-audit", `{"contents":[{"parts":[{"text":"hi"}]}]}`); status != 200 {
			t.Errorf("%s gemini generateContent: %d %s", id, status, body)
		}
	}
	for _, drv := range []string{"openai", "anthropic", "gemini"} {
		var ids []string
		for _, m := range e.Models(drv) {
			ids = append(ids, m.Cfg.ID)
		}
		list := strings.Join(ids, ",")
		for _, id := range []string{"no-protocols", "empty-protocols"} {
			if !strings.Contains(","+list+",", ","+id+",") {
				t.Errorf("%s /models lacks %s: %s", drv, id, list)
			}
		}
	}
	// The listing route of Gemini serves them too.
	h := driver.MapHeaders{"x-goog-api-key": "k-audit"}
	w := handle(t, e, "gemini", engine.Input{Kind: driver.KindModels, Method: "GET", Path: "/v1beta/models", Headers: h})
	if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), "empty-protocols") {
		t.Errorf("gemini GET /v1beta/models: %d %s", w.StatusCode(), w.Body())
	}
}

type auditGeminiResp struct {
	Candidates []struct {
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
}

// TestAuditGeminiMaxOutputTokens: spec 007 — a ceiling below the drawn
// target ends with MAX_TOKENS and exactly that many candidate tokens; a
// ceiling above it ends with STOP.
func TestAuditGeminiMaxOutputTokens(t *testing.T) {
	e := newEngine(t, auditDialectsCfg)
	decode := func(model string, max int) auditGeminiResp {
		t.Helper()
		body := `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":` + strconv.Itoa(max) + `}}`
		status, b := auditGemini(t, e, model, "k-audit", body)
		if status != 200 {
			t.Fatalf("%s: %d %s", model, status, b)
		}
		var r auditGeminiResp
		if err := json.Unmarshal(b, &r); err != nil || len(r.Candidates) != 1 {
			t.Fatalf("%v: %s", err, b)
		}
		return r
	}
	r := decode("long-model", 16)
	if r.Candidates[0].FinishReason != "MAX_TOKENS" || r.UsageMetadata.CandidatesTokenCount != 16 {
		t.Errorf("below target: finishReason %q candidatesTokenCount %d, want MAX_TOKENS and 16", r.Candidates[0].FinishReason, r.UsageMetadata.CandidatesTokenCount)
	}
	r = decode("short-model", 16)
	if r.Candidates[0].FinishReason != "STOP" || r.UsageMetadata.CandidatesTokenCount > 16 {
		t.Errorf("below ceiling: finishReason %q candidatesTokenCount %d, want STOP", r.Candidates[0].FinishReason, r.UsageMetadata.CandidatesTokenCount)
	}
}

// TestAuditAnthropicMaxTokensBelowCeiling: spec 007 — output under max_tokens
// ends with end_turn; at the ceiling, with max_tokens.
func TestAuditAnthropicMaxTokensBelowCeiling(t *testing.T) {
	e := newEngine(t, auditDialectsCfg)
	stop := func(model string) string {
		t.Helper()
		w := call(t, e, "anthropic", "/v1/messages", auditAnth, `{"model":"`+model+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		var out struct {
			StopReason string `json:"stop_reason"`
		}
		if w.StatusCode() != 200 || json.Unmarshal(w.Body(), &out) != nil {
			t.Fatalf("%s: %d %s", model, w.StatusCode(), w.Body())
		}
		return out.StopReason
	}
	if got := stop("short-model"); got != "end_turn" {
		t.Errorf("below ceiling: stop_reason %q, want end_turn", got)
	}
	if got := stop("long-model"); got != "max_tokens" {
		t.Errorf("at ceiling: stop_reason %q, want max_tokens", got)
	}
}

// TestAuditRetiredFollowsUnknownOrder: spec 006 — a retired model sits in the
// same position as a nonexistent one: a bad key is 401 first, an invalid
// body is the same error, and with a valid body the answer has the same
// status and error type.
func TestAuditRetiredFollowsUnknownOrder(t *testing.T) {
	e := newEngine(t, auditDialectsCfg)
	type route struct {
		drv, path string
		good, bad map[string]string
		body      func(model string) string
	}
	routes := []route{
		{
			"openai", "/v1/chat/completions", auditOAI,
			map[string]string{"Authorization": "Bearer nope"},
			func(m string) string { return `{"model":"` + m + `","messages":[{"role":"user","content":"hi"}]}` },
		},
		{
			"anthropic", "/v1/messages", auditAnth,
			map[string]string{"x-api-key": "nope", "anthropic-version": "2023-06-01"},
			func(m string) string {
				return `{"model":"` + m + `","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`
			},
		},
	}
	errType := func(b []byte) string {
		var out struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &out)
		return out.Error.Type
	}
	for _, rt := range routes {
		for _, m := range []string{"retired-model", "no-such-model"} {
			if w := call(t, e, rt.drv, rt.path, rt.bad, rt.body(m)); w.StatusCode() != 401 {
				t.Errorf("%s bad key, %s: %d %s", rt.drv, m, w.StatusCode(), w.Body())
			}
		}
		// Invalid JSON with a good key is rejected whatever the model is.
		if w := call(t, e, rt.drv, rt.path, rt.good, `{not json`); w.StatusCode() != 400 {
			t.Errorf("%s invalid JSON: %d %s", rt.drv, w.StatusCode(), w.Body())
		}
		// A body that is valid JSON but incomplete: the retired model takes
		// the same position in the checks as the unknown one.
		inc := func(m string) string { return `{"model":"` + m + `"}` }
		ri := call(t, e, rt.drv, rt.path, rt.good, inc("retired-model"))
		ui := call(t, e, rt.drv, rt.path, rt.good, inc("no-such-model"))
		if ri.StatusCode() != ui.StatusCode() || errType(ri.Body()) != errType(ui.Body()) {
			t.Errorf("%s incomplete body: retired %d %s vs unknown %d %s", rt.drv, ri.StatusCode(), ri.Body(), ui.StatusCode(), ui.Body())
		}
		ru := call(t, e, rt.drv, rt.path, rt.good, rt.body("retired-model"))
		un := call(t, e, rt.drv, rt.path, rt.good, rt.body("no-such-model"))
		if ru.StatusCode() != un.StatusCode() || errType(ru.Body()) != errType(un.Body()) {
			t.Errorf("%s retired %d %s vs unknown %d %s", rt.drv, ru.StatusCode(), ru.Body(), un.StatusCode(), un.Body())
		}
		if ru.StatusCode() != 404 {
			t.Errorf("%s retired: status %d, want 404", rt.drv, ru.StatusCode())
		}
	}
}
