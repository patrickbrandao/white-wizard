// Package web is the HTML side of wwz-panel (spec 016, "Telas da fase 3";
// ADR 0023): server-side pages rendered with html/template, live regions
// refreshed by the vendored HTMX (static/htmx.min.js, no npm, no CDN) and a
// small own stylesheet, all embedded in the binary.
//
// The pages call the service layer of internal/panel/api (Overview, Usage,
// Entities, Act, Audit, ...), never the JSON API over HTTP. Authentication,
// roles and CSRF come from internal/panel/auth (RequirePageRole). Login and
// logout forms post to the API routes /api/v1/login and /api/v1/logout,
// which answer form posts with redirects.
//
// Adding a screen (task 053): write templates/<name>.html defining
// "content" (whole main area) and "live" (the region refreshed every 2 s
// and swapped by HTMX actions), add the name to pageNames, write a handler
// that builds a view and calls w.render, and register it in Register.
package web

import (
	"cmp"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/api"
	"github.com/patrickbrandao/white-wizard/internal/panel/auth"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// LoginPath is the login page (auth.LoginPage).
const LoginPath = auth.LoginPage

// CSP forbids inline scripts and styles and any external origin; HTMX is
// configured by attributes and the htmx-config meta tag.
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'"

// Options configures New.
type Options struct {
	API  *api.API      // required
	Auth *auth.Service // required
	// OIDC shows the OIDC button on the login page.
	OIDC   bool
	Logger *slog.Logger // nil = slog.Default()
}

// Web serves the panel pages.
type Web struct {
	api  *api.API
	auth *auth.Service
	oidc bool
	log  *slog.Logger
}

// New builds the pages.
func New(o Options) *Web {
	return &Web{api: o.API, auth: o.Auth, oidc: o.OIDC, log: cmp.Or(o.Logger, slog.Default())}
}

// Register mounts the pages and /static/ on mux. It takes "GET /{$}",
// "/login", "/usage", "/blocks", "/audit", "/limits", "/errors", "/scenarios",
// "/config" (with their POST actions) and "/static/"; the API keeps
// /api/v1, /auth/oidc, /health and /metrics.
func (w *Web) Register(mux *http.ServeMux) {
	viewer := w.auth.RequirePageRole(store.RoleViewer, LoginPath)
	operator := w.auth.RequirePageRole(store.RoleOperator, LoginPath)
	mux.Handle("GET /static/{name}", http.HandlerFunc(serveStatic))
	mux.Handle("GET "+LoginPath, w.auth.Optional(http.HandlerFunc(w.login)))
	mux.Handle("GET /{$}", viewer(http.HandlerFunc(w.overview)))
	mux.Handle("GET /usage", viewer(http.HandlerFunc(w.usage)))
	mux.Handle("GET /usage.csv", viewer(http.HandlerFunc(w.usageCSV)))
	mux.Handle("GET /blocks", viewer(http.HandlerFunc(w.blocks)))
	mux.Handle("POST /blocks/act", operator(http.HandlerFunc(w.blocksAct)))
	mux.Handle("GET /audit", viewer(http.HandlerFunc(w.audit)))
	mux.Handle("GET /limits", viewer(http.HandlerFunc(w.limits)))
	mux.Handle("POST /limits/act", operator(http.HandlerFunc(w.limitsAct)))
	mux.Handle("GET /errors", viewer(http.HandlerFunc(w.errorsPage)))
	mux.Handle("POST /errors/act", operator(http.HandlerFunc(w.errorsAct)))
	mux.Handle("GET /scenarios", viewer(http.HandlerFunc(w.scenarios)))
	mux.Handle("POST /scenarios/act", operator(http.HandlerFunc(w.scenariosAct)))
	mux.Handle("GET /config", viewer(http.HandlerFunc(w.configPage)))
	mux.Handle("POST /config/policies", operator(http.HandlerFunc(w.configPolicies)))
}

// SecurityHeaders adds the CSP, X-Frame-Options: DENY, nosniff and the
// referrer policy to every response of h (pages, static and API).
func SecurityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hd := rw.Header()
		hd.Set("Content-Security-Policy", CSP)
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(rw, r)
	})
}

// ---- static assets (name with the content hash, long cache) ----

type asset struct {
	body  []byte
	ctype string
}

var (
	// assetPath maps a file of static/ to its hashed URL.
	assetPath = map[string]string{}
	// assetByName maps the hashed file name to the content.
	assetByName = map[string]asset{}
)

func init() {
	entries, _ := fs.ReadDir(staticFS, "static")
	for _, e := range entries {
		body, _ := staticFS.ReadFile("static/" + e.Name())
		sum := sha256.Sum256(body)
		ext := path.Ext(e.Name())
		hashed := strings.TrimSuffix(e.Name(), ext) + "." + hex.EncodeToString(sum[:6]) + ext
		assetPath[e.Name()] = "/static/" + hashed
		assetByName[hashed] = asset{body: body, ctype: mime.TypeByExtension(ext)}
	}
}

// serveStatic serves an embedded file by its hashed name, cached for a
// year (the name changes when the content does).
func serveStatic(rw http.ResponseWriter, r *http.Request) {
	a, ok := assetByName[r.PathValue("name")]
	if !ok {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", a.ctype)
	rw.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = rw.Write(a.body)
}

// ---- templates ----

// pageNames are the files of templates/ besides layout.html.
var pageNames = []string{"login", "overview", "usage", "blocks", "audit", "limits", "errors", "scenarios", "config"}

var pages = parsePages()

func parsePages() map[string]*template.Template {
	base := template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html"))
	out := map[string]*template.Template{}
	for _, n := range pageNames {
		out[n] = template.Must(template.Must(base.Clone()).ParseFS(templateFS, "templates/"+n+".html"))
	}
	return out
}

// view is the data of every page.
type view struct {
	Title string
	Nav   string // overview | usage | blocks | audit | limits | errors | scenarios | config ("" = login)
	Me    *auth.Identity
	// CanAct shows the action buttons (operator).
	CanAct bool
	// Live is the URL the live region polls (path + query).
	Live  string
	Flash string
	Err   string
	// ErrDetails are the Problem details of Err.
	ErrDetails []string
	Data       any
}

// CSRF is the token of the session ("" with the bearer).
func (v view) CSRF() string {
	if v.Me == nil {
		return ""
	}
	return v.Me.CSRF
}

// isHX reports a request made by HTMX (only the live region is rendered).
func isHX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// newView fills the identity of the request.
func newView(r *http.Request, title, nav string) view {
	v := view{Title: title, Nav: nav}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.Me, v.CanAct = id, id.CanAct()
	}
	return v
}

// fail puts err (service layer) in v and returns its HTTP status.
func (w *Web) fail(v *view, err error, what string) int {
	st, p := api.ProblemOf(err, what)
	if st >= http.StatusInternalServerError {
		w.log.Warn("painel: tela", "page", v.Nav, "status", st, "err", err)
	}
	v.Err, v.ErrDetails = p.Message, p.Details
	return st
}

// render writes page: the whole layout, or only the "live" template for an
// HTMX request. HTMX requests always get 200 so the error is swapped in and
// the polling goes on.
func (w *Web) render(rw http.ResponseWriter, r *http.Request, page string, status int, v view) {
	name := "layout"
	if isHX(r) {
		name, status = "live", http.StatusOK
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(status)
	if err := pages[page].ExecuteTemplate(rw, name, v); err != nil {
		w.log.Error("painel: renderizar", "page", page, "err", err)
	}
}

// ---- template functions ----

var funcs = template.FuncMap{
	"asset": func(name string) string { return assetPath[name] },
	"opt":   optFloat,
	"num":   num,
	"int":   optInt,
	"pct":   pct,
	"usd":   usd,
	"when":  when,
	"whenp": whenPtr,
	"ib":    breakdown,
	"known": func(m map[string]float64) bool { return m != nil },
}

const dash = "—"

// num formats with up to 4 decimals, without trailing zeros.
func num(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(v, 'f', 4, 64), "0"), ".")
}

func optFloat(v *float64) string {
	if v == nil {
		return dash
	}
	return num(*v)
}

func optInt(v *int64) string {
	if v == nil {
		return dash
	}
	return num(float64(*v))
}

func pct(v *float64) string {
	if v == nil {
		return dash
	}
	return num(*v*100) + " %"
}

func usd(v *float64) string {
	if v == nil {
		return dash
	}
	return "US$ " + num(*v)
}

func when(t time.Time) string {
	if t.IsZero() {
		return dash
	}
	return t.UTC().Format("2006-01-02 15:04:05") + " UTC"
}

func whenPtr(t *time.Time) string {
	if t == nil {
		return dash
	}
	return when(*t)
}
