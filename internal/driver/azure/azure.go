// Package azure is the Azure OpenAI driver (ADR 0043, spec 006 16.6): the
// deployment routes /openai/deployments/{deployment}/chat/completions and
// /completions with a required api-version, the v1 API (/openai/v1/
// chat/completions and /responses, model = deployment), the models
// listing, the api-key header (or a bearer), Azure's error envelope with
// innererror and the content filter annotations (prompt_filter_results,
// content_filter_results). There is no real key: everything follows the
// official documentation and stays "not conferred" (ADR 0043, exception to
// ADR 0014).
//
// The deployment is models[].id or an alias (ADR 0046); model in the
// response is the id (or models[].version), never the deployment name.
package azure

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

func init() {
	driver.Register("azure", func(opts map[string]string) (driver.Driver, error) {
		for k, v := range opts {
			// Like OpenAI, Azure never returns the reasoning text (ADR 0046).
			if k != "reasoning_field" || v != oai.ReasoningHidden {
				return nil, fmt.Errorf("azure: option %s=%q is not accepted (reasoning_field accepts only %q)", k, v, oai.ReasoningHidden)
			}
		}
		return &Driver{hooks: hooks()}, nil
	})
}

// Driver implements driver.Driver for Azure OpenAI.
type Driver struct {
	hooks oai.Hooks
}

var (
	_ driver.Driver       = (*Driver)(nil)
	_ driver.Conventioner = (*Driver)(nil)
)

// Byte layouts: compact bodies and chunks (not conferred).
var (
	bodyFormat  = oai.Format{RawHTML: true}
	chunkFormat = oai.Format{RawHTML: true}
)

// fingerprint is system_fingerprint ("fp_" + 10 hex, as the doc examples).
const fingerprint = "fp_b705f0c291"

// hooks is the OpenAI dialect with Azure's errors, headers and content
// filter fields. No service_tier, annotations or obfuscation: the
// documented bodies have none (not conferred).
func hooks() oai.Hooks {
	return oai.Hooks{
		Name: "azure", IDPrefix: "chatcmpl", ReasoningField: oai.ReasoningHidden,
		ErrorBody: errorBody, RateLimitHeaders: oai.OpenAIRateLimitHeaders, SuccessHeaders: successHeaders,
		StreamErrorBody: streamError, SystemFingerprint: fingerprint, Shape: shape,
		BodyFormat: bodyFormat, ChunkFormat: chunkFormat,
	}
}

func (d *Driver) Name() string     { return "azure" }
func (d *Driver) Protocol() string { return "azure" }

// Routes: the deployment routes share one wildcard (the operation is
// after the deployment, ParseRequest tells chat from completions), the v1
// API and the two models listings. Embeddings are outside the MVP (ADR
// 0054) and answer as an unknown route.
func (d *Driver) Routes() []driver.Route {
	return []driver.Route{
		{Method: "POST", Path: deploymentsPrefix + "*", Kind: driver.KindChat},
		{Method: "POST", Path: v1Prefix + "chat/completions", Kind: driver.KindChat},
		{Method: "POST", Path: v1Prefix + "responses", Kind: driver.KindResponses},
		{Method: "GET", Path: "/openai/models", Kind: driver.KindModels},
		{Method: "GET", Path: "/openai/models/*", Kind: driver.KindModel},
		{Method: "GET", Path: v1Prefix + "models", Kind: driver.KindModels},
		{Method: "GET", Path: v1Prefix + "models/*", Kind: driver.KindModel},
	}
}

// Credential schemes.
const (
	schemeAPIKey = "api-key" //nolint:gosec // G101: a header name, not a credential
	schemeBearer = "bearer"
	schemeNone   = "none"
)

// Sniff: the api-key header is Azure's own (spec 005).
func (d *Driver) Sniff(h driver.Headers) bool { return h.Get("api-key") != "" }

// Authenticate reads api-key, else a bearer (an Entra ID token, or the key
// itself on the v1 API; spec 004 accepts the key in both).
func (d *Driver) Authenticate(h driver.Headers) driver.Credential {
	if k := h.Get("api-key"); k != "" {
		return driver.Credential{Key: k, Scheme: schemeAPIKey}
	}
	auth := h.Get("authorization")
	if auth == "" {
		return driver.Credential{Scheme: schemeNone}
	}
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return driver.Credential{Scheme: schemeBearer}
	}
	return driver.Credential{Key: strings.TrimSpace(auth[7:]), Scheme: schemeBearer}
}

// Conventions: the key before the body; model is models[].version or id;
// the request id is apim-request-id, which the driver writes itself (a
// UUID, not OpenAI's req_).
func (d *Driver) Conventions() driver.Conventions {
	return driver.Conventions{AuthFirst: true, ModelVersion: true}
}

// LimitPolicy: the real limits per deployment, TPM with RPM derived from
// it in short windows (DeploymentLimits), reserving max_tokens at
// admission (spec 006 16.6, ADR 0046). The tier's count limits go to the
// deployment ("<account>/<model id>"); the account keeps the dollar
// limits, blocks and bans; key and project keep their own ceilings.
// OpenAI's headers.
func (d *Driver) LimitPolicy() driver.LimitPolicy {
	return driver.LimitPolicy{Entities: []string{"ip", "key", "project", "account", "deployment"}, TPMReserve: "max_tokens", ResetFormat: "go_duration"}
}

func (d *Driver) ParseRequest(kind driver.Kind, body []byte, h driver.Headers) (*core.Request, *core.Error) {
	r := routeOfHeaders(h, kind)
	if r.endpoint == "" || !apiVersionOK(r, h) {
		return nil, &core.Error{Kind: core.ErrNotFound}
	}
	raw := body
	if r.family == familyDeployment {
		body = withModel(body, r.deployment)
	}
	if r.endpoint == core.EndpointResponses {
		return oai.ParseResponses(body, d.Name())
	}
	if err := validate(r.endpoint, body); err != nil {
		return nil, err
	}
	var (
		req *core.Request
		err *core.Error
	)
	if r.endpoint == core.EndpointCompletions {
		req, err = oai.ParseCompletions(body, d.Name())
	} else {
		req, err = oai.ParseChat(body, h, d.Name())
	}
	if err != nil {
		return nil, err
	}
	req.Raw = raw
	return req, nil
}

// withModel sets model to the deployment of the path: on the deployment
// routes the body's model is ignored (the SDK sends it, curl may not).
func withModel(body []byte, deployment string) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil || top == nil {
		return body
	}
	top["model"], _ = json.Marshal(deployment)
	b, _ := json.Marshal(top) // raw values that just decoded always encode
	return b
}

func (d *Driver) WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	if req.Endpoint == core.EndpointResponses {
		return oai.WriteResponses(&d.hooks, w, req, resp, rl)
	}
	fixID(req, resp)
	return oai.WriteChat(&d.hooks, w, req, resp, rl)
}

func (d *Driver) StreamWriter(w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) core.StreamWriter {
	if req.Endpoint == core.EndpointResponses {
		return oai.NewRespStream(&d.hooks, w, req, rl)
	}
	pw := &preWriter{ResponseWriter: w, pre: promptFilterFrame}
	return &stream{Stream: oai.NewStream(&d.hooks, pw, req, rl), pw: pw, req: req}
}

func (d *Driver) WriteError(w core.ResponseWriter, req *core.Request, e *core.Error, rl *core.RateLimitState) {
	oai.WriteError(&d.hooks, w, req, e, rl)
}

// successHeaders are the headers of every 200: apim-request-id and, when a
// limit applies, the six x-ratelimit-* (spec 006 16.6, spec 010).
func successHeaders(req *core.Request, _ *core.Response, rl *core.RateLimitState) map[string]string {
	m := map[string]string{}
	if rl != nil && rl.Present {
		m = oai.OpenAIRateLimitHeaders(rl)
	}
	m["apim-request-id"] = requestID(req)
	return m
}

// Extra: the driver has no auxiliary routes.
func (d *Driver) Extra(_ driver.Kind, w core.ResponseWriter, h driver.Headers, _ []byte, _ driver.ExtraContext) {
	req := &core.Request{Driver: d.Name()}
	if mh, ok := h.(driver.MapHeaders); ok {
		req.Headers = mh
	}
	d.WriteError(w, req, &core.Error{Kind: core.ErrNotFound}, nil)
}
