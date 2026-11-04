package anthropic

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// ListModels is GET /v1/models: {data, has_more, first_id, last_id}, in
// that order (spec 006 16.7).
func (d *Driver) ListModels(w core.ResponseWriter, req *core.Request, models []driver.ModelInfo) {
	d.listModels(w, req, models, time.Now())
}

func (d *Driver) listModels(w core.ResponseWriter, req *core.Request, models []driver.ModelInfo, now time.Time) {
	data := make([]any, 0, len(models))
	var first, last any
	for _, m := range models {
		data = append(data, modelJSON(m, now))
	}
	if len(models) > 0 {
		first, last = models[0].Cfg.ID, models[len(models)-1].Cfg.ID
	}
	d.writeModels(w, req, oai.O("data", data, "has_more", false, "first_id", first, "last_id", last))
}

// GetModel is GET /v1/models/{id}: the item, or the 404 "model: <id>"; an id
// with a "/" is an unknown route (task 316).
func (d *Driver) GetModel(w core.ResponseWriter, req *core.Request, m *driver.ModelInfo, id string) {
	if strings.Contains(id, "/") {
		writeSubpathMiss(w)
		return
	}
	if m == nil {
		er := &core.Request{Model: id}
		if req != nil {
			er.Headers, er.ClientIP = req.Headers, req.ClientIP
		}
		anth.WriteError(&d.hooks, w, er, &core.Error{Kind: core.ErrNotFound, Message: "model: " + id}, nil)
		return
	}
	d.writeModels(w, req, modelJSON(*m, time.Now()))
}

// KeylessModelMiss reports whether GET /v1/models/{param} without a key is
// answered as an unknown route before the key check: a sub-path of a model
// is 404 without a key (with or without anthropic-version) and 401 with an
// invalid one (conferred on 2026-10-31, task 342; the engine reads it).
func (d *Driver) KeylessModelMiss(param string) bool { return strings.Contains(param, "/") }

// writeModels sends a /models body with the listing's headers: request-id
// and the account of req's key (the ids its Messages answers carry, task
// 235), without anthropic-ratelimit-* (spec 006 16.7).
func (d *Driver) writeModels(w core.ResponseWriter, req *core.Request, v oai.Obj) {
	b, err := oai.Format{RawHTML: true}.Marshal(v)
	if err != nil { // a models[].extra value JSON cannot encode
		anth.WriteError(&d.hooks, w, req, &core.Error{Kind: core.ErrServerError, Message: err.Error()}, nil)
		return
	}
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	w.Header("request-id", anth.NewRequestID())
	for k, v := range anth.AnthropicAccountHeaders(req) {
		w.Header(k, v)
	}
	_, _ = w.Write(b)
	_ = w.Flush()
}

// modelJSON is one /v1/models item in the real order: type, id,
// display_name, created_at, line, max_input_tokens, max_tokens,
// capabilities, lifecycle, deprecated_at, retires_at. models[].extra
// replaces any of them in place (capabilities merged key by key) and its
// other keys follow, sorted.
func modelJSON(m driver.ModelInfo, now time.Time) oai.Obj {
	c := m.Cfg
	name := c.DisplayName
	if name == "" {
		name = c.ID
	}
	o := oai.O(
		"type", "model", "id", c.ID, "display_name", name,
		"created_at", time.Unix(c.Created, 0).UTC().Format(time.RFC3339),
		"line", line(c.ID), "max_input_tokens", c.ContextWindow, "max_tokens", c.MaxOutput,
		"capabilities", capabilities(c.Capabilities),
		"lifecycle", lifecycle(c, now), "deprecated_at", date(c.DeprecatedAt), "retires_at", date(c.RetiresAt),
	)
	keys := make([]string, 0, len(c.Extra))
	for k := range c.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := c.Extra[k]
		if base, ok := o.Get(k); ok {
			if bo, ok := base.(oai.Obj); ok {
				if mv, ok := v.(map[string]any); ok {
					v = overlay(bo, mv)
				}
			}
		}
		o.Set(k, v)
	}
	return o
}

// families are the model lines the listing reports.
var families = []string{"haiku", "sonnet", "opus", "fable"}

// line is the family in the id ("claude-haiku-4-5" → "haiku"), "" when
// none is named (models[].extra.line sets it).
func line(id string) string {
	for _, f := range strings.Split(strings.ToLower(id), "-") {
		for _, fam := range families {
			if f == fam {
				return fam
			}
		}
	}
	return ""
}

// lifecycle is "active" before deprecated_at and "deprecated" from it on
// (conferred on 2026-10-31: claude-sonnet-4-5-20250929 deprecated, still
// listed until retires_at; what follows retirement is not listed).
func lifecycle(c *config.ModelConfig, now time.Time) string {
	if c.DeprecatedAt != "" {
		if t, err := time.Parse(time.DateOnly, c.DeprecatedAt); err == nil && !now.Before(t) {
			return "deprecated"
		}
	}
	return "active"
}

// date is a lifecycle date as the listing shows it: null when unset, else
// RFC 3339 (observed "2026-09-30T18:30:00Z"); the configuration has only
// the day, so 00:00 UTC, and models[].extra sets the exact instant.
func date(d string) any {
	if d == "" {
		return nil
	}
	if t, err := time.Parse(time.DateOnly, d); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return d
}

func supported(v bool) oai.Obj { return oai.O("supported", v) }

func flag(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// capabilities derives the capabilities object from models[].capabilities:
// thinking from reasoning, image and PDF input from vision, structured
// outputs from json_schema; the rest as claude-haiku-4-5 lists them
// (observed on 2026-10-31). Unset flags count as supported.
func capabilities(c config.Capabilities) oai.Obj {
	reasoning := flag(c.Reasoning, true)
	vision := flag(c.Vision, true)
	return oai.O(
		"batch", supported(true),
		"citations", supported(true),
		"code_execution", supported(false),
		"context_management", oai.O("supported", true,
			"clear_tool_uses_20250919", supported(true),
			"clear_thinking_20251015", supported(true),
			"compact_20260112", supported(false)),
		"effort", oai.O("supported", false,
			"low", supported(false), "medium", supported(false), "high", supported(false),
			"xhigh", supported(false), "max", supported(false)),
		"image_input", supported(vision),
		"pdf_input", supported(vision),
		"server_tools", oai.O("supported", flag(c.Tools, true),
			"code_execution", supported(true), "web_search", supported(true)),
		"structured_outputs", supported(flag(c.JSONSchema, true)),
		"thinking", oai.O("supported", reasoning,
			"types", oai.O("enabled", supported(reasoning), "adaptive", supported(false),
				"disabled", supported(true), "between_tools", supported(false))),
	)
}

// overlay sets the values of m over base, recursively, keeping base's
// order; keys base does not have follow, sorted.
func overlay(base oai.Obj, m map[string]any) oai.Obj {
	out := append(oai.Obj(nil), base...)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		if b, ok := out.Get(k); ok {
			if bo, ok := b.(oai.Obj); ok {
				if mv, ok := v.(map[string]any); ok {
					v = overlay(bo, mv)
				}
			}
		} else if mv, ok := v.(map[string]any); ok {
			v = overlay(nil, mv)
		}
		out.Set(k, v)
	}
	return out
}
