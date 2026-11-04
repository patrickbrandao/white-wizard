package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestResponsesPromptCacheFixture replays cache_automatic_prefix (task
// 201): the same 1 697-token prompt three times, then streamed. The usage
// object is the real one, cache_write_tokens before cached_tokens and the
// same input and output counts; only cached_tokens follows the rule of spec
// 007 (task 162): from the 2nd call on, the largest 1 024 + 128·k cut of
// the prompt, 1 664. The real hit only on the 3rd call, with 1 536: its hit
// is not deterministic across replicas and one sample does not tell whether
// the Responses API keeps the last block out of the cache (not conferred).
func TestResponsesPromptCacheFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "real", "openai", "responses", "cache_automatic_prefix.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Steps []struct {
			Request struct {
				Body json.RawMessage `json:"body"`
			} `json:"request"`
			Response struct {
				Body  json.RawMessage `json:"body"`
				Usage json.RawMessage `json:"usage"`
			} `json:"response"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil || len(fx.Steps) != 4 {
		t.Fatalf("fixture: %v", err)
	}
	h := start(t, fastCfg+`
models:
  - {id: gpt-4o-mini, version: gpt-4o-mini-2024-07-18, latency: fast}
drivers: [{name: openai}]
`, nil)
	usageRe := regexp.MustCompile(`"usage":(\{"input_tokens".*?"total_tokens":[0-9]+\})`)
	realCached := regexp.MustCompile(`"cached_tokens":[0-9]+`)
	wantCached := []string{"0", "1536", "1536", "1536"}
	for i, st := range fx.Steps {
		resp := h.do("POST", "/v1/responses", map[string]string{"authorization": "Bearer wwz-key-01", "x-wwz-response": "Ok."}, string(st.Request.Body))
		if resp.StatusCode() != 200 {
			t.Fatalf("step %d: %d %s", i, resp.StatusCode(), resp.Body())
		}
		realUsage := st.Response.Usage
		if len(realUsage) == 0 {
			var b struct {
				Usage json.RawMessage `json:"usage"`
			}
			if err := json.Unmarshal(st.Response.Body, &b); err != nil {
				t.Fatal(err)
			}
			realUsage = b.Usage
		}
		var got string
		if body := string(resp.Body()); strings.HasPrefix(body, "event:") {
			m := usageRe.FindAllStringSubmatch(body, -1)
			if len(m) == 0 {
				t.Fatalf("step %d: no usage in the stream", i)
			}
			got = m[len(m)-1][1]
		} else {
			var b struct {
				Usage json.RawMessage `json:"usage"`
			}
			if err := json.Unmarshal(resp.Body(), &b); err != nil {
				t.Fatal(err)
			}
			got = compactJSON(t, b.Usage)
		}
		want := realCached.ReplaceAllString(compactJSON(t, realUsage), `"cached_tokens":`+wantCached[i])
		if got != want {
			t.Errorf("step %d: usage\n wwz  %s\n want %s", i, got, want)
		}
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var c bytes.Buffer
	if err := json.Compact(&c, raw); err != nil {
		t.Fatal(err)
	}
	return c.String()
}
