// Package gemini is the Gemini API driver (Google AI for Developers; ADR
// 0043, spec 006 16.5): models/{model}:generateContent,
// :streamGenerateContent (SSE with alt=sse, a JSON array without it) and
// :countTokens, the paged models listing and cachedContents, under v1beta
// and v1. Every shape and text marked "observed" was recorded against the
// real API on 2026-10-31 (task 164, testdata/real/gemini/shape/).
package gemini

import (
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

func init() {
	driver.Register("gemini", func(map[string]string) (driver.Driver, error) { return &Driver{}, nil })
}

// Driver implements driver.Driver for the Gemini API.
type Driver struct{}

var (
	_ driver.Driver       = (*Driver)(nil)
	_ driver.Conventioner = (*Driver)(nil)
)

func (d *Driver) Name() string     { return "gemini" }
func (d *Driver) Protocol() string { return "gemini" }

// Routes: the same routes under v1beta (the SDKs' default) and v1, as the
// real serves them. GET and POST models/* carry the model (and the method)
// in the path. Anything else under /v1beta is the real's empty 404.
func (d *Driver) Routes() []driver.Route {
	var rs []driver.Route
	for _, v := range []string{"/v1beta", "/v1"} {
		rs = append(rs,
			driver.Route{Method: "POST", Path: v + "/models/*", Kind: driver.KindGenerate},
			driver.Route{Method: "GET", Path: v + "/models", Kind: driver.KindResource},
			driver.Route{Method: "GET", Path: v + "/models/*", Kind: driver.KindResource},
			driver.Route{Method: "POST", Path: v + "/cachedContents", Kind: driver.KindResource},
			driver.Route{Method: "GET", Path: v + "/cachedContents", Kind: driver.KindResource},
			driver.Route{Method: "GET", Path: v + "/cachedContents/*", Kind: driver.KindResource},
			driver.Route{Method: "PATCH", Path: v + "/cachedContents/*", Kind: driver.KindResource},
			driver.Route{Method: "DELETE", Path: v + "/cachedContents/*", Kind: driver.KindResource},
		)
	}
	// /v1 is shared with the other dialects at the root, so only /v1beta
	// gets the catch-all.
	for _, m := range []string{"GET", "POST", "PATCH", "DELETE", "PUT"} {
		rs = append(rs, driver.Route{Method: m, Path: "/v1beta/*", Kind: driver.KindResource})
	}
	return rs
}

// Credential schemes.
const (
	schemeAPIKey = "x-goog-api-key" //nolint:gosec // G101: a header name, not a credential
	schemeQuery  = "key"
	schemeBearer = "bearer"
	schemeNone   = "none"
)

// credentialOf reads the credential (spec 004): x-goog-api-key, else the
// key= query parameter, else an OAuth bearer, which never matches an API
// key (the real answers 401 to any bearer that is not an OAuth token).
func credentialOf(h driver.Headers) driver.Credential {
	if k := h.Get("x-goog-api-key"); k != "" {
		return driver.Credential{Key: k, Scheme: schemeAPIKey}
	}
	if k := driver.Query(h).Get("key"); k != "" {
		return driver.Credential{Key: k, Scheme: schemeQuery}
	}
	if auth := h.Get("authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return driver.Credential{Scheme: schemeBearer}
	}
	return driver.Credential{Scheme: schemeNone}
}

// Sniff: a Gemini credential (x-goog-api-key or ?key=) or the SDKs'
// x-goog-api-client.
func (d *Driver) Sniff(h driver.Headers) bool {
	return h.Get("x-goog-api-key") != "" || driver.Query(h).Get("key") != "" || h.Get("x-goog-api-client") != ""
}

func (d *Driver) Authenticate(h driver.Headers) driver.Credential { return credentialOf(h) }

// Conventions (observed): no request-id header; the key before the body's
// semantics and the model; modelVersion is the id.
func (d *Driver) Conventions() driver.Conventions { return driver.DefaultConventions("gemini") }

// LimitPolicy: the real limits by project, per model, in RPM, input TPM
// and RPD (spec 006 16.5); output tokens are not reserved; there are no
// rate-limit headers.
func (d *Driver) LimitPolicy() driver.LimitPolicy {
	return driver.LimitPolicy{Entities: []string{"ip", "key", "project", "account"}, TPMReserve: "none", ResetFormat: "seconds"}
}

// Extra serves the KindResource routes: the models listing and lookup and
// cachedContents.
func (d *Driver) Extra(kind driver.Kind, w core.ResponseWriter, h driver.Headers, body []byte, ctx driver.ExtraContext) {
	req := &core.Request{Driver: d.Name()}
	if mh, ok := h.(driver.MapHeaders); ok {
		req.Headers = mh
	}
	r := routeOfHeaders(h)
	method := h.Get(driver.MethodHeader)
	switch {
	case kind != driver.KindResource || method == "POST" && strings.HasPrefix(r.resource, "models/") || !served(r, method):
		d.WriteError(w, req, noRoute(), nil)
	case r.resource == "models":
		d.listModels(w, req, h, ctx.Models)
	case strings.HasPrefix(r.resource, "models/"):
		d.getModel(w, req, r, ctx.Models)
	default:
		d.cacheRequest(w, req, r, method, body, ctx)
	}
}
