package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chainFixture is a recorded sequence of chained Responses calls: each step
// names the step whose response its previous_response_id points to.
type chainFixture struct {
	Name  string `json:"name"`
	Steps []struct {
		Request struct {
			Body json.RawMessage `json:"body"`
		} `json:"request"`
		Response struct {
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		} `json:"response"`
		PreviousStep *int `json:"previous_step"`
	} `json:"steps"`
}

// respUsage is the part of a response the chain tests compare.
type respUsage struct {
	ID     string `json:"id"`
	Output []struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens        int `json:"input_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// TestResponsesChainFixtures replays the chained calls recorded on
// 2026-10-31 (previous_response_instructions, previous_response_cache):
// each step says what the real one said, and input_tokens, output_tokens
// and cached_tokens must be the real ones. Instructions are not inherited
// (25 and 37 input tokens after a 1 699-token call); the recorded
// conversation is read from the prompt cache up to 1 536 tokens. The real
// missed the cache once on a replica (previous_response_cache step 2):
// the wwz hits there, by the rule.
func TestResponsesChainFixtures(t *testing.T) {
	replicaMiss := map[string]map[int]int{"previous_response_cache": {2: 1536}}
	for _, name := range []string{"previous_response_instructions", "previous_response_cache"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "real", "openai", "responses", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var fx chainFixture
			if err := json.Unmarshal(raw, &fx); err != nil || len(fx.Steps) < 3 {
				t.Fatalf("fixture: %v", err)
			}
			h := start(t, fastCfg+`
models:
  - {id: gpt-4o-mini, version: gpt-4o-mini-2024-07-18, latency: fast}
drivers: [{name: openai}]
`, nil)
			ids := make([]string, len(fx.Steps))
			for i, st := range fx.Steps {
				var real respUsage
				if err := json.Unmarshal(st.Response.Body, &real); err != nil || len(real.Output) == 0 {
					t.Fatalf("step %d: real body %v", i, err)
				}
				body := string(st.Request.Body)
				var req struct {
					Prev string `json:"previous_response_id"`
				}
				_ = json.Unmarshal(st.Request.Body, &req)
				if st.PreviousStep != nil {
					body = strings.Replace(body, req.Prev, ids[*st.PreviousStep], 1)
				}
				resp := h.do("POST", "/v1/responses", map[string]string{"authorization": "Bearer wwz-key-01", "x-wwz-response": real.Output[0].Content[0].Text}, body)
				var got respUsage
				if err := json.Unmarshal(resp.Body(), &got); err != nil || resp.StatusCode() != st.Response.Status {
					t.Fatalf("step %d: %d %s", i, resp.StatusCode(), resp.Body())
				}
				ids[i] = got.ID
				want := real.Usage
				if c, ok := replicaMiss[name][i]; ok {
					want.InputTokensDetails.CachedTokens = c
				}
				if got.Usage != want {
					t.Errorf("step %d: usage %+v, real %+v", i, got.Usage, real.Usage)
				}
			}
		})
	}
}
