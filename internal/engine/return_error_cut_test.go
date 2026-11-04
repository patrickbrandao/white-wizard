package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/azure"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// Task 405 (ADR 0070): x-wwz-return-error: content_filter is the 200 of
// errors.types: [content_filter] on OpenAI and Anthropic, stream and not,
// with no drawn error replacing it; the other drivers keep their error.
func TestReturnErrorContentFilterIsFinish(t *testing.T) {
	// Every request draws a rate_limit: the header still wins.
	cfg := chaosMatrixCfg(100, "rate_limit", "")
	h := map[string]string{"x-wwz-return-error": "content_filter"}
	oai, anth := chaosDialects[0], chaosDialects[1]

	w := chaosPost(t, newEngine(t, cfg), oai, h, oai.body)
	var c struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		}
	}
	if err := json.Unmarshal(w.Body(), &c); err != nil || w.StatusCode() != 200 || len(c.Choices) != 1 || c.Choices[0].FinishReason != "content_filter" {
		t.Fatalf("openai: %d %s", w.StatusCode(), w.Body())
	}
	w = chaosPost(t, newEngine(t, cfg), oai, h, oai.streamBody)
	if b := string(w.Body()); w.StatusCode() != 200 || !strings.Contains(b, `"finish_reason":"content_filter"`) || !strings.HasSuffix(strings.TrimSpace(b), "data: [DONE]") {
		t.Fatalf("openai stream: %d %s", w.StatusCode(), b)
	}

	w = chaosPost(t, newEngine(t, cfg), anth, h, anth.body)
	var m struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(w.Body(), &m); err != nil || w.StatusCode() != 200 || m.Type != "message" || m.StopReason != "refusal" {
		t.Fatalf("anthropic: %d %s", w.StatusCode(), w.Body())
	}
	w = chaosPost(t, newEngine(t, cfg), anth, h, anth.streamBody)
	if b := string(w.Body()); w.StatusCode() != 200 || !strings.Contains(b, `"stop_reason":"refusal"`) || !strings.Contains(b, "event: message_stop") {
		t.Fatalf("anthropic stream: %d %s", w.StatusCode(), b)
	}

	// OpenRouter moderates with a 403 (spec 006): the driver's error stays.
	or := map[string]string{"Authorization": impAuth["Authorization"], "x-wwz-return-error": "content_filter"}
	w = call(t, newEngine(t, cfg), "openrouter", "/api/v1/chat/completions", or, `{"model":"m-chaos","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 403 || !strings.Contains(string(w.Body()), "content_policy_violation") {
		t.Fatalf("openrouter: %d %s", w.StatusCode(), w.Body())
	}
	// Azure keeps its prompt-filter 400 (spec 006).
	az := map[string]string{"api-key": "wwz-key-01", "x-wwz-return-error": "content_filter"}
	w = call(t, newEngine(t, cfg), "azure", "/openai/v1/chat/completions", az, `{"model":"m-chaos","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 400 || !strings.Contains(string(w.Body()), `"content_filter"`) {
		t.Fatalf("azure: %d %s", w.StatusCode(), w.Body())
	}
	// Other names are still the type's error.
	h["x-wwz-return-error"] = "rate_limit"
	if w = chaosPost(t, newEngine(t, cfg), oai, h, oai.body); w.StatusCode() != 429 {
		t.Fatalf("rate_limit: %d %s", w.StatusCode(), w.Body())
	}
}

// A refusal before any output (moderation mode refusal) has empty content
// and no output tokens on Anthropic (docs "Refusals and fallback": content
// [] and output_tokens 0); OpenAI keeps the refusal text.
func TestRefusalBeforeOutputIsEmptyOnAnthropic(t *testing.T) {
	cfg := strings.Replace(chaosMatrixCfg(0, "rate_limit", ""), "after_chunks: 1}", "after_chunks: 1, moderation: {probability: 100, mode: refusal}}", 1)
	anth := chaosDialects[1]
	w := chaosPost(t, newEngine(t, cfg), anth, nil, anth.body)
	var m struct {
		Content    []any  `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body(), &m); err != nil || w.StatusCode() != 200 || m.StopReason != "refusal" || len(m.Content) != 0 || m.Usage.OutputTokens != 0 {
		t.Fatalf("anthropic: %d %s", w.StatusCode(), w.Body())
	}
	w = chaosPost(t, newEngine(t, cfg), anth, nil, anth.streamBody)
	if b := string(w.Body()); w.StatusCode() != 200 || !strings.Contains(b, `"stop_reason":"refusal"`) || strings.Contains(b, "content_block_start") || !strings.Contains(b, `"output_tokens":0`) {
		t.Fatalf("anthropic stream: %d %s", w.StatusCode(), b)
	}
	oai := chaosDialects[0]
	if b := string(chaosPost(t, newEngine(t, cfg), oai, nil, oai.body).Body()); !strings.Contains(b, `"refusal": "I can't help with that request."`) {
		t.Fatalf("openai: %s", b)
	}
}

const cutResponsesCfg = `
server: {seed: 7}
keys:
  - {key: k-a, label: a, account: Acme}
profiles:
  errors:
    cut: {rate: 0, types: [rate_limit], mid_stream_rate: 100, mid_stream_types: [finish_length], after_chunks: 2}
    trunc: {rate: 0, types: [rate_limit], mid_stream_rate: 100, mid_stream_types: [stream_truncated], after_chunks: 2}
models:
  - {id: m-full, protocols: [openai], errors: none, latency: {ttft: 0ms, itl: 0ms, clock: logical}, prompt_cache: {kind: none}}
  - {id: m-cut, protocols: [openai], errors: cut, latency: {ttft: 0ms, itl: 0ms, clock: logical}, prompt_cache: {kind: none}}
  - {id: m-trunc, protocols: [openai], errors: trunc, latency: {ttft: 0ms, itl: 0ms, clock: logical}, prompt_cache: {kind: none}}
`

func respSay(t *testing.T, e *engine.Engine, say, body string) (int, string) {
	t.Helper()
	d, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := core.NewMemWriter()
	h := driver.MapHeaders{"authorization": "Bearer k-a", "x-wwz-response": say}
	e.Handle(context.Background(), engine.Input{Driver: d, Kind: driver.KindResponses, Method: "POST", Path: "/v1/responses", Headers: h, Body: []byte(body), ClientIP: "10.0.0.1"}, w)
	return w.StatusCode(), string(w.Body())
}

// streamID and streamText read the response id and the text deltas of a
// Responses stream.
func streamID(t *testing.T, b string) string {
	t.Helper()
	i := strings.Index(b, `"id":"resp_`)
	if i < 0 {
		t.Fatalf("no id: %s", b)
	}
	return b[i+6 : i+6+strings.Index(b[i+6:], `"`)]
}

func streamText(b string) string {
	var sb strings.Builder
	for _, line := range strings.Split(b, "\n") {
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil && ev.Type == "response.output_text.delta" {
			sb.WriteString(ev.Delta)
		}
	}
	return sb.String()
}

func inputTokens(t *testing.T, b string) int {
	t.Helper()
	var r struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(b), &r); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return r.Usage.InputTokens
}

// Task 405 (ADR 0070, spec 012): a stream cut by finish_length records the
// output it delivered, so previous_response_id adds only what the client
// saw; an aborted stream records nothing (not conferred).
func TestResponsesRecordAfterFinishLengthCut(t *testing.T) {
	e := newEngine(t, cutResponsesCfg)
	long := "Hello there, this is a much longer answer than two chunks can carry."
	st, b := respSay(t, e, long, `{"model":"m-cut","input":"Say hi.","stream":true}`)
	if st != 200 || !strings.Contains(b, "response.incomplete") {
		t.Fatalf("cut: %d %s", st, b)
	}
	cutID, sent := streamID(t, b), streamText(b)
	if sent == "" || sent == long {
		t.Fatalf("cut sent %q", sent)
	}
	// The same conversation with the delivered text as the whole reply.
	st, b = respSay(t, e, sent, `{"model":"m-full","input":"Say hi."}`)
	if st != 200 {
		t.Fatalf("full: %d %s", st, b)
	}
	fullID := respID(t, b)
	st, cut := respSay(t, e, "ok", chainBody("m-full", cutID))
	st2, full := respSay(t, e, "ok", chainBody("m-full", fullID))
	if st != 200 || st2 != 200 || inputTokens(t, cut) != inputTokens(t, full) {
		t.Fatalf("chained after the cut: %d %s; after the delivered text: %d %s", st, cut, st2, full)
	}
	// The whole reply weighs more: the record is not that of the full text.
	_, b = respSay(t, e, long, `{"model":"m-full","input":"Say hi."}`)
	if _, whole := respSay(t, e, "ok", chainBody("m-full", respID(t, b))); inputTokens(t, whole) <= inputTokens(t, cut) {
		t.Fatalf("whole %s vs cut %s", whole, cut)
	}

	// A tool call cut mid-arguments records the call as sent.
	d, _ := driver.New("openai", nil)
	w := core.NewMemWriter()
	hd := driver.MapHeaders{"authorization": "Bearer k-a", "x-wwz-tool-calls": `[{"name":"get_weather","arguments":{"city":"Recife","unit":"celsius","days":3}}]`}
	e.Handle(context.Background(), engine.Input{Driver: d, Kind: driver.KindResponses, Method: "POST", Path: "/v1/responses", Headers: hd, Body: []byte(`{"model":"m-cut","input":"Weather?","stream":true,"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`), ClientIP: "10.0.0.1"}, w)
	if st, b := respSay(t, e, "ok", chainBody("m-full", streamID(t, string(w.Body())))); st != 200 {
		t.Fatalf("chained after a cut tool call: %d %s", st, b)
	}

	// An aborted stream leaves no record.
	_, b = respSay(t, e, long, `{"model":"m-trunc","input":"Say hi.","stream":true}`)
	if st, b := respSay(t, e, "ok", chainBody("m-full", streamID(t, b))); st != 400 || !strings.Contains(b, "previous_response_not_found") {
		t.Fatalf("chained after an abort: %d %s", st, b)
	}
}
