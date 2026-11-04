package generate_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/tokenize"
)

// TestRealFixturesToolTokens: every request recorded against gpt-4o-mini in
// testdata/real/openai/tools/ (tool definitions, tool_choice,
// parallel_tool_calls, json_schema, tool-call history; Chat Completions and
// Responses, task 252) counts exactly the real prompt_tokens/input_tokens.
func TestRealFixturesToolTokens(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "real", "openai", "tools", "*.json"))
	if err != nil || len(files) < 100 {
		t.Fatalf("fixtures: %d %v", len(files), err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Name    string `json:"name"`
			Request struct {
				Path string `json:"path"`
				Body string `json:"body"`
			} `json:"request"`
			Response struct {
				Status int    `json:"status"`
				Body   string `json:"body"`
			} `json:"response"`
		}
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var usage struct {
			Usage struct {
				Prompt int `json:"prompt_tokens"`
				Input  int `json:"input_tokens"`
			} `json:"usage"`
		}
		if fx.Response.Status != 200 || json.Unmarshal([]byte(fx.Response.Body), &usage) != nil {
			t.Fatalf("%s: status %d", fx.Name, fx.Response.Status)
		}
		var req *core.Request
		var perr *core.Error
		want := usage.Usage.Prompt
		if strings.HasSuffix(fx.Request.Path, "/responses") {
			req, perr = oai.ParseResponses([]byte(fx.Request.Body), "openai")
			want = usage.Usage.Input
		} else {
			req, perr = oai.ParseChat([]byte(fx.Request.Body), nil, "openai")
		}
		if perr != nil {
			t.Fatalf("%s: %s", fx.Name, perr.Message)
		}
		if got := generate.CountPrompt(tokenize.O200k, req, 85); got.Total != want {
			t.Errorf("%s: %d tokens, real %d (%+v)", fx.Name, got.Total, want, got)
		}
	}
}
