package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// Task 393: errors.types: [content_filter] is a 200 that ends filtered on
// OpenAI (finish_reason content_filter) and Anthropic (stop_reason
// refusal), with and without streaming; other drivers keep their own reply.
func TestChaosContentFilterIsFinish(t *testing.T) {
	cfg := chaosMatrixCfg(100, "content_filter", "")
	oai, anth := chaosDialects[0], chaosDialects[1]

	w := chaosPost(t, newEngine(t, cfg), oai, nil, oai.body)
	var c struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		}
	}
	if err := json.Unmarshal(w.Body(), &c); err != nil || w.StatusCode() != 200 || len(c.Choices) != 1 || c.Choices[0].FinishReason != "content_filter" {
		t.Fatalf("openai: %d %s", w.StatusCode(), w.Body())
	}

	w = chaosPost(t, newEngine(t, cfg), anth, nil, anth.body)
	var m struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(w.Body(), &m); err != nil || w.StatusCode() != 200 || m.Type != "message" || m.StopReason != "refusal" {
		t.Fatalf("anthropic: %d %s", w.StatusCode(), w.Body())
	}

	w = chaosPost(t, newEngine(t, cfg), oai, nil, oai.streamBody)
	if b := string(w.Body()); w.StatusCode() != 200 || !strings.Contains(b, `"finish_reason":"content_filter"`) || !strings.HasSuffix(strings.TrimSpace(b), "data: [DONE]") {
		t.Fatalf("openai stream: %d %s", w.StatusCode(), b)
	}
	w = chaosPost(t, newEngine(t, cfg), anth, nil, anth.streamBody)
	if b := string(w.Body()); w.StatusCode() != 200 || !strings.Contains(b, `"stop_reason":"refusal"`) || !strings.Contains(b, "event: message_stop") {
		t.Fatalf("anthropic stream: %d %s", w.StatusCode(), b)
	}

	// OpenRouter moderates with a 403 (spec 006): the driver's error stays.
	w = call(t, newEngine(t, cfg), "openrouter", "/api/v1/chat/completions", impAuth, `{"model":"m-chaos","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 403 || !strings.Contains(string(w.Body()), "content_policy_violation") {
		t.Fatalf("openrouter: %d %s", w.StatusCode(), w.Body())
	}
}

// finish_length ends the stream early with a valid length finish: no
// error, the terminator present and the usage of what was sent.
func TestChaosFinishLengthEndsWithLength(t *testing.T) {
	cfg := strings.Replace(chaosMatrixCfg(0, "rate_limit", ", mid_stream_rate: 100, mid_stream_types: [finish_length]"), "after_chunks: 1", "after_chunks: 2", 1)
	oai, anth := chaosDialects[0], chaosDialects[1]

	body := strings.Replace(oai.streamBody, `"stream":true`, `"stream":true,"stream_options":{"include_usage":true}`, 1)
	b := string(chaosPost(t, newEngine(t, cfg), oai, nil, body).Body())
	if !strings.Contains(b, `"finish_reason":"length"`) || strings.Contains(b, `"error"`) || !strings.HasSuffix(strings.TrimSpace(b), "data: [DONE]") {
		t.Fatalf("openai: %s", b)
	}
	if n := strings.Count(b, `"content":`); n > 4 {
		t.Fatalf("openai: %d content deltas after a cut at 2 chunks: %s", n, b)
	}
	if !strings.Contains(b, `"completion_tokens":2,`) {
		t.Fatalf("openai: usage is not that of the 2 chunks sent: %s", b)
	}

	b = string(chaosPost(t, newEngine(t, cfg), anth, nil, anth.streamBody).Body())
	if !strings.Contains(b, `"stop_reason":"max_tokens"`) || strings.Contains(b, "event: error") || !strings.Contains(b, "event: message_stop") {
		t.Fatalf("anthropic: %s", b)
	}

	// A cut inside a tool call closes the call (its arguments stay cut).
	tool := map[string]string{"x-wwz-tool-calls": `[{"name":"get_weather","arguments":{"city":"Recife","unit":"celsius","days":3}}]`}
	b = string(chaosPost(t, newEngine(t, cfg), anth, tool, anth.streamBody).Body())
	if !strings.Contains(b, `"stop_reason":"max_tokens"`) || strings.Count(b, "event: content_block_start") != strings.Count(b, "event: content_block_stop") {
		t.Fatalf("anthropic tool: %s", b)
	}
	cut := chaosMatrixCfg(0, "rate_limit", ", mid_stream_rate: 100, mid_stream_types: [finish_length]")
	b = string(chaosPost(t, newEngine(t, cut), anth, tool, anth.streamBody).Body())
	if !strings.Contains(b, `"stop_reason":"max_tokens"`) || strings.Count(b, "event: content_block_start") != strings.Count(b, "event: content_block_stop") {
		t.Fatalf("anthropic tool start: %s", b)
	}
}
