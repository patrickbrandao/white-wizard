package openrouter

import (
	_ "embed"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// The real answers to a route miss (conferred on 2026-10-31, task 317,
// testdata/real/openrouter/routes/miss/). The key is never looked at:
// with, without or with an invalid key the answer is the same.
const (
	notFoundBody = `{"error":{"message":"Not Found","code":404}}`
	// missCacheControl goes on the unknown paths under /api, on a GET to
	// a route served with another method and on the 404 of
	// /api/v1/models/{...}; not on a PUT, DELETE or POST to such a route.
	missCacheControl = "private, no-store"
	// The CORS lists of an unknown path under /api (the site's API
	// catch-all, not the worker, so no expose-headers).
	missAllowMethods = "GET,OPTIONS,PATCH,DELETE,POST,PUT"
	missAllowHeaders = "Authorization, User-Agent, X-Api-Key, X-CSRF-Token, X-Requested-With, Accept, Accept-Version, " +
		"Content-Length, Content-MD5, Content-Type, Date, X-Api-Version, HTTP-Referer, X-Windowai-Title, X-Openrouter-Title, " +
		"X-Title, X-Openrouter-Categories, X-Openrouter-App-Visibility, X-Stainless-Lang, X-Stainless-Package-Version, " +
		"X-Stainless-OS, X-Stainless-Arch, X-Stainless-Runtime, X-Stainless-Runtime-Version, X-Stainless-Retry-Count, " +
		"X-Stainless-Timeout, X-Stainless-Helper-Method, Protection-Key, traceparent, tracestate, b3"
	siteCacheControl = "public, max-age=0, must-revalidate"
)

// site404 is the web site's 404 page, as the real sent it to
// GET /v1/chat/completions (fixture outside_api; the bytes change with each
// deploy of the site, the fixture keeps "<html>").
//
//go:embed site404.html
var site404 string

// WriteRouteMiss answers a route no OpenRouter route serves as the real
// does, before it looks at the key: outside /api the site's HTML 404; an
// unknown path under /api the API catch-all's 404 (allow-methods and
// allow-headers, no expose-headers); a wrong method on a known route the
// worker's 404, never a 405 (expose-headers; cache-control only on a GET).
func (d *Driver) WriteRouteMiss(w core.ResponseWriter, _ *core.Request, miss driver.RouteMiss, _ *core.Error) {
	switch {
	case miss.Path != "/api" && !strings.HasPrefix(miss.Path, "/api/"):
		w.Status(404)
		w.Header("Content-Type", "text/html; charset=utf-8")
		w.Header("Content-Length", strconv.Itoa(len(site404)))
		w.Header("Access-Control-Allow-Origin", allowOrigin)
		w.Header("Cache-Control", siteCacheControl)
		w.Header("Content-Disposition", `inline; filename="404"`)
		_, _ = w.Write([]byte(site404))
		_ = w.Flush()
	case len(miss.Allow) == 0:
		w.Status(404)
		w.Header("Content-Type", "application/json")
		w.Header("Content-Length", strconv.Itoa(len(notFoundBody)))
		w.Header("Access-Control-Allow-Headers", missAllowHeaders)
		w.Header("Access-Control-Allow-Methods", missAllowMethods)
		w.Header("Access-Control-Allow-Origin", allowOrigin)
		w.Header("Cache-Control", missCacheControl)
		_, _ = w.Write([]byte(notFoundBody))
		_ = w.Flush()
	case miss.Method == "GET":
		writeRaw(w, 404, notFoundBody, "Cache-Control", missCacheControl)
	default:
		writeRaw(w, 404, notFoundBody)
	}
}
