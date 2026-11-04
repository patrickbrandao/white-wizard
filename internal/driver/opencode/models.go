package opencode

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// The GET /v1/models form, recorded on 2026-10-31 (spec 006 16.7, task 175;
// testdata/real/opencode/shape/models.json).

// modelKeys is the order of a model item.
var modelKeys = []string{
	"id", "object", "created", "owned_by", "name", "canonical_id", "type", "description", "family", "open_weights",
	"knowledge", "release_date", "last_updated", "modalities", "capabilities", "limit", "cost", "api",
}

// nestedKeys are the real orders of the objects inside an item, by path
// ("[]" is an array element; "*" any key), applied to the generated
// objects and to models[].extra values, whose YAML maps carry no order.
// Keys not listed follow in sorted order.
var nestedKeys = map[string][]string{
	"modalities":                        {"input", "output"},
	"capabilities":                      {"tools", "reasoning", "cache", "structured_output", "temperature", "top_p", "top_k", "system_updates", "effort_updates"},
	"capabilities.tools":                {"supported"},
	"capabilities.reasoning":            {"supported", "toggle", "default", "effort", "budget"},
	"capabilities.reasoning.default":    {"type", "value"},
	"capabilities.cache":                {"implicit", "explicit"},
	"capabilities.cache.explicit":       {"ttl", "max_breakpoints", "targets", "min_tokens"},
	"limit":                             {"context", "input", "output"},
	"cost":                              {"text", "audio", "multipliers"},
	"cost.text":                         {"input", "output"},
	"cost.text.input":                   {"price", "cache", "overrides"},
	"cost.text.input.cache":             {"read", "write"},
	"cost.text.input.overrides[]":       {"when", "price", "cache"},
	"cost.text.input.overrides[].cache": {"read", "write"},
	"cost.text.output":                  {"price", "overrides"},
	"cost.text.output.overrides[]":      {"when", "price"},
	"cost.multipliers[]":                {"when", "factor"},
	"api.*":                             {"base_url", "sdk", "compatibility", "capabilities"},
	"api.*.sdk":                         {"aisdk", "opencodeai"},
	"api.*.capabilities.tools":          {"strict", "choice", "search", "freeform", "updates"},
	"cost.multipliers[].when":           {"utc_days", "any"},
	"cost.multipliers[].when.any[]":     {"all"},
	"cost.text.input.overrides[].when":  {"context"},
}

// orderOf is the key order of path, trying the "api.*" wildcard too.
func orderOf(path string) []string {
	if keys, ok := nestedKeys[path]; ok {
		return keys
	}
	if rest, ok := strings.CutPrefix(path, "api."); ok {
		_, sub, _ := strings.Cut(rest, ".")
		if sub == "" {
			return nestedKeys["api.*"]
		}
		return nestedKeys["api.*."+sub]
	}
	return nil
}

// ordered turns the maps inside v into Objs in the real order of path and
// reorders the Objs it finds.
func ordered(path string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := make(oai.Obj, 0, len(x))
		for _, k := range keys {
			o = append(o, oai.Field{Key: k, Val: ordered(path+"."+k, x[k])})
		}
		return o.Reorder(orderOf(path)...)
	case oai.Obj:
		o := make(oai.Obj, len(x))
		for i, f := range x {
			o[i] = oai.Field{Key: f.Key, Val: ordered(path+"."+f.Key, f.Val)}
		}
		return o.Reorder(orderOf(path)...)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = ordered(path+"[]", el)
		}
		return out
	}
	return v
}

// merge sets the keys of the extra value v over base: objects merge key by
// key (recursively), a null removes the key, and anything else replaces.
// A map over a non-object starts from an empty object, so its nulls go
// too.
func merge(base, v any) any {
	m, isMap := v.(map[string]any)
	if !isMap {
		return v
	}
	b, _ := base.(oai.Obj)
	out := append(oai.Obj(nil), b...)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if m[k] == nil {
			out.Delete(k)
			continue
		}
		cur, _ := out.Get(k)
		out.Set(k, merge(cur, m[k]))
	}
	return out
}

// The protocols of api (spec 006 16.7 "OpenCode Go"): the path under the
// catalog's /inference family and the sdk of each, as the real lists them
// (testdata/real/opencode/catalog/).
const (
	protoChat     = "chat-completions"
	protoResp     = "responses"
	protoMessages = "messages"
	protoGenerate = "generate-content"
)

var protocols = map[string]struct {
	path string
	sdk  oai.Obj
}{
	protoChat:     {"/openai/v1", oai.O("aisdk", "@ai-sdk/openai-compatible", "opencodeai", "@opencode/ai/providers/openai-compatible")},
	protoResp:     {"/openai/v1", oai.O("aisdk", "@ai-sdk/openai", "opencodeai", "@opencode/ai/providers/openai/responses")},
	protoMessages: {"/anthropic/v1", oai.O("aisdk", "@ai-sdk/anthropic", "opencodeai", "@opencode/ai/providers/anthropic")},
	protoGenerate: {"/google/v1beta", oai.O("aisdk", "@ai-sdk/google", "opencodeai", "@opencode/ai/providers/google")},
}

// nativeProtocol is the protocol of the model's first dialect: openai →
// chat-completions, anthropic → messages, gemini → generate-content;
// none or "*" → chat-completions (ADR 0072, item 16). responses is
// declared through extra.api.
func nativeProtocol(c *config.ModelConfig) string {
	if len(c.Protocols) > 0 {
		switch c.Protocols[0] {
		case config.DialectAnthropic:
			return protoMessages
		case config.DialectGemini:
			return protoGenerate
		}
	}
	return protoChat
}

// apiEntry is the derived {base_url, sdk} of protocol under base (the
// origin and the catalog's family); a protocol the real does not list
// gets nothing derived.
func apiEntry(protocol, base string) oai.Obj {
	p, ok := protocols[protocol]
	if !ok {
		return oai.Obj{}
	}
	return oai.O("base_url", base+p.path, "sdk", p.sdk)
}

// familyOf is the /inference family of the catalog the listing serves,
// also on /zen and /zen/go (conferred): the model's catalog, else the
// route's; without either, the Go one.
func familyOf(catalog string, req *core.Request) string {
	if catalog == "" && req != nil {
		catalog = catalogOfPath(req.Header("x-wwz-path"))
	}
	if catalog == catalogPAYG {
		return "/inference"
	}
	return "/inference/go"
}

// originOf is the origin of wwz the server computed (driver.Origin, ADR
// 0073), which the real's absolute base_url (https://opencode.ai) stands
// for: scheme, host, external prefix and the mount prefix of the driver.
// Outside the server it is "": base_url is relative.
func originOf(req *core.Request) string { return driver.Origin(req) }

// costJSON is cost in the real form, from the pricing of the catalog:
// text.input{price, cache{read, write?}, overrides?}, text.output{price,
// overrides?} and multipliers?. cache comes with a cache price and on a
// free model (the real lists cache.read 0 on every free one). An override
// goes to input.overrides when it declares the input or a cache price,
// and to output.overrides when it declares the output one; every when is
// echoed as declared.
func costJSON(p config.Pricing) oai.Obj {
	input := oai.O("price", p.InputPerM)
	if p.CacheReadPerM > 0 || p.CacheWritePerM > 0 || p.Free() {
		cache := oai.O("read", p.CacheReadPerM)
		if p.CacheWritePerM > 0 {
			cache = append(cache, oai.Field{Key: "write", Val: p.CacheWritePerM})
		}
		input = append(input, oai.Field{Key: "cache", Val: cache})
	}
	output := oai.O("price", p.OutputPerM)
	var inOver, outOver []any
	for _, o := range p.Overrides {
		when := o.When.Raw()
		if o.InputPerM != nil || o.CacheReadPerM != nil || o.CacheWritePerM != nil {
			e := oai.O("when", when)
			if o.InputPerM != nil {
				e = append(e, oai.Field{Key: "price", Val: *o.InputPerM})
			}
			if o.CacheReadPerM != nil || o.CacheWritePerM != nil {
				var c oai.Obj
				if o.CacheReadPerM != nil {
					c = append(c, oai.Field{Key: "read", Val: *o.CacheReadPerM})
				}
				if o.CacheWritePerM != nil {
					c = append(c, oai.Field{Key: "write", Val: *o.CacheWritePerM})
				}
				e = append(e, oai.Field{Key: "cache", Val: c})
			}
			inOver = append(inOver, e)
		}
		if o.OutputPerM != nil {
			outOver = append(outOver, oai.O("when", when, "price", *o.OutputPerM))
		}
	}
	if len(inOver) > 0 {
		input = append(input, oai.Field{Key: "overrides", Val: inOver})
	}
	if len(outOver) > 0 {
		output = append(output, oai.Field{Key: "overrides", Val: outOver})
	}
	cost := oai.O("text", oai.O("input", input, "output", output))
	if len(p.Multipliers) > 0 {
		ms := make([]any, len(p.Multipliers))
		for i, m := range p.Multipliers {
			ms[i] = oai.O("when", m.When.Raw(), "factor", m.Factor)
		}
		cost = append(cost, oai.Field{Key: "multipliers", Val: ms})
	}
	return cost
}

// declaredAPI are the protocols extra.api declares: its keys with a value
// (a null one is a protocol taken away, as a catalog's extra does over the
// model's; revision of task 434), sorted.
func declaredAPI(c *config.ModelConfig) []string {
	declared, _ := c.Extra["api"].(map[string]any)
	keys := make([]string, 0, len(declared))
	for k, v := range declared {
		if v != nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// apiJSON is api with exactly one item, the native protocol (ADR 0072,
// item 16): with extra.api, its non-null keys are the protocols and only
// base_url and sdk are derived (extra merges over them); without any, the
// first dialect's protocol with the default compatibility and
// capabilities.
func apiJSON(c *config.ModelConfig, reasoning, tools bool, base string) oai.Obj {
	if keys := declaredAPI(c); len(keys) > 0 {
		out := make(oai.Obj, 0, len(keys))
		for _, k := range keys {
			out = append(out, oai.Field{Key: k, Val: apiEntry(k, base)})
		}
		return out
	}
	proto := nativeProtocol(c)
	e := apiEntry(proto, base)
	if proto == protoChat && reasoning {
		e = append(e, oai.Field{Key: "compatibility", Val: oai.O("reasoning_field", "reasoning_content")})
	}
	choice := []any{"auto"}
	if tools {
		choice = []any{"auto", "none", "required", "tool"}
	}
	e = append(e, oai.Field{Key: "capabilities", Val: oai.O("tools", oai.O("choice", choice, "updates", false))})
	return oai.O(proto, e)
}

// dateOf formats a Unix time with layout (UTC); 0 is the Unix epoch.
func dateOf(unix int64, layout string) string { return time.Unix(unix, 0).UTC().Format(layout) }

// modelJSON is one item of /v1/models outside a request (relative
// base_url).
func modelJSON(m driver.ModelInfo, now time.Time) oai.Obj { return modelItem(m, now, nil, "") }

// modelItem is one item of /v1/models: the model effective in the
// listing's catalog (config.ModelConfig.ForCatalog: limit, capabilities,
// the price with multipliers and overrides, and extra merged with the
// catalog's) fills what it knows, its extra sets any field (objects merge
// key by key, a null removes the field, nested ones too), and the result
// follows the real order. created is the instant of the request, owned_by
// is always "opencode" (observed); origin is originOf the request.
func modelItem(m driver.ModelInfo, now time.Time, req *core.Request, origin string) oai.Obj {
	c := m.Cfg.ForCatalog(m.Catalog) // effective already from the engine: an equal model
	tools := c.Capabilities.Tools == nil || *c.Capabilities.Tools
	reasoning := c.Capabilities.Reasoning != nil && *c.Capabilities.Reasoning
	name := c.DisplayName
	if name == "" {
		name = c.ID
	}
	author := c.OwnedBy
	if author == "" || author == "opencode" {
		author, _, _ = strings.Cut(c.ID, "-")
	}
	family := c.ID
	if c.Version != "" {
		family = c.Version
	}
	reason := oai.O("supported", reasoning)
	if reasoning {
		reason = append(reason, oai.Field{Key: "effort", Val: []any{"low", "medium", "high"}})
	}
	caps := oai.O(
		"tools", oai.O("supported", tools),
		"reasoning", reason,
		"structured_output", c.Capabilities.JSONSchema != nil && *c.Capabilities.JSONSchema,
		"temperature", true, "top_p", true, "top_k", true, "system_updates", true, "effort_updates", false,
	)
	out := oai.O(
		"id", c.ID, "object", "model", "created", now.Unix(), "owned_by", "opencode", "name", name,
		"canonical_id", author+"/"+c.ID, "type", "chat",
		"description", name+" model served by White Wizard (wwz)",
		"family", family, "open_weights", false,
		"knowledge", dateOf(c.Created, "2006-01"), "release_date", dateOf(c.Created, "2006-01-02"), "last_updated", dateOf(c.Created, "2006-01-02"),
		"modalities", oai.O("input", []any{"text"}, "output", []any{"text"}),
		"capabilities", caps,
		"limit", oai.O("context", c.ContextWindow, "output", c.MaxOutput),
		"cost", costJSON(m.Pricing()),
		"api", apiJSON(c, reasoning, tools, origin+familyOf(m.Catalog, req)),
	)
	keys := make([]string, 0, len(c.Extra))
	for k := range c.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if c.Extra[k] == nil {
			out.Delete(k)
			continue
		}
		cur, _ := out.Get(k)
		out.Set(k, merge(cur, c.Extra[k]))
	}
	for i, f := range out {
		out[i].Val = ordered(f.Key, f.Val)
	}
	return out.Reorder(modelKeys...)
}

// ListModels writes {object: "list", data} (answered without a key on the
// real gateway): the models of the route's catalog, base_url absolute in
// the catalog's /inference family.
func (d *Driver) ListModels(w core.ResponseWriter, req *core.Request, models []driver.ModelInfo) {
	now := time.Now()
	origin := originOf(req)
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, modelItem(m, now, req, origin))
	}
	b, _ := jsonFormat.Marshal(oai.O("object", "list", "data", data))
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	modelsHeaders(w)
	_, _ = w.Write(b)
	_ = w.Flush()
}

// modelsHeaderMap are the headers of every /v1/models answer (200 and
// 401), the log id included.
func modelsHeaderMap() map[string]string {
	return map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Headers": "Content-Type, Authorization, X-Api-Key, X-Opencode-Org-Id",
		"Access-Control-Allow-Methods": "GET, OPTIONS",
		"Cache-Control":                "no-store",
		"x-opencode-log-id":            newLogID(time.Now()),
	}
}

func modelsHeaders(w core.ResponseWriter) {
	for k, v := range modelsHeaderMap() {
		w.Header(k, v)
	}
}

// GetModel answers GET /v1/models/{id} as the real does for any id, listed
// or not: 404 with an empty body (conferred).
func (d *Driver) GetModel(w core.ResponseWriter, _ *core.Request, _ *driver.ModelInfo, _ string) {
	w.Status(404)
	w.Header("Content-Length", "0")
	w.Header("x-opencode-log-id", newLogID(time.Now()))
	_ = w.Flush()
}
