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
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// Helpers shared by the imported_i*_test.go files (spec 017, I1–I23).

// impTools offers read, write and list tools; read_file has an enum property
// so enum_mismatch has something to break.
const impTools = `"tools":[
 {"type":"function","function":{"name":"read_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"mode":{"type":"string","enum":["text","binary"]}}}}},
 {"type":"function","function":{"name":"write_file","parameters":{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}}}}},
 {"type":"function","function":{"name":"list_dir","parameters":{"type":"object","required":["dir"],"properties":{"dir":{"type":"string"}}}}}]`

var impAuth = map[string]string{"Authorization": "Bearer wwz-key-01"}

// impHeaders merges extra headers over the default auth.
func impHeaders(extra map[string]string) map[string]string {
	h := map[string]string{}
	for k, v := range impAuth {
		h[k] = v
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// impAddScenario parses a scenario YAML (so defaults apply) and registers it.
func impAddScenario(t *testing.T, e *engine.Engine, yaml string) {
	t.Helper()
	s, err := scenario.ParseYAML([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	e.Scenarios.Add(s)
}

// impBody builds an OpenAI chat body for model with the given extra JSON
// fields (leading comma included by the caller when non-empty).
func impBody(model, extra string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"do the task"}]` + extra + `}`
}

type impToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type impChat struct {
	Choices []struct {
		Message struct {
			Content   *string       `json:"content"`
			ToolCalls []impToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens            int `json:"prompt_tokens"`
		CompletionTokens        int `json:"completion_tokens"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func (c impChat) content() string {
	if len(c.Choices) == 0 || c.Choices[0].Message.Content == nil {
		return ""
	}
	return *c.Choices[0].Message.Content
}

func (c impChat) tools() []impToolCall {
	if len(c.Choices) == 0 {
		return nil
	}
	return c.Choices[0].Message.ToolCalls
}

func (c impChat) finish() string {
	if len(c.Choices) == 0 {
		return ""
	}
	return c.Choices[0].FinishReason
}

// impChatCall sends a non-streaming OpenAI chat request and decodes a 200.
func impChatCall(t *testing.T, e *engine.Engine, headers map[string]string, body string) (impChat, *core.MemWriter) {
	t.Helper()
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(headers), body)
	if w.StatusCode() != 200 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	var c impChat
	if err := json.Unmarshal(w.Body(), &c); err != nil {
		t.Fatalf("%v: %s", err, w.Body())
	}
	return c, w
}

// impStream is the decoded view of an OpenAI SSE body.
type impStream struct {
	raw       string
	texts     []string // non-empty content deltas, in order
	argFrames int      // frames carrying tool argument deltas
	frames    int      // data frames (excluding [DONE])
	finish    string
	done      bool
}

func impParseStream(t *testing.T, b []byte) impStream {
	t.Helper()
	s := impStream{raw: string(b)}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "data: [DONE]" {
			s.done = true
			continue
		}
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		s.frames++
		var c struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &c); err != nil {
			continue // malformed frames are counted, not decoded
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != "" {
				s.texts = append(s.texts, ch.Delta.Content)
			}
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Function.Arguments != "" {
					s.argFrames++
				}
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				s.finish = *ch.FinishReason
			}
		}
	}
	return s
}

// impStreamCall sends a streaming OpenAI chat request.
func impStreamCall(t *testing.T, e *engine.Engine, headers map[string]string, body string) (impStream, *core.MemWriter) {
	t.Helper()
	w := call(t, e, "openai", "/v1/chat/completions", impHeaders(headers), body)
	if w.StatusCode() != 200 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	return impParseStream(t, w.Body()), w
}

// impLastRecord returns the newest recorder entry.
func impLastRecord(t *testing.T, e *engine.Engine) engine.Record {
	t.Helper()
	recs := e.Recorder.Last(1, nil)
	if len(recs) == 0 {
		t.Fatal("no record")
	}
	return recs[0]
}

// stampWriter records the wall time of every Write carrying a data frame.
type stampWriter struct {
	*core.MemWriter
	mu     sync.Mutex
	stamps []time.Time
	// onData, when set, runs after each data frame (used to cancel).
	onData func(n int)
}

func newStampWriter() *stampWriter { return &stampWriter{MemWriter: core.NewMemWriter()} }

func (w *stampWriter) Write(p []byte) (int, error) {
	n, err := w.MemWriter.Write(p)
	if strings.Contains(string(p), "data: {") {
		w.mu.Lock()
		w.stamps = append(w.stamps, time.Now())
		k := len(w.stamps)
		cb := w.onData
		w.mu.Unlock()
		if cb != nil {
			cb(k)
		}
	}
	return n, err
}

func (w *stampWriter) Stream(fn func(core.ResponseWriter)) { fn(w) }

// impHandle runs the OpenAI chat route with a caller-provided writer and context.
func impHandle(ctx context.Context, t *testing.T, e *engine.Engine, w core.ResponseWriter, headers map[string]string, body string) {
	t.Helper()
	drv, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := driver.MapHeaders{}
	for k, v := range impHeaders(headers) {
		h[strings.ToLower(k)] = v
	}
	e.Handle(ctx, engine.Input{Driver: drv, Kind: driver.KindChat, Method: "POST", Path: "/v1/chat/completions", Headers: h, Body: []byte(body), ClientIP: "127.0.0.1"}, w)
}
