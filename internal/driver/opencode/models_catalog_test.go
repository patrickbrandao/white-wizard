package opencode_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// The listing by catalog (ADR 0072, items 15 and 16; spec 006 16.7
// "OpenCode Go"): the public catalogs recorded without a key on
// 2026-11-03 (testdata/real/opencode/catalog/), 79 pay-as-you-go models
// and 28 Go ones; /zen lists the same as /inference.

type catalogFixture struct {
	Request struct {
		Path string `json:"path"`
	} `json:"request"`
	Response struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	} `json:"response"`
}

// catalogItems reads the items of a catalog fixture, each field kept raw
// (in the real order).
func catalogItems(t *testing.T, name string) (path string, items []map[string]json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "opencode", "catalog", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx catalogFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var list struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(fx.Response.Body), &list); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fx.Request.Path, list.Data
}

// realCost is the part of cost the configuration declares.
type realCost struct {
	Text struct {
		Input struct {
			Price float64            `json:"price"`
			Cache map[string]float64 `json:"cache"`
			Over  []struct {
				When  map[string]any     `json:"when"`
				Price *float64           `json:"price"`
				Cache map[string]float64 `json:"cache"`
			} `json:"overrides"`
		} `json:"input"`
		Output struct {
			Price float64 `json:"price"`
			Over  []struct {
				When  map[string]any `json:"when"`
				Price float64        `json:"price"`
			} `json:"overrides"`
		} `json:"output"`
	} `json:"text"`
	Audio       any `json:"audio"`
	Multipliers []struct {
		When   map[string]any `json:"when"`
		Factor float64        `json:"factor"`
	} `json:"multipliers"`
}

// pricingOf is the models[].pricing block that prices an item as the real
// does: prices, multipliers and overrides (an input override and the
// output one with the same when are one entry), parsed as the YAML is.
func pricingOf(t *testing.T, id string, c realCost) *config.Pricing {
	t.Helper()
	in := c.Text.Input
	block := map[string]any{
		"input_per_m": in.Price, "output_per_m": c.Text.Output.Price,
		"cache_read_per_m": in.Cache["read"], "cache_write_per_m": in.Cache["write"],
	}
	if len(c.Multipliers) > 0 {
		var ms []any
		for _, m := range c.Multipliers {
			ms = append(ms, map[string]any{"when": m.When, "factor": m.Factor})
		}
		block["multipliers"] = ms
	}
	if len(in.Over) != len(c.Text.Output.Over) {
		t.Fatalf("%s: %d input overrides, %d output ones", id, len(in.Over), len(c.Text.Output.Over))
	}
	var os []any
	for i, o := range in.Over {
		out := c.Text.Output.Over[i]
		if a, b := mustJSON(t, o.When), mustJSON(t, out.When); a != b {
			t.Fatalf("%s: override %d: input when %s, output when %s", id, i, a, b)
		}
		e := map[string]any{"when": o.When, "output_per_m": out.Price}
		if o.Price != nil {
			e["input_per_m"] = *o.Price
		}
		for k, v := range o.Cache {
			e["cache_"+k+"_per_m"] = v
		}
		os = append(os, e)
	}
	if len(os) > 0 {
		block["overrides"] = os
	}
	var p config.Pricing
	if err := yaml.Unmarshal([]byte(mustJSON(t, block)), &p); err != nil {
		t.Fatalf("%s: pricing: %v", id, err)
	}
	return &p
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// dialectOf is the models[].protocols dialect of an api protocol.
var dialectOf = map[string]string{
	"chat-completions": config.DialectOpenAI, "responses": config.DialectOpenAI,
	"messages": config.DialectAnthropic, "generate-content": config.DialectGemini,
}

// realSDK is the sdk the real lists for most models of each protocol (the
// one the driver derives); mistral-large-4 has its own, which extra keeps.
var realSDK = map[string]map[string]any{
	"chat-completions": {"aisdk": "@ai-sdk/openai-compatible", "opencodeai": "@opencode/ai/providers/openai-compatible"},
	"responses":        {"aisdk": "@ai-sdk/openai", "opencodeai": "@opencode/ai/providers/openai/responses"},
	"messages":         {"aisdk": "@ai-sdk/anthropic", "opencodeai": "@opencode/ai/providers/anthropic"},
	"generate-content": {"aisdk": "@ai-sdk/google", "opencodeai": "@opencode/ai/providers/google"},
}

// defaultAPI is the api entry (without base_url and sdk) the driver
// derives for protocol without extra.api.
func defaultAPI(protocol string, reasoning, tools bool) map[string]any {
	e := map[string]any{}
	if protocol == "chat-completions" && reasoning {
		e["compatibility"] = map[string]any{"reasoning_field": "reasoning_content"}
	}
	choice := []any{"auto"}
	if tools {
		choice = []any{"auto", "none", "required", "tool"}
	}
	e["capabilities"] = map[string]any{"tools": map[string]any{"choice": choice, "updates": false}}
	return e
}

// catalogModel is the models[] entry of a fixture item in catalog: the
// price in catalogs.<catalog>.pricing (models[].pricing is a decoy), the
// protocol in protocols, and extra only for what the configuration has no
// field for: canonical_id (null when the real has none), the api
// capabilities that differ from the derived ones (always for responses),
// an sdk of its own, cost.audio and a cache.write of 0. derived counts the items whose api
// came from protocols alone.
func catalogModel(t *testing.T, catalog string, item map[string]json.RawMessage, derived *int) *config.ModelConfig {
	t.Helper()
	var id string
	_ = json.Unmarshal(item["id"], &id)
	var cost realCost
	if err := json.Unmarshal(item["cost"], &cost); err != nil {
		t.Fatalf("%s: cost: %v", id, err)
	}
	var caps struct {
		Tools     struct{ Supported bool } `json:"tools"`
		Reasoning struct{ Supported bool } `json:"reasoning"`
	}
	_ = json.Unmarshal(item["capabilities"], &caps)
	var api map[string]map[string]any
	if err := json.Unmarshal(item["api"], &api); err != nil || len(api) != 1 {
		t.Fatalf("%s: api %s (%v)", id, item["api"], err)
	}
	extra := map[string]any{"canonical_id": nil}
	if raw, ok := item["canonical_id"]; ok {
		var cid string
		_ = json.Unmarshal(raw, &cid)
		extra["canonical_id"] = cid
	}
	m := &config.ModelConfig{
		ID:       id,
		Pricing:  config.Pricing{InputPerM: 99, OutputPerM: 99},
		Catalogs: map[string]config.CatalogEntry{catalog: {Pricing: pricingOf(t, id, cost)}},
		Capabilities: config.Capabilities{
			Tools: &caps.Tools.Supported, Reasoning: &caps.Reasoning.Supported,
		},
		Extra: extra,
	}
	for proto, e := range api {
		m.Protocols = []string{dialectOf[proto]}
		delete(e, "base_url")
		if mustJSON(t, e["sdk"]) == mustJSON(t, realSDK[proto]) {
			delete(e, "sdk")
		}
		if _, own := e["sdk"]; proto == "responses" || own || mustJSON(t, e) != mustJSON(t, defaultAPI(proto, caps.Reasoning.Supported, caps.Tools.Supported)) {
			extra["api"] = map[string]any{proto: e}
		} else {
			*derived++
		}
	}
	if cost.Audio != nil {
		extra["cost"] = map[string]any{"audio": cost.Audio}
	}
	if w, ok := cost.Text.Input.Cache["write"]; ok && w == 0 {
		extra["cost"] = map[string]any{"text": map[string]any{"input": map[string]any{"cache": map[string]any{"write": 0}}}}
	}
	return m
}

// opencodeAI is a request on path whose origin, computed by the server
// (driver.OriginHeader, ADR 0073), is https://opencode.ai.
func opencodeAI(path string) *core.Request {
	return &core.Request{Headers: map[string]string{
		driver.OriginHeader: "https://opencode.ai", "x-wwz-path": path,
	}}
}

// listing lists models on req and returns its items, each field raw.
func listing(t *testing.T, req *core.Request, models []driver.ModelInfo) []map[string]json.RawMessage {
	t.Helper()
	w := core.NewMemWriter()
	newDriver(t).ListModels(w, req, models)
	var list struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body(), &list); err != nil {
		t.Fatalf("listing %s: %v", w.Body(), err)
	}
	return list.Data
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b bytes.Buffer
	if len(raw) == 0 {
		return ""
	}
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// sameFields compares the listed fields of got and want, item by item.
func sameFields(t *testing.T, name string, got, want []map[string]json.RawMessage, fields ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d items, real %d", name, len(got), len(want))
	}
	for i := range want {
		for _, f := range fields {
			if g, r := compact(t, got[i][f]), compact(t, want[i][f]); g != r {
				t.Errorf("%s: %s %s\n wwz  %s\n real %s", name, want[i]["id"], f, g, r)
			}
		}
	}
}

// TestCatalogListingMatchesReal: the configuration of every model of the
// two public catalogs (price, multipliers, overrides, protocol and
// canonical_id) lists cost, api and canonical_id byte for byte as the
// real, item by item; /zen and /zen/go list the same as /inference and
// /inference/go, base_url in /inference.
func TestCatalogListingMatchesReal(t *testing.T) {
	for _, tc := range []struct {
		fixture, zen, catalog string
		n                     int
	}{
		{"payg_models", "zen_models", "opencode", 79},
		{"go_models", "zen_go_models", "opencode-go", 28},
	} {
		t.Run(tc.catalog, func(t *testing.T) {
			path, real := catalogItems(t, tc.fixture)
			if len(real) != tc.n {
				t.Fatalf("%s lists %d models, want %d", tc.fixture, len(real), tc.n)
			}
			derived := 0
			models := make([]driver.ModelInfo, len(real))
			for i, item := range real {
				models[i] = driver.ModelInfo{Cfg: catalogModel(t, tc.catalog, item, &derived), Catalog: tc.catalog}
			}
			if derived == 0 {
				t.Error("no item had its api derived from protocols alone")
			}
			got := listing(t, opencodeAI(path), models)
			sameFields(t, tc.fixture, got, real, "id", "canonical_id", "cost", "api")
			for i := range got {
				if _, has := real[i]["canonical_id"]; !has {
					if _, listed := got[i]["canonical_id"]; listed {
						t.Errorf("%s: canonical_id listed, the real has none", real[i]["id"])
					}
				}
			}

			zenPath, zenReal := catalogItems(t, tc.zen)
			zen := listing(t, opencodeAI(zenPath), models)
			sameFields(t, tc.zen, zen, zenReal, "id", "canonical_id", "cost", "api")
			for i := range zen {
				for k, v := range got[i] {
					if k != "created" && compact(t, zen[i][k]) != compact(t, v) {
						t.Errorf("%s: %s differs from %s: %s", zenPath, k, path, zen[i][k])
					}
				}
				if len(zen[i]) != len(got[i]) {
					t.Errorf("%s: %d fields, %s %d", zenPath, len(zen[i]), path, len(got[i]))
				}
			}
		})
	}
}

// TestCatalogSamePriceDiffers: deepseek-v4-pro is 1.74/3.48 on
// /inference/v1/models and 0.66/1.98 on /inference/go/v1/models; one
// models[] entry with both catalogs lists each catalog's price, as the
// real.
func TestCatalogSamePriceDiffers(t *testing.T) {
	pick := func(fixture string) map[string]json.RawMessage {
		_, items := catalogItems(t, fixture)
		for _, it := range items {
			if string(it["id"]) == `"deepseek-v4-pro"` {
				return it
			}
		}
		t.Fatalf("%s: no deepseek-v4-pro", fixture)
		return nil
	}
	payg, gov := pick("payg_models"), pick("go_models")
	n := 0
	m := catalogModel(t, "opencode", payg, &n)
	m.Catalogs["opencode-go"] = catalogModel(t, "opencode-go", gov, &n).Catalogs["opencode-go"]
	for _, tc := range []struct {
		path, catalog string
		real          map[string]json.RawMessage
		price         string
	}{
		{"/inference/v1/models", "opencode", payg, `"price":1.74`},
		{"/inference/go/v1/models", "opencode-go", gov, `"price":0.66`},
	} {
		got := listing(t, opencodeAI(tc.path), []driver.ModelInfo{{Cfg: m, Catalog: tc.catalog}})
		sameFields(t, tc.path, got, []map[string]json.RawMessage{tc.real}, "cost")
		if !bytes.Contains(got[0]["cost"], []byte(tc.price)) {
			t.Errorf("%s: cost %s, want %s", tc.path, got[0]["cost"], tc.price)
		}
	}
}

// apiOf lists one model and returns its api and canonical_id.
func apiOf(t *testing.T, req *core.Request, m driver.ModelInfo) (api string, canonical json.RawMessage) {
	t.Helper()
	got := listing(t, req, []driver.ModelInfo{m})
	return compact(t, got[0]["api"]), got[0]["canonical_id"]
}

// TestListingAPIByProtocol: api has exactly the native protocol, with
// base_url absolute (scheme, host and prefix of the request) in the
// /inference family of the catalog and the real sdk.
func TestListingAPIByProtocol(t *testing.T) {
	no := false
	chatSDK := `"sdk":{"aisdk":"@ai-sdk/openai-compatible","opencodeai":"@opencode/ai/providers/openai-compatible"}`
	tail := `"capabilities":{"tools":{"choice":["auto"],"updates":false}}`
	for _, tc := range []struct {
		name      string
		protocols []string
		extra     map[string]any
		catalog   string
		req       *core.Request
		want      string
	}{
		{
			"no protocols", nil, nil, "opencode", opencodeAI("/inference/v1/models"),
			`{"chat-completions":{"base_url":"https://opencode.ai/inference/openai/v1",` + chatSDK + `,` + tail + `}}`,
		},
		{
			"star",
			[]string{"*"},
			nil, "opencode-go", opencodeAI("/zen/go/v1/models"),
			`{"chat-completions":{"base_url":"https://opencode.ai/inference/go/openai/v1",` + chatSDK + `,` + tail + `}}`,
		},
		{
			"anthropic first",
			[]string{"anthropic", "openai"},
			nil, "opencode", opencodeAI("/zen/v1/models"),
			`{"messages":{"base_url":"https://opencode.ai/inference/anthropic/v1","sdk":{"aisdk":"@ai-sdk/anthropic","opencodeai":"@opencode/ai/providers/anthropic"},` + tail + `}}`,
		},
		{
			"gemini",
			[]string{"gemini"},
			nil, "opencode-go", opencodeAI("/inference/go/v1/models"),
			`{"generate-content":{"base_url":"https://opencode.ai/inference/go/google/v1beta","sdk":{"aisdk":"@ai-sdk/google","opencodeai":"@opencode/ai/providers/google"},` + tail + `}}`,
		},
		{
			"responses by extra", nil,
			map[string]any{"api": map[string]any{"responses": map[string]any{}}},
			"opencode",
			&core.Request{Headers: map[string]string{driver.OriginHeader: "http://wwz:8080", "x-wwz-path": "/inference/v1/models"}},
			`{"responses":{"base_url":"http://wwz:8080/inference/openai/v1","sdk":{"aisdk":"@ai-sdk/openai","opencodeai":"@opencode/ai/providers/openai/responses"}}}`,
		},
		{
			"explicit base_url wins", nil,
			map[string]any{"api": map[string]any{"messages": map[string]any{"base_url": "https://x/y"}}},
			"opencode", opencodeAI("/inference/v1/models"),
			`{"messages":{"base_url":"https://x/y","sdk":{"aisdk":"@ai-sdk/anthropic","opencodeai":"@opencode/ai/providers/anthropic"}}}`,
		},
		{"origin with external and mount prefixes", nil, nil, "", &core.Request{Headers: map[string]string{
			driver.OriginHeader: "https://wwz.example/edge/opencode", "x-wwz-path": "/zen/v1/models",
		}}, `{"chat-completions":{"base_url":"https://wwz.example/edge/opencode/inference/openai/v1",` + chatSDK + `,` + tail + `}}`},
		{"host and x-forwarded-* not read, catalog from the route", nil, nil, "", &core.Request{Headers: map[string]string{
			"host": "wwz", "x-forwarded-host": "evil", "x-forwarded-proto": "https", "x-forwarded-prefix": "/x", "x-wwz-path": "/inference/go/v1/models",
		}}, `{"chat-completions":{"base_url":"/inference/go/openai/v1",` + chatSDK + `,` + tail + `}}`},
		{"relative origin (no host)", nil, nil, "", &core.Request{Headers: map[string]string{
			driver.OriginHeader: "/opencode", "x-wwz-path": "/inference/go/v1/models",
		}}, `{"chat-completions":{"base_url":"/opencode/inference/go/openai/v1",` + chatSDK + `,` + tail + `}}`},
		{"no host: relative", nil, nil, "opencode", &core.Request{}, `{"chat-completions":{"base_url":"/inference/openai/v1",` + chatSDK + `,` + tail + `}}`},
		{"unknown protocol: nothing derived", nil, map[string]any{"api": map[string]any{"realtime": map[string]any{"x": 1}}}, "opencode", nil, `{"realtime":{"x":1}}`},
		{"api null", nil, map[string]any{"api": nil}, "opencode", nil, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := driver.ModelInfo{Catalog: tc.catalog, Cfg: &config.ModelConfig{
				ID: "m-1", Protocols: tc.protocols, Extra: tc.extra,
				Capabilities: config.Capabilities{Tools: &no},
			}}
			if got, _ := apiOf(t, tc.req, m); got != tc.want {
				t.Errorf("api\n got  %s\n want %s", got, tc.want)
			}
		})
	}
}

// TestListingExtraNullAndPlainPricing: canonical_id is derived, taken
// from extra or removed by its null (as any key of extra, nested ones
// too); a pricing without multipliers, overrides or catalogs lists the
// plain cost; a free model lists cache.read 0, as the real.
func TestListingExtraNullAndPlainPricing(t *testing.T) {
	cfg := &config.ModelConfig{ID: "kimi-k3", OwnedBy: "moonshotai", Pricing: config.Pricing{InputPerM: 3, OutputPerM: 15}}
	m := driver.ModelInfo{Cfg: cfg}
	if _, cid := apiOf(t, nil, m); string(cid) != `"moonshotai/kimi-k3"` {
		t.Errorf("derived canonical_id %s", cid)
	}
	got := listing(t, nil, []driver.ModelInfo{m})
	if c := compact(t, got[0]["cost"]); c != `{"text":{"input":{"price":3},"output":{"price":15}}}` {
		t.Errorf("plain cost %s", c)
	}
	cfg.Extra = map[string]any{
		"canonical_id": nil,
		"limit":        map[string]any{"output": nil},
		"modalities":   map[string]any{"input": nil},
		"cost":         map[string]any{"audio": map[string]any{"input": 1, "output": nil}},
	}
	got = listing(t, nil, []driver.ModelInfo{m})
	if _, ok := got[0]["canonical_id"]; ok {
		t.Errorf("canonical_id: null left %s", got[0]["canonical_id"])
	}
	for f, want := range map[string]string{
		"limit":      `{"context":0}`,
		"modalities": `{"output":["text"]}`,
		"cost":       `{"text":{"input":{"price":3},"output":{"price":15}},"audio":{"input":1}}`,
	} {
		if g := compact(t, got[0][f]); g != want {
			t.Errorf("%s = %s, want %s", f, g, want)
		}
	}
	cfg.Extra = map[string]any{"canonical_id": "reflection/beam-1.0-501b-a23b"}
	if _, cid := apiOf(t, nil, m); string(cid) != `"reflection/beam-1.0-501b-a23b"` {
		t.Errorf("extra canonical_id %s", cid)
	}

	var free config.Pricing
	if err := yaml.Unmarshal([]byte(`{input_per_m: 0, output_per_m: 0}`), &free); err != nil {
		t.Fatal(err)
	}
	cfg.Catalogs = map[string]config.CatalogEntry{"opencode": {Pricing: &free}}
	got = listing(t, nil, []driver.ModelInfo{{Cfg: cfg, Catalog: "opencode"}})
	if c := compact(t, got[0]["cost"]); c != `{"text":{"input":{"price":0,"cache":{"read":0}},"output":{"price":0}}}` {
		t.Errorf("free cost %s", c)
	}
	if got := listing(t, nil, []driver.ModelInfo{{Cfg: cfg, Catalog: "opencode-go"}}); compact(t, got[0]["cost"]) != `{"text":{"input":{"price":3},"output":{"price":15}}}` {
		t.Errorf("a catalog without pricing lists the model's: %s", got[0]["cost"])
	}
}

// spec003CatalogFields is the "campos por catálogo" yaml block of spec 003
// (revision of task 434), as a raw document.
func spec003CatalogFields(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "specs", "003-configuracao.md"))
	if err != nil {
		t.Fatal(err)
	}
	const head = "```yaml\n# OpenCode: campos por catálogo (ADR 0072, revisão da task 434)\n"
	doc := string(data)
	start := strings.Index(doc, head)
	if start < 0 {
		t.Fatal("no catalog fields block in spec 003")
	}
	body := doc[start+len("```yaml\n"):]
	body = body[:strings.Index(body, "\n```")]
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// deepSet merges over into base (objects key by key; anything else
// replaces).
func deepSet(base, over map[string]any) {
	for k, v := range over {
		if vm, ok := v.(map[string]any); ok {
			if bm, ok := base[k].(map[string]any); ok {
				deepSet(bm, vm)
				continue
			}
		}
		base[k] = v
	}
}

// catalogFieldsRest is what the spec block leaves out of minimax-m3 and
// deepseek-v4-pro and both catalogs share, or that the block does not
// show (minimax-m3's description and system_updates in Go): display name,
// limits and capabilities of the model, and the item fields without a
// field of their own.
const catalogFieldsRest = `
minimax-m3:
  display_name: MiniMax-M3
  capabilities: {reasoning: true}
  extra:
    description: MiniMax multimodal model for long-context coding, perception, and agent planning
    knowledge: null
    capabilities: {reasoning: {toggle: true, effort: null}, structured_output: null, top_k: false, system_updates: false}
  catalogs:
    opencode-go:
      extra:
        description: MiniMax multimodal coding model for long-context reasoning and agent tasks
        capabilities: {system_updates: true}
deepseek-v4-pro:
  display_name: DeepSeek V4 Pro
  context_window: 1000000
  max_output: 384000
  capabilities: {reasoning: true, json_schema: true}
  extra: {family: deepseek-thinking, knowledge: "2025-05"}
`

// TestCatalogFieldsMatchReal: the spec 003 block, completed with what
// both catalogs share, lists minimax-m3 and deepseek-v4-pro on
// /inference/v1/models and /inference/go/v1/models with the real name,
// description, family, knowledge, limit, capabilities, cost and api
// protocol of each catalog, whether the driver gets the model effective
// (as the engine hands it) or as declared.
func TestCatalogFieldsMatchReal(t *testing.T) {
	raw := spec003CatalogFields(t)
	var rest map[string]any
	if err := yaml.Unmarshal([]byte(catalogFieldsRest), &rest); err != nil {
		t.Fatal(err)
	}
	for _, m := range raw["models"].([]any) {
		mm := m.(map[string]any)
		if r, ok := rest[mm["id"].(string)].(map[string]any); ok {
			deepSet(mm, r)
		}
	}
	b, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(b)
	if err != nil {
		t.Fatalf("spec 003 block: %v\n%s", err, b)
	}
	for _, tc := range []struct{ fixture, catalog string }{
		{"payg_models", "opencode"},
		{"go_models", "opencode-go"},
	} {
		path, items := catalogItems(t, tc.fixture)
		real := map[string]map[string]json.RawMessage{}
		for _, it := range items {
			var id string
			_ = json.Unmarshal(it["id"], &id)
			real[id] = it
		}
		for _, id := range []string{"minimax-m3", "deepseek-v4-pro"} {
			m := cfg.FindModel(id)
			for _, info := range []driver.ModelInfo{
				{Cfg: m.ForCatalog(tc.catalog), Catalog: tc.catalog},
				{Cfg: m, Catalog: tc.catalog},
			} {
				got := listing(t, opencodeAI(path), []driver.ModelInfo{info})
				want := []map[string]json.RawMessage{real[id]}
				sameFields(t, path, got, want, "id", "name", "description", "family", "knowledge", "limit", "capabilities", "cost")
				var gotAPI, realAPI map[string]json.RawMessage
				_ = json.Unmarshal(got[0]["api"], &gotAPI)
				_ = json.Unmarshal(real[id]["api"], &realAPI)
				for proto := range realAPI {
					if _, ok := gotAPI[proto]; !ok || len(gotAPI) != 1 {
						t.Errorf("%s %s: api %s, real protocol %s", path, id, got[0]["api"], proto)
					}
				}
			}
		}
	}
}

// TestListingCatalogExtraNulls: the protocols of api are the non-null keys
// of the effective extra.api (a catalog takes the model's away with a
// null), else the first dialect's; a nested null of the catalog's extra
// removes the field the model's extra set and the derived one.
func TestListingCatalogExtraNulls(t *testing.T) {
	m := &config.ModelConfig{
		ID: "m-1", Protocols: []string{config.DialectAnthropic},
		Extra: map[string]any{
			"api":          map[string]any{"chat-completions": map[string]any{}},
			"capabilities": map[string]any{"cache": map[string]any{"implicit": true}},
		},
		Catalogs: map[string]config.CatalogEntry{
			"opencode": {},
			"opencode-go": {Extra: map[string]any{
				"api":          map[string]any{"chat-completions": nil, "messages": map[string]any{}},
				"capabilities": map[string]any{"cache": nil, "top_k": nil},
			}},
		},
	}
	for _, tc := range []struct{ catalog, path, proto, caps string }{
		{"opencode", "/inference/v1/models", "chat-completions", `"cache":{"implicit":true}`},
		{"opencode-go", "/inference/go/v1/models", "messages", ""},
	} {
		got := listing(t, opencodeAI(tc.path), []driver.ModelInfo{{Cfg: m.ForCatalog(tc.catalog), Catalog: tc.catalog}})
		var api map[string]json.RawMessage
		_ = json.Unmarshal(got[0]["api"], &api)
		if _, ok := api[tc.proto]; !ok || len(api) != 1 {
			t.Errorf("%s: api %s, want only %s", tc.catalog, got[0]["api"], tc.proto)
		}
		caps := string(got[0]["capabilities"])
		if tc.caps != "" && !strings.Contains(caps, tc.caps) || tc.caps == "" && (strings.Contains(caps, "cache") || strings.Contains(caps, "top_k")) {
			t.Errorf("%s: capabilities %s", tc.catalog, caps)
		}
	}
	// Only null keys: the protocol comes from protocols.
	m.Extra = map[string]any{"api": map[string]any{"chat-completions": nil}}
	if a, _ := apiOf(t, opencodeAI("/inference/v1/models"), driver.ModelInfo{Cfg: m, Catalog: "opencode"}); !strings.HasPrefix(a, `{"messages":{`) {
		t.Errorf("null-only extra.api: %s", a)
	}
}

// rawItems lists models on req and returns its items raw, each in the
// listed key order.
func rawItems(t *testing.T, req *core.Request, models []driver.ModelInfo) []json.RawMessage {
	t.Helper()
	w := core.NewMemWriter()
	newDriver(t).ListModels(w, req, models)
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body(), &list); err != nil {
		t.Fatalf("listing %s: %v", w.Body(), err)
	}
	return list.Data
}

// createdField is the created of an item, the one field the wwz lists
// from its own clock (release_date) and the real from its own.
var createdField = regexp.MustCompile(`"created":\d+,`)

// TestExampleListingKeyOrderMatchesReal: examples/opencode-completo lists
// the 79 pay-as-you-go items and the 28 Go ones byte for byte as the real
// (testdata/real/opencode/catalog/), the order of the keys at every
// depth included (capabilities.cache and its explicit too, task 438),
// created aside.
func TestExampleListingKeyOrderMatchesReal(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "..", "examples", "opencode-completo", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		fixture, catalog string
		n                int
	}{
		{"payg_models", "opencode", 79},
		{"go_models", "opencode-go", 28},
	} {
		t.Run(tc.catalog, func(t *testing.T) {
			path, real := catalogItems(t, tc.fixture)
			var raw catalogFixture
			b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "opencode", "catalog", tc.fixture+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &raw); err != nil {
				t.Fatal(err)
			}
			var list struct {
				Data []json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(raw.Response.Body), &list); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{}
			for i, item := range list.Data {
				var id string
				_ = json.Unmarshal(real[i]["id"], &id)
				want[id] = createdField.ReplaceAllString(compact(t, item), "")
			}
			var models []driver.ModelInfo
			for i := range cfg.Models {
				m := &cfg.Models[i]
				if m.ModelSupports("opencode") && m.InCatalog(tc.catalog) {
					models = append(models, driver.ModelInfo{Cfg: m.ForCatalog(tc.catalog), Catalog: tc.catalog})
				}
			}
			got := rawItems(t, opencodeAI(path), models)
			if len(got) != tc.n || len(want) != tc.n {
				t.Fatalf("%s: wwz lists %d items, real %d, want %d", path, len(got), len(want), tc.n)
			}
			for _, item := range got {
				var head struct{ ID string }
				_ = json.Unmarshal(item, &head)
				g := createdField.ReplaceAllString(compact(t, item), "")
				if w, ok := want[head.ID]; !ok {
					t.Errorf("%s: %s not in the real", path, head.ID)
				} else if g != w {
					t.Errorf("%s: %s differs\n wwz  %s\n real %s", path, head.ID, g, w)
				}
			}
		})
	}
}
