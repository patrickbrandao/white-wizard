package opencode

import (
	_ "embed"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// site404 is the web site's 404 page, as the real sent it (task 317,
// testdata/real/opencode/routes/miss/); {{path}} is the request path, which
// the page echoes in data-start-url.
//
//go:embed site404.html
var site404 string

// WriteRouteMiss answers a route the gateway does not serve (unknown path
// or wrong method) as the real does, before it looks at the key (conferred
// on 2026-10-31 with and without a key): under /inference and under the
// /zen/v1 and /zen/go/v1 APIs a 404 without a body, with x-opencode-log-id
// under /inference (task 297); everywhere else (/zen/nope, /zen/openai/...,
// /zen/go/..., paths outside the bases) the site's HTML 404 page, without
// x-opencode-log-id (task 371).
func (d *Driver) WriteRouteMiss(w core.ResponseWriter, _ *core.Request, miss driver.RouteMiss, _ *core.Error) {
	inference := miss.Path == "/inference" || strings.HasPrefix(miss.Path, "/inference/")
	api := strings.HasPrefix(miss.Path, "/zen/v1/") || strings.HasPrefix(miss.Path, "/zen/go/v1/")
	w.Status(404)
	if inference || api {
		w.Header("Content-Length", "0")
		if inference {
			w.Header("x-opencode-log-id", newLogID(time.Now()))
		}
		_ = w.Flush()
		return
	}
	page := strings.Replace(site404, "{{path}}", html.EscapeString(miss.Path), 1)
	w.Header("Content-Type", "text/html")
	w.Header("Content-Length", strconv.Itoa(len(page)))
	_, _ = w.Write([]byte(page))
	_ = w.Flush()
}
