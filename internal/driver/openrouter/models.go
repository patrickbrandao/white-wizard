package openrouter

import (
	"sort"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// The /api/v1/models form, recorded on 2026-10-31 (spec 006 16.7, task 174).

// modelKeys is the order of a model item.
var modelKeys = []string{
	"id", "canonical_slug", "alias_target", "hugging_face_id", "name", "created", "description", "context_length",
	"architecture", "pricing", "top_provider", "per_request_limits", "supported_parameters", "default_parameters",
	"supported_voices", "knowledge_cutoff", "expiration_date", "links", "endpoints", "benchmarks", "reasoning",
}

// pricingKeys is the order of pricing (audio_output's place was not
// conferred: after audio).
var pricingKeys = []string{
	"prompt", "completion", "image", "image_output", "audio", "audio_output", "input_audio_cache", "web_search",
	"internal_reasoning", "input_cache_read", "input_cache_write", "input_cache_write_1h", "overrides",
}

// nestedKeys are the real orders of the objects inside an item, by path
// ("[]" is an array element), applied to models[].extra values, whose YAML
// maps carry no order. Keys not listed follow in sorted order.
var nestedKeys = map[string][]string{
	"architecture":                   {"modality", "input_modalities", "output_modalities", "tokenizer", "instruct_type"},
	"top_provider":                   {"context_length", "max_completion_tokens", "is_moderated"},
	"alias_target":                   {"name", "slug"},
	"links":                          {"details"},
	"reasoning":                      {"mandatory", "default_enabled", "supported_efforts", "default_effort"},
	"pricing.overrides[]":            append([]string{"min_prompt_tokens"}, pricingKeys...),
	"benchmarks":                     {"design_arena", "artificial_analysis"},
	"benchmarks.design_arena[]":      {"arena", "category", "elo", "win_rate", "rank"},
	"benchmarks.artificial_analysis": {"intelligence_index", "coding_index", "agentic_index"},
	"default_parameters":             {"temperature", "top_p", "top_k", "frequency_penalty", "presence_penalty", "repetition_penalty"},
}

// ordered turns the maps inside v into Objs in the real order of path.
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
		return o.Reorder(nestedKeys[path]...)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = ordered(path+"[]", el)
		}
		return out
	}
	return v
}

// modelEndpoints are the inference routes of the driver that serve a model.
var modelEndpoints = []any{"/api/v1/chat/completions", "/api/v1/completions", "/api/v1/messages"}

// listCacheControl and oneCacheControl are the Cache-Control of the
// listing and of one model.
const (
	listCacheControl = "private, max-age=5, stale-while-revalidate=5"
	oneCacheControl  = "private, no-store"
)

// perToken renders a per-million price as OpenRouter's per-token decimal
// string ("0.1" → "0.0000001"), shifting the decimal point instead of
// dividing, so no binary rounding shows.
func perToken(perM float64) string {
	if perM == 0 {
		return "0"
	}
	ip, fp, _ := strings.Cut(strconv.FormatFloat(perM, 'f', -1, 64), ".")
	digits, pos := ip+fp, len(ip)-6
	for len(digits) > 1 && digits[0] == '0' {
		digits, pos = digits[1:], pos-1
	}
	var s string
	switch {
	case pos <= 0:
		s = "0." + strings.Repeat("0", -pos) + digits
	default:
		s = digits[:pos] + "." + digits[pos:]
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// canonicalSlug is the slug of the snapshot (models[].version, ADR 0049):
// a version without an author takes the id's ("openai/gpt-4.1-nano" with
// version "gpt-4.1-nano-2025-04-14" → "openai/gpt-4.1-nano-2025-04-14");
// without a version, the id's slug.
func canonicalSlug(id, version string) string {
	switch {
	case version == "":
		return Slug(id)
	case strings.Contains(version, "/"):
		return version
	}
	if author, _, ok := strings.Cut(id, "/"); ok {
		return author + "/" + version
	}
	return Slug(version)
}

// modelJSON is one item of /api/v1/models: the configuration fills what it
// knows, models[].extra sets any field (pricing is merged key by key), and
// the result follows the real order.
func modelJSON(m driver.ModelInfo) oai.Obj {
	c := m.Cfg
	tools := c.Capabilities.Tools == nil || *c.Capabilities.Tools
	reasoning := c.Capabilities.Reasoning != nil && *c.Capabilities.Reasoning
	params := []string{"frequency_penalty", "max_tokens", "presence_penalty", "response_format", "seed", "stop", "temperature", "top_p"}
	if tools {
		params = append(params, "tool_choice", "tools")
	}
	if reasoning {
		params = append(params, "include_reasoning", "reasoning")
	}
	sort.Strings(params)
	supported := make([]any, len(params))
	for i, p := range params {
		supported[i] = p
	}
	canonical := canonicalSlug(c.ID, c.Version)
	pricing := oai.O("prompt", perToken(c.Pricing.InputPerM), "completion", perToken(c.Pricing.OutputPerM))
	if c.Pricing.CacheReadPerM > 0 {
		pricing = append(pricing, oai.Field{Key: "input_cache_read", Val: perToken(c.Pricing.CacheReadPerM)})
	}
	if c.Pricing.CacheWritePerM > 0 {
		pricing = append(pricing, oai.Field{Key: "input_cache_write", Val: perToken(c.Pricing.CacheWritePerM)})
	}
	var expiration any
	if c.RetiresAt != "" {
		expiration = c.RetiresAt
	}
	out := oai.O(
		"id", Slug(c.ID),
		"canonical_slug", canonical,
		"hugging_face_id", "",
		"name", "White Wizard: "+c.DisplayName,
		"created", c.Created,
		"description", "Simulated model served by White Wizard (wwz).",
		"context_length", c.ContextWindow,
		"architecture", oai.O("modality", "text->text", "input_modalities", []any{"text"}, "output_modalities", []any{"text"}, "tokenizer", "Other", "instruct_type", nil),
		"pricing", pricing,
		"top_provider", oai.O("context_length", c.ContextWindow, "max_completion_tokens", c.MaxOutput, "is_moderated", false),
		"per_request_limits", nil,
		"supported_parameters", supported,
		"default_parameters", oai.Obj{},
		"supported_voices", nil,
		"knowledge_cutoff", nil,
		"expiration_date", expiration,
		"links", oai.O("details", "/api/v1/models/"+canonical+"/endpoints"),
		"endpoints", modelEndpoints,
	)
	if reasoning {
		out = append(out, oai.Field{Key: "reasoning", Val: oai.O("mandatory", false, "default_enabled", true, "supported_efforts", []any{"high", "medium", "low"}, "default_effort", "medium")})
	}
	keys := make([]string, 0, len(c.Extra))
	for k := range c.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := c.Extra[k]
		if p, ok := v.(map[string]any); k == "pricing" && ok {
			sub := make([]string, 0, len(p))
			for pk := range p {
				sub = append(sub, pk)
			}
			sort.Strings(sub)
			for _, pk := range sub {
				pricing.Set(pk, ordered("pricing."+pk, p[pk]))
			}
			out.Set("pricing", pricing.Reorder(pricingKeys...))
			continue
		}
		out.Set(k, ordered(k, v))
	}
	return out.Reorder(modelKeys...)
}

// ListModels writes {data, total_count, links{next: null}}.
func (d *Driver) ListModels(w core.ResponseWriter, _ *core.Request, models []driver.ModelInfo) {
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, modelJSON(m))
	}
	writeObj(w, 200, oai.O("data", data, "total_count", len(data), "links", oai.O("next", nil)), "Cache-Control", listCacheControl)
}

// writeObj sends o compact, with OpenRouter's headers and the extra
// header pairs.
func writeObj(w core.ResponseWriter, status int, o oai.Obj, hdr ...string) {
	b, _ := jsonFormat.Marshal(o)
	writeRaw(w, status, string(b), hdr...)
}

// jsonFormat is OpenRouter's byte layout: compact, < > & as they are.
var jsonFormat = oai.Format{RawHTML: true}
