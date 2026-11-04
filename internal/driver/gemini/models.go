package gemini

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// Models (spec 004, spec 006 16.5), conferred on 2026-10-31 (task 164):
// GET models pages with pageSize (default 50, at most 1000) and pageToken,
// nextPageToken absent on the last page; GET models/{id} is the Model
// resource; an unknown id is 404 "Model is not found"; a path with a
// method (models/{id}:generateContent) on GET is the empty 404.

const (
	defaultPageSize = 50
	maxPageSize     = 1000
)

// defaultMethods are the supportedGenerationMethods of a current text
// model (observed on gemini-3.5-flash-lite).
var defaultMethods = []any{"generateContent", "countTokens", "createCachedContent", "batchGenerateContent"}

// modelKeys is the order of the Model resource (observed).
var modelKeys = []string{
	"name", "version", "displayName", "description", "inputTokenLimit", "outputTokenLimit",
	"supportedGenerationMethods", "temperature", "topP", "topK", "maxTemperature", "thinking",
}

// modelObj renders models[] as the Model resource; models[].extra sets or
// adds fields (supportedGenerationMethods, description, temperature...).
func modelObj(m *driver.ModelInfo) oai.Obj {
	c := m.Cfg
	version := c.Version
	if version == "" {
		version = "001"
	}
	display := c.DisplayName
	if display == "" {
		display = c.ID
	}
	in, out := c.ContextWindow, c.MaxOutput
	if in <= 0 {
		in = 1048576
	}
	if out <= 0 {
		out = 65536
	}
	o := oai.O("name", "models/"+c.ID, "version", version, "displayName", display, "description", display,
		"inputTokenLimit", in, "outputTokenLimit", out, "supportedGenerationMethods", defaultMethods,
		"temperature", 1, "topP", 0.95, "topK", 64, "maxTemperature", 2)
	if c.Capabilities.Reasoning == nil || *c.Capabilities.Reasoning {
		o = append(o, oai.Field{Key: "thinking", Val: true})
	}
	keys := make([]string, 0, len(c.Extra))
	for k := range c.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o.Set(k, c.Extra[k])
	}
	return o.Reorder(modelKeys...)
}

// pageToken is the token of the page after name: base64 of the proto field
// 1 holding "models/<id>" (observed: ChVtb2RlbHMvZ2VtaW5pLTIuNS1wcm8=).
func pageToken(name string) string {
	b := append([]byte{0x0a}, varint(len(name))...)
	return base64.StdEncoding.EncodeToString(append(b, name...))
}

// pageStart is the index of the first model of the page a pageToken
// opens; ok is false for a token that names no listed model.
func pageStart(models []driver.ModelInfo, tok string) (int, bool) {
	for i := range models {
		if pageToken("models/"+models[i].Cfg.ID) == tok {
			return i + 1, true
		}
	}
	return 0, false
}

// listModels answers GET models.
func (d *Driver) listModels(w core.ResponseWriter, req *core.Request, h driver.Headers, models []driver.ModelInfo) {
	q := driver.Query(h)
	size := defaultPageSize
	if s := q.Get("pageSize"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			size = min(n, maxPageSize)
		}
	}
	start := 0
	if tok := q.Get("pageToken"); tok != "" {
		var ok bool
		if start, ok = pageStart(models, tok); !ok {
			d.WriteError(w, req, invalid("Invalid page_token."), nil)
			return
		}
	}
	end := min(start+size, len(models))
	if start >= end {
		writeJSON(w, req, 200, oai.Obj{})
		return
	}
	items := make([]any, 0, end-start)
	for i := start; i < end; i++ {
		items = append(items, modelObj(&models[i]))
	}
	o := oai.O("models", items)
	if end < len(models) {
		o = append(o, oai.Field{Key: "nextPageToken", Val: pageToken("models/" + models[end-1].Cfg.ID)})
	}
	writeJSON(w, req, 200, o)
}

// getModel answers GET models/{id} on a served route (no method, no '/' in
// the id).
func (d *Driver) getModel(w core.ResponseWriter, req *core.Request, r route, models []driver.ModelInfo) {
	m := findModel(models, r.model)
	if m == nil {
		d.WriteError(w, req, &core.Error{Kind: core.ErrModelNotFound, Extra: map[string]any{extraMessage: fmt.Sprintf(msgModelGet, r.model, r.version)}}, nil)
		return
	}
	writeJSON(w, req, 200, modelObj(m))
}

// ListModels and GetModel serve the engine's KindModels and KindModel
// routes, which this driver does not declare: the listing needs the query
// (pageSize, pageToken) and the API version of the path, so it is a
// KindResource served by Extra.
func (d *Driver) ListModels(w core.ResponseWriter, _ *core.Request, models []driver.ModelInfo) {
	d.listModels(w, nil, driver.MapHeaders{}, models)
}

func (d *Driver) GetModel(w core.ResponseWriter, _ *core.Request, m *driver.ModelInfo, id string) {
	if m == nil {
		d.WriteError(w, nil, &core.Error{Kind: core.ErrModelNotFound, Extra: map[string]any{extraMessage: fmt.Sprintf(msgModelGet, id, "v1beta")}}, nil)
		return
	}
	writeJSON(w, nil, 200, modelObj(m))
}
