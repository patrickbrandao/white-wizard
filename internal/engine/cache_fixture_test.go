package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// The real prompt cache, recorded on 2026-10-31 (testdata/real/<provedor>/
// cache/, task 162): sequences of requests sent to OpenAI (gpt-4o-mini) and
// Anthropic (claude-haiku-4-5). Replayed against the wwz, each sequence must
// produce the same cache fields by the real rule: the same reads and writes
// are zero or not, and values the real API reports equal (a write and the
// read that follows it, say) are equal in the wwz too. The token counts
// themselves differ, because the tokenizers differ (spec 007).

// cacheFixtureCfg mirrors the real models: gpt-4o-mini (1 024 tokens, steps
// of 128) and claude-haiku-4-5 (minimum 4 096: a 1.4 k prompt is not cached,
// a 4.7 k one is). The wwz counts with its simulated tokenizer, not the
// model's, so the test scales the minimum and the step by the ratio between
// the two counts of the sequence's first prompt: the same model, measured
// in wwz tokens.
const cacheFixtureCfg = `
server: {seed: 1}
models:
  - id: gpt-4o-mini
    protocols: [openai]
    errors: none
    latency: {ttft: 0ms, itl: 0ms, clock: logical}
    context_window: 1000000
    prompt_cache: {%s}
  - id: claude-haiku-4-5
    protocols: [anthropic]
    errors: none
    latency: {ttft: 0ms, itl: 0ms, clock: logical}
    context_window: 1000000
    prompt_cache: {%s}
`

// realRule is the real cache rule of each recorded model.
var realRule = map[string][2]int{"openai": {1024, 128}, "anthropic": {4096, 0}}

func fixtureEngine(t *testing.T, provider, rule string) *engine.Engine {
	t.Helper()
	oai, anth := "kind: none", "kind: none"
	if provider == "openai" {
		oai = rule
	} else {
		anth = rule
	}
	return newEngine(t, strings.Replace(strings.Replace(cacheFixtureCfg, "%s", oai, 1), "%s", anth, 1))
}

type cacheFixture struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Steps    []struct {
		Request struct {
			Body json.RawMessage `json:"body"`
		} `json:"request"`
		Response struct {
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		} `json:"response"`
	} `json:"steps"`
}

// realUsage is the union of the cache fields of both dialects.
type realUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokens   int `json:"input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	Creation      struct {
		E5m int `json:"ephemeral_5m_input_tokens"`
		E1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// cacheFields are the compared fields of one step, in a fixed order.
func cacheFields(provider string, u realUsage) []int {
	if provider == "openai" {
		return []int{u.PromptTokensDetails.CachedTokens}
	}
	return []int{u.CacheCreation, u.CacheRead, u.Creation.E5m, u.Creation.E1h}
}

// usageDriver keeps the core usage of every response it writes, so the
// test sees the 5 min / 1 h split the engine decided.
type usageDriver struct {
	driver.Driver
	last core.Usage
}

func (d *usageDriver) WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	d.last = resp.Usage
	return d.Driver.WriteResponse(w, req, resp, rl)
}

func TestPromptCacheMatchesRealFixtures(t *testing.T) {
	files, err := filepath.Glob("../../testdata/real/*/cache/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx cacheFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		t.Run(fx.Provider+"/"+fx.Name, func(t *testing.T) {
			inner, err := driver.New(fx.Provider, nil)
			if err != nil {
				t.Fatal(err)
			}
			drv := &usageDriver{Driver: inner}
			h := driver.MapHeaders{"authorization": "Bearer wwz-key-01", "x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}
			kind := driver.KindChat
			if fx.Provider == "anthropic" {
				kind = driver.KindMessages
			}
			send := func(e *engine.Engine, body []byte) *core.MemWriter {
				w := core.NewMemWriter()
				e.Handle(context.Background(), engine.Input{Driver: drv, Kind: kind, Method: "POST", Path: fx.Path, Headers: h, Body: body, ClientIP: "127.0.0.1"}, w)
				return w
			}
			// Scale the real rule to the wwz tokenizer.
			var first struct {
				Usage realUsage `json:"usage"`
			}
			if err := json.Unmarshal(fx.Steps[0].Response.Body, &first); err != nil {
				t.Fatal(err)
			}
			realPrompt := first.Usage.PromptTokens + first.Usage.InputTokens + first.Usage.CacheCreation + first.Usage.CacheRead
			send(fixtureEngine(t, fx.Provider, "kind: none"), fx.Steps[0].Request.Body)
			ratio := float64(drv.last.PromptTokens) / float64(realPrompt)
			rr := realRule[fx.Provider]
			rule := "min_tokens: " + strconv.Itoa(int(float64(rr[0])*ratio)) + ", granularity: " + strconv.Itoa(int(float64(rr[1])*ratio))
			e := fixtureEngine(t, fx.Provider, rule)
			var real, sim [][]int
			var usageDiffs []string
			for i, st := range fx.Steps {
				var rb struct {
					Usage realUsage `json:"usage"`
				}
				if err := json.Unmarshal(st.Response.Body, &rb); err != nil || st.Response.Status != 200 {
					t.Fatalf("step %d: real status %d %v", i, st.Response.Status, err)
				}
				w := send(e, st.Request.Body)
				var wb struct {
					Usage realUsage `json:"usage"`
				}
				if err := json.Unmarshal(w.Body(), &wb); err != nil || w.StatusCode() != 200 {
					t.Fatalf("step %d: wwz status %d %v: %s", i, w.StatusCode(), err, w.Body())
				}
				// The cache fields as the driver writes them on the wire
				// (task 387): the 5 min / 1 h split of cache_creation is the
				// engine's, and the usage object has the real fields in the
				// real order, the token counts as markers.
				u := wb.Usage
				if fx.Provider == "anthropic" {
					if u.Creation.E1h != drv.last.CacheWrite1hTokens || u.Creation.E5m != drv.last.CacheWriteTokens-drv.last.CacheWrite1hTokens {
						t.Errorf("step %d: cache_creation %+v, engine wrote %d (1h %d)", i, u.Creation, drv.last.CacheWriteTokens, drv.last.CacheWrite1hTokens)
					}
					if sum := u.InputTokens + u.CacheCreation + u.CacheRead; sum != drv.last.PromptTokens {
						t.Errorf("step %d: input %d + creation %d + read %d != prompt %d", i, u.InputTokens, u.CacheCreation, u.CacheRead, drv.last.PromptTokens)
					}
				}
				if got, want := usageForm(t, w.Body()), usageForm(t, st.Response.Body); got != want {
					usageDiffs = append(usageDiffs, fmt.Sprintf("step %d: usage\n wwz  %s\n real %s", i, got, want))
				}
				real = append(real, cacheFields(fx.Provider, rb.Usage))
				sim = append(sim, cacheFields(fx.Provider, u))
			}
			compareCacheRule(t, real, sim)
			if len(usageDiffs) > 0 {
				t.Error(strings.Join(usageDiffs, "\n"))
			}
		})
	}
}

// reCount is a non-zero token count: the tokenizers differ (spec 007), so
// only zero or not is compared.
var reCount = regexp.MustCompile(`:[1-9][0-9]*`)

// usageForm is the compact "usage" object of a body with every non-zero
// count replaced by a marker, byte for byte otherwise.
func usageForm(t *testing.T, body []byte) string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	var b bytes.Buffer
	if err := json.Compact(&b, top["usage"]); err != nil {
		t.Fatalf("usage %s: %v", top["usage"], err)
	}
	return reCount.ReplaceAllString(b.String(), ":<n>")
}

// compareCacheRule requires the same zero pattern and that equal non-zero
// real values are equal in the wwz.
func compareCacheRule(t *testing.T, real, sim [][]int) {
	t.Helper()
	type cell struct{ step, field int }
	var cells []cell
	for i := range real {
		for j := range real[i] {
			if (real[i][j] == 0) != (sim[i][j] == 0) {
				t.Errorf("step %d field %d: real %d, wwz %d (zero pattern differs)", i, j, real[i][j], sim[i][j])
			}
			if real[i][j] != 0 {
				cells = append(cells, cell{i, j})
			}
		}
	}
	for a := range cells {
		for b := a + 1; b < len(cells); b++ {
			x, y := cells[a], cells[b]
			if real[x.step][x.field] == real[y.step][y.field] && sim[x.step][x.field] != sim[y.step][y.field] {
				t.Errorf("real step %d field %d = step %d field %d = %d, wwz %d != %d", x.step, x.field, y.step, y.field,
					real[x.step][x.field], sim[x.step][x.field], sim[y.step][y.field])
			}
		}
	}
	if t.Failed() {
		t.Logf("real %v\nwwz  %v", real, sim)
	}
}
