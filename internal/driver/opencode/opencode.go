// Package opencode is the OpenCode Console / Go gateway driver: OpenAI and
// Anthropic surfaces under /inference and /inference/go, dollar quotas in
// three windows, the Go error envelope and the opencode.* usage fields.
package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

func init() {
	driver.Register("opencode", func(opts map[string]string) (driver.Driver, error) {
		if v, ok := opts["require_session"]; ok && v != "true" {
			return nil, fmt.Errorf("opencode: option require_session=%q is invalid (allowed: true)", v)
		}
		perDay, err := freePerDayOption(opts)
		if err != nil {
			return nil, err
		}
		d := &Driver{freePerDay: perDay, consoleKey: opts["console_key"]}
		field, err := oai.GatewayReasoningField(opts["reasoning_field"], "reasoning_content")
		if err != nil {
			return nil, fmt.Errorf("opencode: %w", err)
		}
		d.oai = oai.Hooks{
			Name: "opencode", IDPrefix: "gw", ReasoningField: field, FinishUsage: true,
			ErrorBody: d.errorBody, StreamErrorBody: d.streamError, SuccessHeaders: successHeaders,
			UsageExtras: chatUsageExtras, Shape: shapeChat,
			BodyFormat: jsonFormat, ChunkFormat: jsonFormat,
		}
		d.anth = anth.DefaultHooks()
		d.anth.Name = "opencode"
		d.anth.RequireVersion = false
		d.anth.ErrorBody = d.anthErrorBody
		d.anth.RateLimitHeaders = nil
		d.anth.SuccessHeaders = successHeaders
		d.anth.UsageExtras = messagesUsageExtras
		d.anth.Shape = shapeMessages
		d.anth.Signature = "" // thinking blocks without signature (observed)
		d.anth.ToolChoice = toolChoiceRules
		return d, nil
	})
}

// Driver implements driver.Driver for OpenCode.
type Driver struct {
	oai  oai.Hooks
	anth anth.Hooks
	// freePerDay is drivers[].options.free_requests_per_day.
	freePerDay int
	// consoleKey is drivers[].options.console_key: the label of the
	// keys[] entry the Console's login approves and whose value signs its
	// tokens; "" never approves (console.go).
	consoleKey string
}

func (d *Driver) Name() string     { return "opencode" }
func (d *Driver) Protocol() string { return "opencode" }

func (d *Driver) Routes() []driver.Route {
	var rs []driver.Route
	for _, base := range []string{"/inference", "/inference/go", "/zen", "/zen/go"} {
		rs = append(rs,
			driver.Route{Method: "POST", Path: base + "/openai/v1/chat/completions", Kind: driver.KindChat},
			driver.Route{Method: "POST", Path: base + "/v1/chat/completions", Kind: driver.KindChat},
			driver.Route{Method: "POST", Path: base + "/openai/v1/responses", Kind: driver.KindResponses},
			driver.Route{Method: "POST", Path: base + "/v1/responses", Kind: driver.KindResponses},
			driver.Route{Method: "POST", Path: base + "/anthropic/v1/messages", Kind: driver.KindMessages},
			driver.Route{Method: "POST", Path: base + "/v1/messages", Kind: driver.KindMessages},
			driver.Route{Method: "GET", Path: base + "/v1/models", Kind: driver.KindModels},
			driver.Route{Method: "GET", Path: base + "/v1/models/*", Kind: driver.KindModel},
			driver.Route{Method: "GET", Path: base + "/v1/usage", Kind: driver.KindUsage},
		)
	}
	// The Gemini routes exist only under /inference (task 241; /zen/go
	// answers an HTML 404); v1 is a 404 without body (conferred).
	for _, base := range []string{"/inference", "/inference/go"} {
		rs = append(rs,
			driver.Route{Method: "POST", Path: base + "/google/v1beta/models/*", Kind: driver.KindGenerate},
			driver.Route{Method: "POST", Path: base + "/google/v1/models/*", Kind: driver.KindGenerate},
		)
	}
	return append(rs, consoleRoutes()...)
}

func (d *Driver) Sniff(h driver.Headers) bool {
	auth := h.Get("authorization")
	return strings.HasPrefix(strings.TrimPrefix(auth, "Bearer "), "oc_sk_")
}

// Authenticate reads the bearer key; the Messages routes also take
// x-api-key (the official client sends it there; conferred,
// free/messages_x_api_key.json) and the Gemini routes x-goog-api-key
// (conferred, gen_goog), never ?key= (gen_querykey). public, in any of
// them, is the free mode without an account (ADR 0072, item 4): Anonymous,
// never looked up in keys[].
func (d *Driver) Authenticate(h driver.Headers) driver.Credential {
	auth := h.Get("authorization")
	path := h.Get("x-wwz-path")
	var c driver.Credential
	switch {
	case strings.HasPrefix(strings.ToLower(auth), "bearer "):
		c = driver.Credential{Key: strings.TrimSpace(auth[7:]), Scheme: "bearer"}
	case isMessagesPath(path) && h.Get("x-api-key") != "":
		c = driver.Credential{Key: h.Get("x-api-key"), Scheme: "x-api-key"}
	case strings.Contains(path, googleSegment) && h.Get("x-goog-api-key") != "":
		c = driver.Credential{Key: h.Get("x-goog-api-key"), Scheme: "x-goog-api-key"}
	default:
		return driver.Credential{Scheme: "none"}
	}
	c.Anonymous = c.Key == publicKey
	c.Workspace = h.Get("x-opencode-org-id")
	if h.Get("x-opencode-session") == "" {
		c.Missing = append(c.Missing, "x-opencode-session")
	}
	return c
}

// isMessagesPath reports whether path is a Messages route of a base
// (<base>/anthropic/v1/messages or <base>/v1/messages).
func isMessagesPath(path string) bool {
	return strings.HasSuffix(path, "/v1/messages")
}

func (d *Driver) ParseRequest(kind driver.Kind, body []byte, h driver.Headers) (*core.Request, *core.Error) {
	var req *core.Request
	var err *core.Error
	switch kind {
	case driver.KindChat:
		req, err = oai.ParseChat(body, h, d.Name())
		if req != nil {
			reasonByDefault(req, body, "reasoning_effort", "reasoning")
		}
	case driver.KindResponses:
		return d.parseResponses(body)
	case driver.KindGenerate:
		return d.parseGoogle(body, h)
	case driver.KindMessages:
		if model, missing := missingMaxTokens(body); missing {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "max_tokens is required", Extra: map[string]any{extraZenModel: model}}
		}
		req, err = anth.ParseMessages(&d.anth, body, h, d.Name())
		if req != nil {
			reasonByDefault(req, body, "thinking")
		}
	default:
		return nil, &core.Error{Kind: core.ErrNotFound, Message: "Not found"}
	}
	return req, err
}

// reasonByDefault: the gateway's upstreams think without being asked
// (deepseek-v4.1-flash on both routes, qwen3.8-flash on Messages;
// conferred on 2026-10-31), so a request that names none of the reasoning
// fields asks for the model's default reasoning. A model without
// capabilities.reasoning still answers without it (spec 007 R7).
func reasonByDefault(req *core.Request, body []byte, fields ...string) {
	if req.Reasoning.Requested {
		return
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return
	}
	for _, f := range fields {
		if _, ok := m[f]; ok {
			return
		}
	}
	req.Reasoning.Requested = true
}

// missingMaxTokens reports a Messages body without max_tokens, and its
// model: the gateway refuses it with 400 {"model": "<model>"} (conferred
// with deepseek-v4.1-flash and qwen3.8-flash). A body that is not a JSON
// object is left to the parser.
func missingMaxTokens(body []byte) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return "", false
	}
	if _, ok := m["max_tokens"]; ok {
		return "", false
	}
	var model string
	_ = json.Unmarshal(m["model"], &model)
	return model, true
}

func (d *Driver) WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	if isGoogle(req) {
		return writeGoogle(w, req, resp)
	}
	if req.Endpoint == core.EndpointMessages {
		return anth.WriteMessage(&d.anth, w, req, resp, nil)
	}
	if isResponses(req) {
		return writeResponses(w, req, resp)
	}
	return oai.WriteChat(&d.oai, w, req, resp, nil)
}

func (d *Driver) StreamWriter(w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) core.StreamWriter {
	if isGoogle(req) {
		return newGoogleStream(w, req)
	}
	if req.Endpoint == core.EndpointMessages {
		return anth.NewStream(&d.anth, w, req, nil)
	}
	if isResponses(req) {
		return newRespStream(d, w, req)
	}
	return oai.NewStream(&d.oai, w, req, nil)
}

func (d *Driver) WriteError(w core.ResponseWriter, req *core.Request, e *core.Error, rl *core.RateLimitState) {
	if _, path := driver.RouteOfRequest(req); isConsolePath(path) {
		writeConsoleJSON(w, 401, unauthorized) // the Console's routes refuse in their own shape
		return
	}
	if r, ok := e.Extra[extraRawReply].(rawReply); ok {
		writeRawReply(w, r) // the Responses and Gemini routes' checks
		return
	}
	if isGoogle(req) {
		d.writeGoogleError(w, req, e)
		return
	}
	if req != nil && req.Endpoint == core.EndpointMessages {
		anth.WriteError(&d.anth, w, req, e, nil)
		return
	}
	oai.WriteError(&d.oai, w, req, e, nil)
}

func (d *Driver) LimitPolicy() driver.LimitPolicy {
	return driver.LimitPolicy{Entities: []string{"ip", "key", "user", "project", "account"}, TPMReserve: "none", ResetFormat: "seconds"}
}

func (d *Driver) Extra(kind driver.Kind, w core.ResponseWriter, h driver.Headers, body []byte, ctx driver.ExtraContext) {
	if _, path := driver.RouteOf(h); kind == driver.KindUsage && isConsolePath(path) {
		d.serveConsole(w, h, body, ctx)
		return
	}
	if kind != driver.KindUsage {
		d.WriteError(w, nil, &core.Error{Kind: core.ErrNotFound, Message: "Not found"}, nil)
		return
	}
	d.writeUsageRoute(w, h, ctx, time.Now())
}
