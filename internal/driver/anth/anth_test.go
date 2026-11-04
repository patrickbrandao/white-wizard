package anth

import (
	"encoding/json"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// TestInputTokensExcludesCache: Anthropic's input_tokens is the prompt minus
// cache reads and writes; the three fields add up to the whole prompt.
func TestInputTokensExcludesCache(t *testing.T) {
	cases := []struct {
		u    core.Usage
		want int
	}{
		{core.Usage{PromptTokens: 100}, 100},
		{core.Usage{PromptTokens: 100, CachedTokens: 80}, 20},
		{core.Usage{PromptTokens: 100, CachedTokens: 30, CacheWriteTokens: 50}, 20},
		{core.Usage{PromptTokens: 10, CachedTokens: 30}, 0},
	}
	h := DefaultHooks()
	for _, c := range cases {
		if got := InputTokens(c.u); got != c.want {
			t.Errorf("InputTokens(%+v) = %d, want %d", c.u, got, c.want)
		}
		m := UsageJSON(&h, &core.Request{}, &core.Response{}, c.u)
		in, _ := m.Get("input_tokens")
		read, _ := m.Get("cache_read_input_tokens")
		write, _ := m.Get("cache_creation_input_tokens")
		if in != c.want || read != c.u.CachedTokens || write != c.u.CacheWriteTokens {
			t.Errorf("UsageJSON(%+v) = %v", c.u, m)
		}
	}
}

// TestStreamUsageExcludesCache: message_start and message_delta report
// input_tokens without the cached part, like the non-stream body.
func TestStreamUsageExcludesCache(t *testing.T) {
	h := DefaultHooks()
	w := core.NewMemWriter()
	s := NewStream(&h, w, &core.Request{}, nil)
	u := core.Usage{PromptTokens: 50, CompletionTokens: 4, CachedTokens: 40}
	if err := s.Start(&core.Response{ID: "msg_1", Model: "m", Usage: u}); err != nil {
		t.Fatal(err)
	}
	if err := s.TextDelta("hi"); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(core.FinishStop, u, true); err != nil {
		t.Fatal(err)
	}
	var starts, deltas int
	for _, line := range splitData(w.Body()) {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage map[string]any `json:"usage"`
			} `json:"message"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		switch ev.Type {
		case "message_start":
			starts++
			if ev.Message.Usage["input_tokens"] != float64(10) || ev.Message.Usage["cache_read_input_tokens"] != float64(40) {
				t.Errorf("message_start usage %v", ev.Message.Usage)
			}
		case "message_delta":
			deltas++
			if ev.Usage["input_tokens"] != float64(10) || ev.Usage["output_tokens"] != float64(4) {
				t.Errorf("message_delta usage %v", ev.Usage)
			}
		}
	}
	if starts != 1 || deltas != 1 {
		t.Fatalf("message_start %d, message_delta %d", starts, deltas)
	}
}

func splitData(b []byte) []string {
	var out []string
	start := 0
	for i := 0; i <= len(b); i++ {
		if i == len(b) || b[i] == '\n' {
			line := string(b[start:i])
			if len(line) > 6 && line[:6] == "data: " {
				out = append(out, line[6:])
			}
			start = i + 1
		}
	}
	return out
}

// TestAuthMessages: the 401 texts of a missing and of an unknown key
// (auth-order/nokey_*, ADR 0053).
func TestAuthMessages(t *testing.T) {
	for msg, want := range map[string]string{
		"":                               "invalid x-api-key",
		"You didn't provide an API key.": "x-api-key header is required",
		"This API key has expired.":      "This API key has expired.",
	} {
		status, body, _, _ := AnthropicErrorBody(&core.Request{}, &core.Error{Kind: core.ErrInvalidAPIKey, Message: msg})
		inner := body.(map[string]any)["error"].(map[string]any)
		if status != 401 || inner["type"] != "authentication_error" || inner["message"] != want {
			t.Errorf("%q: %d %v", msg, status, inner)
		}
	}
}

// TestWriteErrorHeaderExtras: "header:" extras of core.Error become response
// headers in the Anthropic dialect too (x-wwz-input-breakdown, x-wwz-pod...).
func TestWriteErrorHeaderExtras(t *testing.T) {
	h := DefaultHooks()
	w := core.NewMemWriter()
	e := &core.Error{Kind: core.ErrOverloaded, Extra: map[string]any{
		"header:x-wwz-pod":              "pod-1",
		"header:x-wwz-input-breakdown":  `{"total":3}`,
		"header:x-ignored-not-a-string": 7,
		"limit":                         "rpm",
	}}
	WriteError(&h, w, &core.Request{RequestID: "req_1"}, e, nil)
	if w.StatusCode() != 529 {
		t.Fatalf("status %d", w.StatusCode())
	}
	hdr := w.Headers()
	if hdr["x-wwz-pod"] != "pod-1" || hdr["x-wwz-input-breakdown"] != `{"total":3}` {
		t.Fatalf("headers %v", hdr)
	}
	if _, ok := hdr["x-ignored-not-a-string"]; ok {
		t.Fatalf("non-string extra became a header: %v", hdr)
	}
	if _, ok := hdr["limit"]; ok {
		t.Fatalf("non-header extra became a header: %v", hdr)
	}
}
