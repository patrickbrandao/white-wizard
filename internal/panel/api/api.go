// Package api is the JSON API of wwz-panel (cmd/wwz-panel/openapi.yaml,
// spec 016): /api/v1/*, /auth/oidc/*, /health, /health/ready and /metrics.
//
// The exported methods of API (Overview, Usage, Entities, Act, ...) are the
// service layer shared with the HTML screens (task 050): handlers only parse
// the request, call them and write JSON. Errors returned by the service
// layer are *Error (status + Problem) or source errors, mapped by ProblemOf.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/auth"
	"github.com/patrickbrandao/white-wizard/internal/panel/source"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// Roles of x-wwz-role.
const (
	RoleNone     = "none"
	RoleViewer   = store.RoleViewer
	RoleOperator = store.RoleOperator
)

// Options configures New.
type Options struct {
	Store *store.Store  // required
	Auth  *auth.Service // required
	Admin *source.Admin // required: wwz control plane
	// Valkey is nil in the mode without Valkey (store: memory).
	Valkey *source.Valkey
	// Prom is nil without Prometheus.
	Prom *source.Prom
	// Metrics combines Prom and Admin (nil = NewMetrics(Prom, Admin)).
	Metrics *source.Metrics
	// Consumer (with Valkey) and Poller (without) feed the events table;
	// they only report counters to /metrics here. Both may be nil.
	Consumer *source.Consumer
	Poller   *source.RequestPoller
	Logger   *slog.Logger // nil = slog.Default()
	// Now is the clock (nil = time.Now).
	Now func() time.Time
	// ClientIP extracts the client address for audit (nil = host of
	// RemoteAddr).
	ClientIP func(*http.Request) string
	// PingTimeout bounds each source check of /health/ready and /metrics
	// (0 = 2 s).
	PingTimeout time.Duration
}

// API serves the panel API.
type API struct {
	st       *store.Store
	auth     *auth.Service
	admin    *source.Admin
	vk       *source.Valkey
	prom     *source.Prom
	metrics  *source.Metrics
	consumer *source.Consumer
	poller   *source.RequestPoller
	log      *slog.Logger
	now      func() time.Time
	clientIP func(*http.Request) string
	pingTO   time.Duration
}

// New builds the API.
func New(o Options) (*API, error) {
	if o.Store == nil || o.Auth == nil || o.Admin == nil {
		return nil, errors.New("api: Store, Auth e Admin são obrigatórios")
	}
	a := &API{
		st: o.Store, auth: o.Auth, admin: o.Admin, vk: o.Valkey, prom: o.Prom, metrics: o.Metrics,
		consumer: o.Consumer, poller: o.Poller, log: cmp.Or(o.Logger, slog.Default()),
		now: o.Now, clientIP: o.ClientIP, pingTO: cmp.Or(o.PingTimeout, 2*time.Second),
	}
	if a.metrics == nil {
		a.metrics = source.NewMetrics(o.Prom, o.Admin)
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.clientIP == nil {
		a.clientIP = remoteIP
	}
	return a, nil
}

// Route is one operation of the contract served by the API.
type Route struct {
	Method string
	Path   string // OpenAPI path ({kind}, {id}, {name})
	Role   string // none | viewer | operator (x-wwz-role)
}

// handler pairs a route with its implementation.
type handler struct {
	Route
	h http.Handler
}

func (a *API) table() []handler {
	ent := "/api/v1/entities/{kind}/{id}"
	return []handler{
		{Route{"GET", "/health", RoleNone}, http.HandlerFunc(a.health)},
		{Route{"GET", "/health/ready", RoleNone}, http.HandlerFunc(a.ready)},
		{Route{"GET", "/metrics", RoleNone}, http.HandlerFunc(a.metricsHandler)},
		{Route{"GET", "/auth/oidc/login", RoleNone}, a.auth.OIDCLoginHandler()},
		{Route{"GET", "/auth/oidc/callback", RoleNone}, a.auth.OIDCCallbackHandler()},
		{Route{"POST", "/api/v1/login", RoleNone}, a.auth.LoginHandler()},
		{Route{"POST", "/api/v1/logout", RoleViewer}, a.auth.LogoutHandler()},
		{Route{"GET", "/api/v1/me", RoleViewer}, a.auth.MeHandler()},
		{Route{"GET", "/api/v1/overview", RoleViewer}, http.HandlerFunc(a.getOverview)},
		{Route{"GET", "/api/v1/usage", RoleViewer}, http.HandlerFunc(a.getUsage)},
		{Route{"GET", "/api/v1/usage/series", RoleViewer}, http.HandlerFunc(a.getUsageSeries)},
		{Route{"GET", "/api/v1/entities", RoleViewer}, http.HandlerFunc(a.listEntities)},
		{Route{"GET", ent, RoleViewer}, http.HandlerFunc(a.getEntity)},
		{Route{"POST", ent + "/block", RoleOperator}, a.action(ActionBlock)},
		{Route{"POST", ent + "/unblock", RoleOperator}, a.action(ActionUnblock)},
		{Route{"POST", ent + "/ban", RoleOperator}, a.action(ActionBan)},
		{Route{"POST", ent + "/unban", RoleOperator}, a.action(ActionUnban)},
		{Route{"POST", ent + "/forgive", RoleOperator}, a.action(ActionForgive)},
		{Route{"POST", ent + "/leak", RoleOperator}, a.action(ActionLeak)},
		{Route{"POST", ent + "/credits", RoleOperator}, a.action(ActionCredits)},
		{Route{"POST", ent + "/tier", RoleOperator}, a.action(ActionTier)},
		{Route{"POST", ent + "/reset", RoleOperator}, a.action(ActionReset)},
		{Route{"GET", "/api/v1/instances", RoleViewer}, http.HandlerFunc(a.getInstances)},
		{Route{"GET", "/api/v1/events", RoleViewer}, http.HandlerFunc(a.listEvents)},
		{Route{"GET", "/api/v1/errors", RoleViewer}, http.HandlerFunc(a.getErrors)},
		{Route{"POST", "/api/v1/injection", RoleOperator}, http.HandlerFunc(a.postInjection)},
		{Route{"GET", "/api/v1/chaos", RoleViewer}, http.HandlerFunc(a.getChaos)},
		{Route{"POST", "/api/v1/chaos", RoleOperator}, http.HandlerFunc(a.postChaos)},
		{Route{"DELETE", "/api/v1/chaos", RoleOperator}, http.HandlerFunc(a.deleteChaos)},
		{Route{"GET", "/api/v1/scenarios", RoleViewer}, http.HandlerFunc(a.getScenarios)},
		{Route{"POST", "/api/v1/scenarios", RoleOperator}, http.HandlerFunc(a.postScenarios)},
		{Route{"DELETE", "/api/v1/scenarios/{name}", RoleOperator}, http.HandlerFunc(a.deleteScenario)},
		{Route{"GET", "/api/v1/config", RoleViewer}, http.HandlerFunc(a.getConfig)},
		{Route{"GET", "/api/v1/policies", RoleViewer}, http.HandlerFunc(a.getPolicies)},
		{Route{"PUT", "/api/v1/policies", RoleOperator}, http.HandlerFunc(a.putPolicies)},
		{Route{"GET", "/api/v1/audit", RoleViewer}, http.HandlerFunc(a.listAudit)},
	}
}

// Routes lists every operation the API registers (contract test).
func (a *API) Routes() []Route {
	t := a.table()
	out := make([]Route, len(t))
	for i, h := range t {
		out[i] = h.Route
	}
	return out
}

// Register mounts every route on mux ("METHOD /path" patterns of Go 1.22+).
// Login, logout, me and OIDC come wrapped by the auth package; the other
// viewer/operator routes get RequireRole (401, 403 csrf, 403 forbidden).
func (a *API) Register(mux *http.ServeMux) {
	for _, h := range a.table() {
		hh := h.h
		if h.Role != RoleNone && h.Path != "/api/v1/logout" && h.Path != "/api/v1/me" {
			hh = a.auth.RequireRole(h.Role)(hh)
		}
		mux.Handle(h.Method+" "+h.Path, hh)
	}
}

// Handler is a mux with only the API routes.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	a.Register(mux)
	return mux
}

// ---- errors ----

// Error is a failure with its HTTP status and Problem.
type Error struct {
	Status  int
	Code    string
	Message string
	Details []string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// dbErr marks a failure of the panel SQLite (500 internal).
func dbErr(err error) error {
	return &Error{Status: http.StatusInternalServerError, Code: "internal", Message: "erro no banco do painel", Details: []string{err.Error()}}
}

func invalid(msg string, details ...string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid", Message: msg, Details: details}
}

// ProblemOf maps an error of the service layer to the HTTP status and
// Problem of the contract: *Error as is; admin 400/404 passed through with
// the admin message in details; any other source failure 502
// upstream_unavailable (what = "admin", "valkey", "prometheus").
func ProblemOf(err error, what string) (int, auth.Problem) {
	var e *Error
	if errors.As(err, &e) {
		return e.Status, auth.Problem{Error: e.Code, Message: e.Message, Details: e.Details}
	}
	var ae *source.AdminError
	if errors.As(err, &ae) {
		switch st := ae.HTTPStatus(); st {
		case http.StatusBadRequest, http.StatusNotFound:
			code := "invalid"
			if st == http.StatusNotFound {
				code = "not_found"
			}
			details := []string{}
			if ae.Problem.Message != "" {
				details = append(details, ae.Problem.Message)
			}
			details = append(details, ae.Problem.Details...)
			return st, auth.Problem{Error: code, Message: "o wwz recusou a requisição", Details: details}
		}
		what = "admin"
	}
	return http.StatusBadGateway, auth.Problem{
		Error: "upstream_unavailable", Message: "fonte de dados indisponível", Details: []string{what + ": " + err.Error()},
	}
}

func (a *API) fail(w http.ResponseWriter, err error, what string) {
	st, p := ProblemOf(err, what)
	if st >= http.StatusInternalServerError {
		a.log.Warn("painel: api", "status", st, "err", err)
	}
	auth.WriteProblem(w, st, p.Error, p.Message, p.Details...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ctxTimeout bounds a source check.
func (a *API) ctxTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, a.pingTO)
}
