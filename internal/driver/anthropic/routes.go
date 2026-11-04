package anthropic

import (
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// The bodies of the real route misses (conferred on 2026-10-31, task 235,
// testdata/real/anthropic/routes/).
const (
	notFoundBody = `{"type":"error","error":{"type":"not_found_error","message":"Not found"},"request_id":null}`
	// edgeMethodBody is the edge's 405 to GET /v1/messages: indented, no
	// request_id.
	edgeMethodBody = "{\n  \"type\": \"error\",\n  \"error\": {\n    \"type\": \"invalid_request_error\",\n    \"message\": \"Method Not Allowed\"\n  }\n}"
	methodPrefix   = `{"type":"error","error":{"type":"invalid_request_error","message":"Method Not Allowed"},"request_id":"`
	// helloBody is the answer of /api/hello, with the space after the
	// colon (20 bytes; fixtures routes/api_hello_*).
	helloBody = `{"message": "hello"}`
	pathHello = "/api/hello"
)

// helloMethods are the methods the real answers on /api/hello (curl on
// 2026-11-03): all of them get the 200.
var helloMethods = []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"}

// helloRoutes declare /api/hello, the probe Claude Code sends as HEAD
// before a session (ADR 0074): a public driver.KindResource route on every
// method, served by Extra (task 449).
func helloRoutes() []driver.Route {
	rs := make([]driver.Route, len(helloMethods))
	for i, m := range helloMethods {
		rs[i] = driver.Route{Method: m, Path: pathHello, Kind: driver.KindResource}
	}
	return rs
}

// isHello reports whether route is one of helloRoutes.
func isHello(route driver.Route) bool {
	return route.Kind == driver.KindResource && route.Path == pathHello
}

// writeHello answers /api/hello as the real does to every method, with or
// without a key: 200, application/json, no request-id; HEAD gets the
// Content-Length without the body (the data plane drops it).
func writeHello(w core.ResponseWriter) {
	w.Status(200)
	w.Header("Content-Type", "application/json")
	write(w, helloBody)
	_ = w.Flush()
}

// WriteRouteMiss answers a route no Anthropic route serves, in the real
// order: outside /v1 a 404
// without a body (any key); GET on the Messages
// route the edge's indented 405 (any key); then a presented invalid key
// is the 401; then an unknown path is a 404 with request_id null and no
// request-id header, a wrong method a 405 with request-id and allow (GET
// routes allow "GET,HEAD"). A missing key does not stop the 404 and 405.
func (d *Driver) WriteRouteMiss(w core.ResponseWriter, req *core.Request, miss driver.RouteMiss, authErr *core.Error) {
	switch {
	case miss.Path == pathOAuthHello:
		writeOAuthHello(w, miss.Method)
		return
	case miss.Path == pathFirstTokenDay && miss.Method == "GET":
		// Asked after a claude.ai login; answered with or without oauth_key.
		writeJSON(w, 200, oai.O("first_token_date", nil))
		return
	case bearerPaths[miss.Path] && authErr != nil && req != nil && oauthLike(bearerOf(req.Header)):
		// An account route without oauth_key: every token is invalid
		// (fixture oauth_profile_badkey).
		anth.WriteError(&d.hooks, w, req, authErr, nil)
		return
	case !strings.HasPrefix(miss.Path, "/v1/"):
		w.Status(404)
		w.Header("Content-Length", "0")
	case miss.Kind == driver.KindMessages && miss.Method == "GET" && len(miss.Allow) > 0:
		w.Status(405)
		w.Header("Content-Type", "application/json")
		w.Header("Cache-Control", "private, max-age=0, no-store, no-cache, must-revalidate, post-check=0, pre-check=0")
		w.Header("Expires", "Thu, 01 Jan 1970 00:00:01 GMT")
		w.Header("Referrer-Policy", "same-origin")
		w.Header("X-Frame-Options", "SAMEORIGIN")
		write(w, edgeMethodBody)
	case authErr != nil && driver.AccountOf(req).Key != "":
		anth.WriteError(&d.hooks, w, req, authErr, nil)
		return
	case len(miss.Allow) == 0:
		w.Status(404)
		w.Header("Content-Type", "application/json")
		w.Header("x-should-retry", "false")
		write(w, notFoundBody)
	default:
		id := anth.NewRequestID()
		w.Status(405)
		w.Header("Content-Type", "application/json")
		w.Header("request-id", id)
		w.Header("x-should-retry", "false")
		w.Header("Allow", allowHeader(miss.Allow))
		write(w, methodPrefix+id+`"}`)
	}
	_ = w.Flush()
}

// writeOAuthHello answers /v1/oauth/hello as platform.claude.com does
// (fixtures routes/oauth_hello_*): GET 200 {"message":"hello"} with a
// request-id; any other method the 405 with allow GET (HEAD included).
func writeOAuthHello(w core.ResponseWriter, method string) {
	id := anth.NewRequestID()
	if method == "GET" {
		w.Status(200)
		w.Header("Content-Type", "application/json")
		w.Header("request-id", id)
		write(w, oauthHelloBody)
		_ = w.Flush()
		return
	}
	w.Status(405)
	w.Header("Content-Type", "application/json")
	w.Header("x-should-retry", "false")
	w.Header("Allow", "GET")
	w.Header("request-id", id)
	write(w, methodPrefix+id+`"}`)
	_ = w.Flush()
}

// writeSubpathMiss is the real answer to /v1/models/{id}/...: an unknown
// route whose 404 carries a request_id and the request-id header (unlike
// other unknown paths; fixture model_subpath), without account headers.
func writeSubpathMiss(w core.ResponseWriter) {
	id := anth.NewRequestID()
	w.Status(404)
	w.Header("Content-Type", "application/json")
	w.Header("request-id", id)
	w.Header("x-should-retry", "false")
	write(w, `{"type":"error","error":{"type":"not_found_error","message":"Not found"},"request_id":"`+id+`"}`)
	_ = w.Flush()
}

func write(w core.ResponseWriter, body string) {
	w.Header("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write([]byte(body))
}

// allowHeader lists the methods as the real does: HEAD after GET.
func allowHeader(methods []string) string {
	out := make([]string, 0, len(methods)+1)
	for _, m := range methods {
		out = append(out, m)
		if m == "GET" {
			out = append(out, "HEAD")
		}
	}
	return strings.Join(out, ",")
}
