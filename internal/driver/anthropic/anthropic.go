// Package anthropic is the Anthropic (Claude API) driver.
package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
)

func init() {
	// require_version only accepts "true" (ADR 0068): the real API always
	// requires anthropic-version, so the option changes nothing here.
	driver.Register("anthropic", func(opts map[string]string) (driver.Driver, error) {
		h := anth.APIHooks()
		h.Shape = shapeStopDetails
		h.ClaudeCode = true
		d := &Driver{hooks: h, oauthKey: opts["oauth_key"], oauthPlan: opts["oauth_plan"], verify: opts["client_verification"] == verifyClaudeCode}
		if _, ok := plans[d.oauthPlan]; !ok {
			d.oauthPlan = planNames[0]
		}
		return d, nil
	})
}

// Driver implements driver.Driver for Anthropic.
type Driver struct {
	hooks anth.Hooks
	// oauthKey is drivers[].options.oauth_key: the label of the keys[]
	// entry the subscription (OAuth) stands for and whose value signs its
	// tokens; "" turns the subscription off (oauth_token.go).
	oauthKey string
	// oauthPlan is drivers[].options.oauth_plan, the plan the profile
	// routes report.
	oauthPlan string
	// verify is drivers[].options.client_verification: claude-code, the
	// strict client verification of the inference routes (verify.go).
	verify bool
}

func (d *Driver) Name() string     { return "anthropic" }
func (d *Driver) Protocol() string { return "anthropic" }

// Routes are the inference routes, the public /api/hello (routes.go)
// and, with oauth_key, the subscription's OAuth and account routes
// (oauth_routes.go).
func (d *Driver) Routes() []driver.Route {
	rs := append([]driver.Route{
		{Method: "POST", Path: "/v1/messages", Kind: driver.KindMessages},
		{Method: "POST", Path: "/v1/messages/count_tokens", Kind: driver.KindCountTokens},
		{Method: "GET", Path: "/v1/models", Kind: driver.KindModels},
		{Method: "GET", Path: "/v1/models/*", Kind: driver.KindModel},
	}, helloRoutes()...)
	if d.oauthKey != "" {
		rs = append(rs, oauthRoutes()...)
	}
	return rs
}

func (d *Driver) Sniff(h driver.Headers) bool {
	return h.Get("x-api-key") != "" || h.Get("anthropic-version") != ""
}

func (d *Driver) Authenticate(h driver.Headers) driver.Credential {
	c := driver.Credential{Version: h.Get("anthropic-version"), Workspace: h.Get("anthropic-workspace-id")}
	if k := h.Get("x-api-key"); k != "" {
		c.Key, c.Scheme = k, "x-api-key"
	} else if auth := h.Get("authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		c.Key, c.Scheme = strings.TrimSpace(auth[7:]), "bearer"
	} else {
		c.Scheme = "none"
	}
	if d.hooks.RequireVersion && c.Version == "" {
		c.Missing = append(c.Missing, "anthropic-version")
	}
	return c
}

func (d *Driver) ParseRequest(kind driver.Kind, body []byte, h driver.Headers) (*core.Request, *core.Error) {
	switch kind {
	case driver.KindMessages, driver.KindCountTokens:
		if kind == driver.KindCountTokens {
			// count_tokens has no max_tokens; inject a placeholder.
			var m map[string]any
			if json.Unmarshal(body, &m) == nil {
				if _, ok := m["max_tokens"]; !ok {
					m["max_tokens"] = 1
					body, _ = json.Marshal(m)
				}
			}
		}
		req, e := anth.ParseMessages(&d.hooks, body, h, d.Name())
		if e != nil {
			return nil, e
		}
		if kind == driver.KindCountTokens {
			req.Endpoint = core.EndpointCountTokens
		}
		return req, nil
	}
	return nil, &core.Error{Kind: core.ErrNotFound, Message: "Not Found"}
}

func (d *Driver) WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	if req.Endpoint == core.EndpointCountTokens {
		return anth.WriteCountTokens(&d.hooks, w, req, resp)
	}
	return anth.WriteMessage(d.subscriptionHooks(req, nil).ForRequest(req, false), w, req, resp, rl)
}

func (d *Driver) StreamWriter(w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) core.StreamWriter {
	return anth.NewRequestStream(d.subscriptionHooks(req, nil), w, req, rl)
}

func (d *Driver) WriteError(w core.ResponseWriter, req *core.Request, e *core.Error, rl *core.RateLimitState) {
	anth.WriteError(d.subscriptionHooks(req, e), w, req, e, rl)
}

func (d *Driver) LimitPolicy() driver.LimitPolicy {
	return driver.LimitPolicy{Entities: []string{"ip", "key", "project", "account"}, TPMReserve: "none", ExcludeCacheRead: true, ResetFormat: "rfc3339"}
}

// Extra serves /api/hello and the subscription's routes (oauth_routes.go);
// the driver has no other auxiliary route.
func (d *Driver) Extra(kind driver.Kind, w core.ResponseWriter, h driver.Headers, body []byte, ctx driver.ExtraContext) {
	if _, path := driver.RouteOf(h); kind == driver.KindResource && path == pathHello {
		writeHello(w)
		return
	}
	if kind == driver.KindUsage && d.oauthKey != "" {
		d.serveOAuth(w, h, body, ctx)
		return
	}
	anth.WriteError(&d.hooks, w, nil, &core.Error{Kind: core.ErrNotFound, Message: "Not Found"}, nil)
}
