package generate_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/tokenize"
)

// toolFixture is a recording of testdata/real/<provider>/tools/.
type toolFixture struct {
	Name    string            `json:"name"`
	Compare map[string]string `json:"compare"`
	Request struct {
		Path string `json:"path"`
		Body string `json:"body"`
	} `json:"request"`
	Response struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	} `json:"response"`
	tokens int
}

func loadToolFixtures(t *testing.T, provider string) map[string]*toolFixture {
	t.Helper()
	return loadFixtureDir(t, filepath.Join("..", "..", "testdata", "real", provider, "tools"))
}

// loadFixtureDir reads the fixtures of one directory, all accepted, with
// the prompt count of count_tokens, Messages or Chat.
func loadFixtureDir(t *testing.T, dir string) map[string]*toolFixture {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("fixtures %s: %d %v", dir, len(files), err)
	}
	out := map[string]*toolFixture{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx toolFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var usage struct {
			InputTokens int `json:"input_tokens"`
			Usage       struct {
				Input  int `json:"input_tokens"`
				Prompt int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if fx.Response.Status != 200 || json.Unmarshal([]byte(fx.Response.Body), &usage) != nil {
			t.Fatalf("%s: status %d", fx.Name, fx.Response.Status)
		}
		fx.tokens = usage.InputTokens + usage.Usage.Input + usage.Usage.Prompt
		out[fx.Name] = &fx
	}
	return out
}

// oracle is a Tokenizer that knows the real Claude count of the texts the
// probe fixtures measured (a user message with the text costs 7 + its
// tokens); any other text fails the test.
type oracle struct {
	t      *testing.T
	counts map[string]int
}

func (o oracle) Count(text string) int {
	n, ok := o.counts[text]
	if !ok {
		o.t.Fatalf("no real count for %q", text)
	}
	return n
}

func (o oracle) Split(string) []string { return nil }

func parseAnthropic(t *testing.T, fx *toolFixture) *core.Request {
	t.Helper()
	d, err := driver.New("anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	kind := driver.KindMessages
	if strings.HasSuffix(fx.Request.Path, "/count_tokens") {
		kind = driver.KindCountTokens
	}
	req, perr := d.ParseRequest(kind, []byte(fx.Request.Body), driver.MapHeaders{"anthropic-version": "2023-06-01"})
	if perr != nil {
		t.Fatalf("%s: %s", fx.Name, perr.Message)
	}
	return req
}

// TestRealFixturesAnthropicTools checks the Claude tools rule against
// claude-haiku-4-5 (testdata/real/anthropic/tools/, task 259) and the same
// requests on claude-fable-5 and claude-haiku-5-5 (tools-<model>/, task
// 264):
//   - every probe_<set> fixture asked count_tokens of exactly
//     AnthropicToolsTextFor of that tool set (UTF-8 from Claude 4.6 on);
//   - with the real counts of those texts, ToolSchemaTokens equals the real
//     cost of the tools (the case minus its no-tools base) in every case,
//     which pins the fixed overheads exactly;
//   - with the simulated tokenizer the wwz uses for Claude, the error stays
//     small (logged; the text part is approximate by design, ADR 0053);
//   - /v1/messages charges the same input_tokens as count_tokens.
func TestRealFixturesAnthropicTools(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "real", "anthropic")
	for _, c := range []struct {
		dir      string
		base     int
		messages int
	}{
		{"tools", 8, 6},
		{"tools-claude-fable-5", 7, 0},
		{"tools-claude-haiku-5-5", 9, 0},
	} {
		t.Run(c.dir, func(t *testing.T) {
			checkAnthropicTools(t, loadFixtureDir(t, filepath.Join(root, c.dir)), c.base, c.messages)
		})
	}
}

func checkAnthropicTools(t *testing.T, fxs map[string]*toolFixture, base, wantMessages int) {
	baseX := fxs["base_x"]
	if baseX == nil || baseX.tokens != base {
		t.Fatalf("base_x: a user message with 'x' costs %d", base)
	}
	counts := probeCounts(t, fxs)
	ora := oracle{t: t, counts: counts}
	var cases, messages int
	var absErr, relErr, worst float64
	names := make([]string, 0, len(fxs))
	for name := range fxs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fx := fxs[name]
		if ct := fx.Compare["count_tokens"]; ct != "" {
			if fxs[ct] == nil || fxs[ct].tokens != fx.tokens {
				t.Errorf("%s: messages %d, count_tokens %s differs", name, fx.tokens, ct)
			}
			req := parseAnthropic(t, fx)
			if req.Endpoint != core.EndpointMessages {
				t.Errorf("%s: endpoint %s", name, req.Endpoint)
			}
			messages++
			continue
		}
		base, probe := fxs[fx.Compare["base"]], fxs[fx.Compare["probe"]]
		if base == nil || probe == nil {
			continue
		}
		cases++
		req := parseAnthropic(t, fx)
		var pb struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal([]byte(probe.Request.Body), &pb)
		if got := generate.AnthropicToolsTextFor(req.Model, req.Tools); got != pb.Messages[0].Content {
			t.Errorf("%s: rendering differs from %s:\n%s\n%s", name, probe.Name, got, pb.Messages[0].Content)
			continue
		}
		real := fx.tokens - base.tokens
		if got := generate.ToolSchemaTokens(ora, req); got != real {
			t.Errorf("%s: tools cost %d, real %d", name, got, real)
		}
		sim := generate.ToolSchemaTokens(generate.Simulated{}, req)
		e := float64(sim - real)
		absErr += math.Abs(e)
		rel := math.Abs(e) / float64(real)
		relErr += rel
		worst = math.Max(worst, rel)
		t.Logf("%-28s real %4d simulated %4d (%+d)", name, real, sim, sim-real)
		// The whole prompt, with the wwz's single-user-message rule.
		if fx.Compare["base"] == "base_x" {
			if total := generate.CountPrompt(ora, req, 0).Total; total != fx.tokens {
				t.Errorf("%s: total %d, real %d", name, total, fx.tokens)
			}
		}
	}
	if cases < 55 || messages < wantMessages {
		t.Fatalf("cases %d, messages %d", cases, messages)
	}
	t.Logf("simulated: mean |error| %.1f tokens, mean %.2f%%, worst %.2f%% (%d cases)", absErr/float64(cases), 100*relErr/float64(cases), 100*worst, cases)
	if worst > 0.05 {
		t.Errorf("simulated tokenizer off by %.1f%% on the tools prompt", 100*worst)
	}
}

// TestRealFixturesOpenRouterTools: openai/gpt-4.1-nano through OpenRouter
// (testdata/real/openrouter/tools/, task 259) counts exactly the real
// prompt_tokens with o200k_base and the GPT-4.1 template.
func TestRealFixturesOpenRouterTools(t *testing.T) {
	fxs := loadToolFixtures(t, "openrouter")
	if len(fxs) < 90 {
		t.Fatalf("fixtures: %d", len(fxs))
	}
	for name, fx := range fxs {
		req, perr := oai.ParseChat([]byte(fx.Request.Body), nil, "openrouter")
		if perr != nil {
			t.Fatalf("%s: %s", name, perr.Message)
		}
		if got := generate.CountPrompt(tokenize.O200k, req, 85); got.Total != fx.tokens {
			t.Errorf("%s: %d tokens, real %d (%+v)", name, got.Total, fx.tokens, got)
		}
	}
}
