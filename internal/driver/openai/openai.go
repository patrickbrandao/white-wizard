// Package openai is the OpenAI API driver.
package openai

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

func init() {
	driver.Register("openai", func(opts map[string]string) (driver.Driver, error) {
		d := &Driver{hooks: oai.DefaultHooks()}
		for k, v := range opts {
			// The real OpenAI never returns the reasoning text (ADR 0046):
			// hidden is the only accepted value.
			if k != "reasoning_field" || v != oai.ReasoningHidden {
				return nil, fmt.Errorf("openai: option %s=%q is not accepted (reasoning_field accepts only %q)", k, v, oai.ReasoningHidden)
			}
		}
		return d, nil
	})
}

// Driver implements driver.Driver for OpenAI.
type Driver struct {
	hooks oai.Hooks
}

func (d *Driver) Name() string     { return "openai" }
func (d *Driver) Protocol() string { return "openai" }

func (d *Driver) Routes() []driver.Route {
	return []driver.Route{
		// GET first: callers that map path to kind by the last route keep the POST one.
		{Method: "GET", Path: "/v1/chat/completions", Kind: driver.KindResource},
		{Method: "POST", Path: "/v1/chat/completions", Kind: driver.KindChat},
		{Method: "POST", Path: "/v1/completions", Kind: driver.KindCompletions},
		{Method: "POST", Path: "/v1/responses", Kind: driver.KindResponses},
		{Method: "GET", Path: "/v1/models", Kind: driver.KindModels},
		{Method: "GET", Path: "/v1/models/*", Kind: driver.KindModel},
	}
}

func (d *Driver) Sniff(h driver.Headers) bool {
	auth := h.Get("authorization")
	if auth == "" {
		return h.Get("x-api-key") == "" && h.Get("anthropic-version") == ""
	}
	key := strings.TrimPrefix(auth, "Bearer ")
	return !strings.HasPrefix(key, "sk-or-") && !strings.HasPrefix(key, "oc_sk_") && h.Get("anthropic-version") == ""
}

func (d *Driver) Authenticate(h driver.Headers) driver.Credential {
	auth := h.Get("authorization")
	if auth == "" {
		return driver.Credential{Scheme: "none"}
	}
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return driver.Credential{Scheme: "bearer"}
	}
	return driver.Credential{Key: strings.TrimSpace(auth[7:]), Scheme: "bearer", Workspace: h.Get("openai-project")}
}

func (d *Driver) ParseRequest(kind driver.Kind, body []byte, h driver.Headers) (*core.Request, *core.Error) {
	switch kind {
	case driver.KindChat:
		return oai.ParseChat(body, h, d.Name())
	case driver.KindCompletions:
		return oai.ParseCompletions(body, d.Name())
	case driver.KindResponses:
		return oai.ParseResponses(body, d.Name())
	}
	return nil, &core.Error{Kind: core.ErrNotFound, Message: "unsupported endpoint"}
}

func (d *Driver) WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	if req.Endpoint == core.EndpointResponses {
		return oai.WriteResponses(&d.hooks, w, req, resp, rl)
	}
	return oai.WriteChat(&d.hooks, w, req, resp, rl)
}

func (d *Driver) StreamWriter(w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) core.StreamWriter {
	if req.Endpoint == core.EndpointResponses {
		return oai.NewRespStream(&d.hooks, w, req, rl)
	}
	return oai.NewStream(&d.hooks, w, req, rl)
}

func (d *Driver) WriteError(w core.ResponseWriter, req *core.Request, e *core.Error, rl *core.RateLimitState) {
	oai.WriteError(&d.hooks, w, req, e, rl)
}

func (d *Driver) ListModels(w core.ResponseWriter, _ *core.Request, models []driver.ModelInfo) {
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, modelJSON(m))
	}
	d.writeModels(w, oai.O("object", "list", "data", data))
}

func (d *Driver) GetModel(w core.ResponseWriter, _ *core.Request, m *driver.ModelInfo, id string) {
	if m == nil {
		oai.WriteError(&d.hooks, w, &core.Request{Model: id}, &core.Error{Kind: core.ErrModelNotFound}, nil)
		return
	}
	d.writeModels(w, modelJSON(*m))
}

// modelJSON is one /v1/models item: id, object, created, owned_by,
// shutdown_date and models[].extra. shutdown_date is on every item, as on
// the real listing (spec 006 16.7, task 230): models[].retires_at
// (YYYY-MM-DD) when set, null otherwise.
func modelJSON(m driver.ModelInfo) oai.Obj {
	var shutdown any
	if m.Cfg.RetiresAt != "" {
		shutdown = m.Cfg.RetiresAt
	}
	out := oai.O("id", m.Cfg.ID, "object", "model", "created", m.Cfg.Created, "owned_by", m.Cfg.OwnedBy, "shutdown_date", shutdown)
	out.Merge(m.Cfg.Extra)
	return out
}

// modelsFormat is the layout of /v1/models: 2 spaces, no final newline.
var modelsFormat = oai.Format{Indent: "  ", RawHTML: true}

// writeModels sends a /v1/models body with the listing's headers:
// openai-processing-ms, openai-version and a UUID x-request-id, without
// x-ratelimit-* (spec 006 16.7).
func (d *Driver) writeModels(w core.ResponseWriter, v oai.Obj) {
	t0 := time.Now()
	b, err := modelsFormat.Marshal(v)
	if err != nil { // a models[].extra value JSON cannot encode
		oai.WriteError(&d.hooks, w, nil, &core.Error{Kind: core.ErrServerError, Message: err.Error()}, nil)
		return
	}
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	w.Header("openai-processing-ms", strconv.FormatInt(time.Since(t0).Milliseconds(), 10))
	w.Header("openai-version", oai.OpenAIVersion)
	w.Header("x-request-id", oai.NewUUID())
	_, _ = w.Write(b)
	_ = w.Flush()
}

func (d *Driver) LimitPolicy() driver.LimitPolicy {
	return driver.LimitPolicy{Entities: []string{"ip", "key", "project", "account"}, TPMReserve: "max_tokens", ResetFormat: "go_duration"}
}

// Extra serves GET /v1/chat/completions, the listing of the stored chat
// completions (task 315). The wwz stores none, so it is the real empty list
// (testdata/real/openai/routes/chat_get.json): compact JSON without a final
// newline, the account headers of the key, a req_ x-request-id and no
// x-ratelimit-*. Any other auxiliary route is a 404.
func (d *Driver) Extra(kind driver.Kind, w core.ResponseWriter, h driver.Headers, body []byte, ctx driver.ExtraContext) {
	if method, path := driver.RouteOf(h); kind == driver.KindResource && method == "GET" && path == "/v1/chat/completions" {
		t0 := time.Now()
		req := &core.Request{}
		if mh, ok := h.(driver.MapHeaders); ok {
			req.Headers = mh
		}
		org, project := oai.AccountHeaders(req)
		const b = `{"object":"list","data":[],"first_id":null,"last_id":null,"has_more":false}`
		w.Status(200)
		w.Header("Content-Type", "application/json")
		w.Header("Content-Length", strconv.Itoa(len(b)))
		w.Header("openai-organization", org)
		w.Header("openai-processing-ms", strconv.FormatInt(time.Since(t0).Milliseconds(), 10))
		w.Header("openai-project", project)
		w.Header("openai-version", oai.OpenAIVersion)
		w.Header("x-request-id", oai.NewRequestID())
		_, _ = w.Write([]byte(b))
		_ = w.Flush()
		return
	}
	oai.WriteError(&d.hooks, w, nil, &core.Error{Kind: core.ErrNotFound, Message: "Not found"}, nil)
}
