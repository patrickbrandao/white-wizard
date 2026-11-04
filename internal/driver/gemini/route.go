package gemini

import (
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Methods of models/{model}:{method}.
const (
	methodGenerate = "generateContent"
	methodStream   = "streamGenerateContent"
	methodCount    = "countTokens"
)

// endpointGenerate is generateContent and streamGenerateContent in the
// engine's metrics and scenarios; countTokens is core.EndpointCountTokens.
const endpointGenerate core.Endpoint = "generate_content"

// route is what the path of a request says: the API version, the resource
// after it and, on models/{model}:{method}, the model and the method.
type route struct {
	version  string // v1beta | v1
	resource string // models, models/{id}, cachedContents/{id}...
	model    string
	method   string
}

// routeOf parses the path relative to the mount prefix (x-wwz-path).
func routeOf(path string) route {
	path = strings.TrimPrefix(path, "/")
	version, rest, _ := strings.Cut(path, "/")
	r := route{version: version, resource: rest}
	if id, ok := strings.CutPrefix(rest, "models/"); ok {
		if i := strings.LastIndexByte(id, ':'); i >= 0 {
			r.model, r.method = id[:i], id[i+1:]
		} else {
			r.model = id
		}
	}
	if r.version == "" {
		r.version = "v1beta"
	}
	return r
}

// routeOfHeaders is the route of a request through its headers.
func routeOfHeaders(h driver.Headers) route {
	if h == nil {
		return route{version: "v1beta"}
	}
	return routeOf(h.Get("x-wwz-path"))
}

// routeOfReq is the route of a request; nil is a bare v1beta route.
func routeOfReq(req *core.Request) route {
	if req == nil {
		return route{version: "v1beta"}
	}
	return routeOf(req.Header("x-wwz-path"))
}

// sse reports whether a stream answers in SSE (alt=sse) rather than as a
// JSON array.
func sse(h driver.Headers) bool {
	return driver.Query(h).Get("alt") == "sse"
}

// reqHeaders adapts a request's header map to driver.Headers.
func reqHeaders(req *core.Request) driver.Headers {
	if req == nil {
		return driver.MapHeaders{}
	}
	return driver.MapHeaders(req.Headers)
}

// served reports whether Google serves method on the route at all.
// Anything else is the empty 404 before the credential is looked at: a
// bad key, no key or a bearer get the same 404 (observed, task 265).
func served(r route, method string) bool {
	switch {
	case r.version != "v1beta" && r.version != "v1":
		return false
	case r.resource == "models":
		return method == "GET"
	case strings.HasPrefix(r.resource, "models/"):
		if method == "GET" {
			return r.method == "" && !strings.ContainsRune(r.model, '/')
		}
		return method == "POST" && r.model != "" && (r.method == methodGenerate || r.method == methodStream || r.method == methodCount)
	case r.resource == "cachedContents":
		return method == "GET" || method == "POST"
	case strings.HasPrefix(r.resource, "cachedContents/"):
		return strings.Count(r.resource, "/") == 1 && (method == "GET" || method == "PATCH" || method == "DELETE")
	}
	return false
}

// rpcMethod is the RPC name Google reports for a route in the 401's
// ErrorInfo (google.ai.generativelanguage.v1beta.GenerativeService.GenerateContent).
func rpcMethod(r route, httpMethod string) string {
	svc, name := "GenerativeService", "GenerateContent"
	switch {
	case r.method == methodStream:
		name = "StreamGenerateContent"
	case r.method == methodCount:
		name = "CountTokens"
	case r.resource == "models":
		svc, name = "ModelService", "ListModels"
	case r.model != "" && r.method == "":
		svc, name = "ModelService", "GetModel"
	case strings.HasPrefix(r.resource, "cachedContents"):
		svc = "CacheService"
		switch {
		case r.resource == "cachedContents" && httpMethod == "POST":
			name = "CreateCachedContent"
		case r.resource == "cachedContents":
			name = "ListCachedContents"
		case httpMethod == "DELETE":
			name = "DeleteCachedContent"
		case httpMethod == "PATCH":
			name = "UpdateCachedContent"
		default:
			name = "GetCachedContent"
		}
	}
	return "google.ai.generativelanguage." + r.version + "." + svc + "." + name
}

// WriteRouteMiss answers a path no Gemini route serves (GET /nope,
// /v2/models, /v1/nope; with or without a key) as the real does: the
// front-end's empty 404 of /v1beta/nope, before the key is looked at
// (conferred on 2026-10-31, task 317, testdata/real/gemini/routes/miss).
func (d *Driver) WriteRouteMiss(w core.ResponseWriter, req *core.Request, _ driver.RouteMiss, _ *core.Error) {
	d.WriteError(w, req, noRoute(), nil)
}
