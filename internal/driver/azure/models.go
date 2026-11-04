package azure

import (
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// modelJSON is one item of the models listing (/openai/models and
// /openai/v1/models): the Azure model object, with the capabilities, the
// lifecycle status and, when models[].retires_at is set, the inference
// deprecation as Unix seconds. Shape from the REST reference (Models -
// List); not conferred (ADR 0043).
func modelJSON(m driver.ModelInfo) oai.Obj {
	o := oai.O(
		"status", "succeeded",
		"capabilities", oai.O("fine_tune", false, "inference", true, "completion", false, "chat_completion", true, "embeddings", false),
		"lifecycle_status", "generally-available",
	)
	if m.Cfg.RetiresAt != "" {
		if t, err := time.Parse(time.DateOnly, m.Cfg.RetiresAt); err == nil {
			o = append(o, oai.Field{Key: "deprecation", Val: oai.O("inference", t.Unix())})
		}
	}
	o = append(o, oai.Field{Key: "id", Val: m.Cfg.ID}, oai.Field{Key: "created_at", Val: m.Cfg.Created}, oai.Field{Key: "object", Val: "model"})
	o.Merge(m.Cfg.Extra)
	return o
}

func (d *Driver) ListModels(w core.ResponseWriter, req *core.Request, models []driver.ModelInfo) {
	if !modelsVersionOK(req) {
		d.WriteError(w, req, &core.Error{Kind: core.ErrNotFound}, nil)
		return
	}
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, modelJSON(m))
	}
	d.writeModels(w, oai.O("data", data, "object", "list"))
}

// GetModel answers /openai/models/{id}; an unknown id is a 404 in Azure's
// envelope.
func (d *Driver) GetModel(w core.ResponseWriter, req *core.Request, m *driver.ModelInfo, id string) {
	if !modelsVersionOK(req) {
		d.WriteError(w, req, &core.Error{Kind: core.ErrNotFound}, nil)
		return
	}
	if m == nil {
		d.WriteError(w, &core.Request{Model: id}, &core.Error{Kind: core.ErrNotFound}, nil)
		return
	}
	d.writeModels(w, modelJSON(*m))
}

// modelsVersionOK reports whether the api-version of a models request is
// one its family accepts: on /openai/models (the listing the AzureOpenAI
// client calls) required and known, as on the deployment routes; on
// /openai/v1/models absent, v1 or preview. Without a path (contract tests)
// anything goes. The engine answers the key first (AuthFirst), then this
// 404 Resource not found (not conferred).
func modelsVersionOK(req *core.Request) bool {
	if req == nil {
		return true
	}
	h := driver.MapHeaders(req.Headers)
	path := h.Get("x-wwz-path")
	r := route{}
	if strings.HasPrefix(path, "/openai/models") {
		r.family = familyDeployment
	} else if strings.HasPrefix(path, v1Prefix) {
		r.family = familyV1
	}
	return apiVersionOK(r, h)
}

func (d *Driver) writeModels(w core.ResponseWriter, v oai.Obj) {
	b, err := bodyFormat.Marshal(v)
	if err != nil { // a models[].extra value JSON cannot encode
		d.WriteError(w, nil, &core.Error{Kind: core.ErrServerError, Message: err.Error()}, nil)
		return
	}
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	w.Header("apim-request-id", oai.NewUUID())
	_, _ = w.Write(b)
	_ = w.Flush()
}
