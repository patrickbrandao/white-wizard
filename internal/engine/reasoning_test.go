package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// ADR 0022: reasoning faithful to each provider.

// timedWriter records when the first SSE data frame was written.
type timedWriter struct {
	*core.MemWriter
	mu        sync.Mutex
	firstData time.Time
}

func (w *timedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.firstData.IsZero() && strings.Contains(string(p), "data:") {
		w.firstData = time.Now()
	}
	w.mu.Unlock()
	return w.MemWriter.Write(p)
}

// Stream hands the engine this writer, not the embedded MemWriter.
func (w *timedWriter) Stream(fn func(core.ResponseWriter)) { fn(w) }

// callWith is call with driver options and a caller-provided writer.
func callWith(t *testing.T, e *engine.Engine, w core.ResponseWriter, drvName string, opts map[string]string, path, body string) {
	t.Helper()
	drv, err := driver.New(drvName, opts)
	if err != nil {
		t.Fatal(err)
	}
	var kind driver.Kind
	for _, rt := range drv.Routes() {
		if rt.Path == path {
			kind = rt.Kind
		}
	}
	if kind == "" {
		t.Fatalf("no route for %s", path)
	}
	h := driver.MapHeaders{"authorization": "Bearer wwz-key-01"}
	if drvName == "opencode" {
		h["x-opencode-session"] = "ses_1" // required, ADR 0059
	}
	e.Handle(context.Background(), engine.Input{Driver: drv, Kind: kind, Method: "POST", Path: path, Headers: h, Body: []byte(body), ClientIP: "127.0.0.1"}, w)
}

type reasoningUsage struct {
	PromptTokens            int     `json:"prompt_tokens"`
	CompletionTokens        int     `json:"completion_tokens"`
	Cost                    float64 `json:"cost"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type reasoningBody struct {
	Choices []struct {
		Message map[string]any `json:"message"`
		Delta   map[string]any `json:"delta"`
	} `json:"choices"`
	Usage *reasoningUsage `json:"usage"`
}

// streamChunks decodes every data frame except [DONE].
func streamChunks(t *testing.T, b []byte) []reasoningBody {
	t.Helper()
	var out []reasoningBody
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var c reasoningBody
		if err := json.Unmarshal([]byte(line[6:]), &c); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		out = append(out, c)
	}
	return out
}

// hasReasoningField reports a reasoning field with a value: OpenRouter's
// message always carries reasoning, null without reasoning (task 174).
func hasReasoningField(m map[string]any) bool {
	return m["reasoning_content"] != nil || m["reasoning"] != nil || m["reasoning_details"] != nil
}

// checkCounted: reasoning tokens > 0 and included in completion_tokens.
func checkCounted(t *testing.T, u *reasoningUsage, raw []byte) {
	t.Helper()
	if u == nil {
		t.Fatalf("no usage: %s", raw)
	}
	if u.CompletionTokensDetails.ReasoningTokens <= 0 || u.CompletionTokens <= u.CompletionTokensDetails.ReasoningTokens {
		t.Errorf("reasoning_tokens %d, completion_tokens %d: hidden reasoning must still count", u.CompletionTokensDetails.ReasoningTokens, u.CompletionTokens)
	}
}

func TestOpenAIReasoningHidden(t *testing.T) {
	e := newEngine(t, fastCfg)
	w := core.NewMemWriter()
	callWith(t, e, w, "openai", nil, "/v1/chat/completions", `{"model":"wwz-fast","reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`)
	if w.StatusCode() != 200 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	var b reasoningBody
	if err := json.Unmarshal(w.Body(), &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Choices) != 1 || hasReasoningField(b.Choices[0].Message) {
		t.Fatalf("openai must not serialise the reasoning text: %s", w.Body())
	}
	if c, _ := b.Choices[0].Message["content"].(string); c == "" || strings.Contains(c, "<thinking>") {
		t.Errorf("content %q", c)
	}
	checkCounted(t, b.Usage, w.Body())
}

func TestOpenAIReasoningHiddenStream(t *testing.T) {
	const itl = 2 * time.Millisecond
	e := newEngine(t, "server: {seed: 42}\nprofiles:\n  latency:\n    fast: {ttft: 0ms, itl: 0ms, reasoning_itl: 2ms}\n")
	w := &timedWriter{MemWriter: core.NewMemWriter()}
	t0 := time.Now()
	callWith(t, e, w, "openai", nil, "/v1/chat/completions", `{"model":"wwz-fast","stream":true,"stream_options":{"include_usage":true},"reasoning_effort":"low","messages":[{"role":"user","content":"hello"}]}`)
	body := string(w.Body())
	chunks := streamChunks(t, w.Body())
	if len(chunks) < 3 {
		t.Fatalf("short stream: %s", body)
	}
	for i, c := range chunks {
		for _, ch := range c.Choices {
			if hasReasoningField(ch.Delta) {
				t.Errorf("chunk %d carries reasoning: %v", i, ch.Delta)
			}
		}
	}
	// Silence while reasoning: the role chunk comes right before the first
	// content chunk; no empty frame or comment stands in for a reasoning delta.
	if chunks[0].Choices[0].Delta["role"] != "assistant" {
		t.Errorf("first chunk %v, want role", chunks[0].Choices[0].Delta)
	}
	if c, _ := chunks[1].Choices[0].Delta["content"].(string); c == "" {
		t.Errorf("second chunk %v, want content", chunks[1].Choices[0].Delta)
	}
	if strings.HasPrefix(body, ":") || strings.Contains(body, "\n:") {
		t.Errorf("unexpected SSE comment in an openai stream: %s", body)
	}
	u := chunks[len(chunks)-1].Usage
	checkCounted(t, u, w.Body())
	// TTFT includes the reasoning time: one reasoning_itl per reasoning chunk
	// (chunk_tokens 1 by default) before the first frame.
	w.mu.Lock()
	ttft := w.firstData.Sub(t0)
	w.mu.Unlock()
	if want := itl * time.Duration(u.CompletionTokensDetails.ReasoningTokens); ttft < want {
		t.Errorf("first chunk after %v, want >= %v (reasoning_tokens %d x reasoning_itl)", ttft, want, u.CompletionTokensDetails.ReasoningTokens)
	}
}

func TestOpenCodeReasoningContentOption(t *testing.T) {
	e := newEngine(t, fastCfg)
	opts := map[string]string{"reasoning_field": "reasoning_content"}
	w := core.NewMemWriter()
	callWith(t, e, w, "opencode", opts, "/zen/go/v1/chat/completions", `{"model":"wwz-fast","reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`)
	var b reasoningBody
	if err := json.Unmarshal(w.Body(), &b); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.Choices[0].Message["reasoning_content"].(string); r == "" {
		t.Errorf("reasoning_content missing: %s", w.Body())
	}
	w = core.NewMemWriter()
	callWith(t, e, w, "opencode", opts, "/zen/go/v1/chat/completions", `{"model":"wwz-fast","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`)
	firstReasoning, lastReasoning, firstContent := -1, -1, -1
	for i, c := range streamChunks(t, w.Body()) {
		if len(c.Choices) == 0 {
			continue
		}
		if r, _ := c.Choices[0].Delta["reasoning_content"].(string); r != "" {
			if firstReasoning < 0 {
				firstReasoning = i
			}
			lastReasoning = i
		}
		if x, _ := c.Choices[0].Delta["content"].(string); x != "" && firstContent < 0 {
			firstContent = i
		}
	}
	if firstReasoning != 1 || firstContent < 0 || lastReasoning > firstContent {
		t.Errorf("reasoning_content deltas %d..%d, first content %d: want reasoning right after the role chunk", firstReasoning, lastReasoning, firstContent)
	}
}

func TestOpenRouterReasoningExclude(t *testing.T) {
	e := newEngine(t, fastCfg)
	const outPerToken = 1.5 / 1e6 // wwz-fast output price per token
	for _, stream := range []bool{false, true} {
		body := `{"model":"wwz-fast","reasoning":{"effort":"high","exclude":true},"messages":[{"role":"user","content":"hello"}]}`
		if stream {
			body = `{"model":"wwz-fast","stream":true,"stream_options":{"include_usage":true},"reasoning":{"effort":"high","exclude":true},"messages":[{"role":"user","content":"hello"}]}`
		}
		w := core.NewMemWriter()
		callWith(t, e, w, "openrouter", nil, "/api/v1/chat/completions", body)
		var u *reasoningUsage
		if stream {
			chunks := streamChunks(t, w.Body())
			if len(chunks) == 0 {
				t.Fatalf("empty stream: %s", w.Body())
			}
			for i, c := range chunks {
				for _, ch := range c.Choices {
					if hasReasoningField(ch.Delta) {
						t.Errorf("stream chunk %d carries reasoning: %v", i, ch.Delta)
					}
				}
			}
			u = chunks[len(chunks)-1].Usage
		} else {
			var b reasoningBody
			if err := json.Unmarshal(w.Body(), &b); err != nil {
				t.Fatal(err)
			}
			if len(b.Choices) != 1 || hasReasoningField(b.Choices[0].Message) {
				t.Errorf("exclude: true must omit reasoning: %s", w.Body())
			}
			u = b.Usage
		}
		checkCounted(t, u, w.Body())
		if low := float64(u.CompletionTokens) * outPerToken; u.Cost < low-1e-6 {
			t.Errorf("stream=%v cost %v, want >= %v (completion_tokens incl. reasoning)", stream, u.Cost, low)
		}
	}
}

func TestOpenRouterReasoningDisabled(t *testing.T) {
	e := newEngine(t, fastCfg)
	for _, ask := range []string{`{"enabled":false}`, `{"effort":"none"}`, `{"enabled":false,"exclude":true}`} {
		w := core.NewMemWriter()
		callWith(t, e, w, "openrouter", nil, "/api/v1/chat/completions", `{"model":"wwz-fast","reasoning":`+ask+`,"messages":[{"role":"user","content":"hello"}]}`)
		var b reasoningBody
		if err := json.Unmarshal(w.Body(), &b); err != nil {
			t.Fatal(err)
		}
		if b.Usage == nil || len(b.Choices) != 1 || b.Usage.CompletionTokensDetails.ReasoningTokens != 0 || hasReasoningField(b.Choices[0].Message) {
			t.Errorf("reasoning %s: want no reasoning, got %s", ask, w.Body())
		}
	}
}
