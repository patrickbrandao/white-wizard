// Package admin is the control plane of wwz (net/http): /health,
// /health/ready, /metrics, /dashboard and the /admin/* API described in
// cmd/wwz/openapi.yaml. /admin/* requires a bearer token compared in constant
// time; without a token only loopback peers are accepted (ADR 0004).
package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// TokenEnv is the environment variable holding the admin bearer token.
const TokenEnv = "WWZ_ADMIN_TOKEN" //nolint:gosec // G101: variable name, not a credential

// maxBody bounds request bodies of the control plane.
const maxBody = 8 << 20

// Options configures the control plane.
type Options struct {
	// Engine is the data-plane engine whose configuration, store, injector,
	// scenarios, recorder and metrics the control plane operates. Required.
	Engine *engine.Engine
	// ConfigPath is the YAML file re-read by an empty POST /admin/config and
	// by SIGHUP. Empty means there is no file to reload (unless Load is set).
	ConfigPath string
	// Load re-reads the configuration from disk; default config.Load(ConfigPath).
	Load func() (*config.Config, error)
	// Override is applied to every new configuration (from file or patch)
	// before validation, e.g. to re-apply command-line flags.
	Override func(*config.Config)
	// Check is an extra validation run before a new configuration is swapped
	// in (e.g. refuse changes that need a restart).
	Check func(*config.Config) error
	// Token is the bearer token for /admin/*. Empty falls back to
	// $WWZ_ADMIN_TOKEN, then to admin.token of the current configuration.
	// With no token at all only loopback peers may call /admin/*.
	Token string
	// PodName is reported by /admin/stats; default $POD_NAME or hostname.
	PodName string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Server is the control plane.
type Server struct {
	eng      *engine.Engine
	opts     Options
	log      *slog.Logger
	now      func() time.Time
	mux      *http.ServeMux
	pod      string
	token    string
	notReady atomic.Bool

	// cfgMu serialises configuration changes (patch, reload).
	cfgMu     sync.Mutex
	observers []func(*config.Config)
	obsMu     sync.RWMutex

	statsMu  sync.Mutex
	baseline map[string]float64
	resetAt  time.Time
}

// New builds the control plane. Call it before the data plane starts
// serving: when admin.debug is on and the engine has no Recorder, New
// installs one.
func New(opts Options) (*Server, error) {
	if opts.Engine == nil {
		return nil, errors.New("admin: Engine is required")
	}
	s := &Server{eng: opts.Engine, opts: opts, log: opts.Logger, now: opts.Now, pod: opts.PodName}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.token = opts.Token
	if s.token == "" {
		s.token = os.Getenv(TokenEnv)
	}
	if s.pod == "" {
		s.pod = os.Getenv("POD_NAME")
	}
	if s.pod == "" {
		s.pod, _ = os.Hostname()
	}
	if cfg := s.eng.Config(); cfg != nil && cfg.Admin.Debug && s.eng.Recorder == nil {
		s.eng.Recorder = engine.NewRecorder(1000)
	}
	s.routes()
	return s, nil
}

// Handler returns the HTTP handler of the control plane.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

// SetReady marks the instance as (not) ready, on top of server.startup_duration.
// The default is ready.
func (s *Server) SetReady(ready bool) { s.notReady.Store(!ready) }

// effectiveToken is the token that /admin/* requires ("" = loopback only).
func (s *Server) effectiveToken() string {
	if s.token != "" {
		return s.token
	}
	if cfg := s.eng.Config(); cfg != nil {
		return cfg.Admin.Token
	}
	return ""
}

// TokenConfigured reports whether a bearer token protects /admin/*.
func (s *Server) TokenConfigured() bool { return s.effectiveToken() != "" }

// ListenAddr returns the address the control plane should bind to: listen
// unchanged when a token is configured, otherwise the same port on 127.0.0.1
// (ADR 0004). restricted tells the caller to log a warning.
func ListenAddr(listen, token string) (addr string, restricted bool) {
	if token != "" {
		return listen, false
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "127.0.0.1:" + strings.TrimPrefix(listen, ":"), true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() || host == "localhost" {
		return listen, true
	}
	return net.JoinHostPort("127.0.0.1", port), true
}

// ListenAddr returns where this server should listen given the current
// configuration (see the package-level ListenAddr).
func (s *Server) ListenAddr() (string, bool) {
	listen := ":9000"
	if cfg := s.eng.Config(); cfg != nil && cfg.Admin.Listen != "" {
		listen = cfg.Admin.Listen
	}
	return ListenAddr(listen, s.effectiveToken())
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") {
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			problem(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) authorized(r *http.Request) bool {
	tok := s.effectiveToken()
	if tok == "" {
		return isLoopback(r.RemoteAddr)
	}
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("GET /health", s.health)
	m.HandleFunc("GET /health/ready", s.ready)
	m.HandleFunc("GET /metrics", s.metrics)
	m.HandleFunc("GET /dashboard", s.dashboard)

	m.HandleFunc("GET /admin/config", s.getConfig)
	m.HandleFunc("POST /admin/config", s.postConfig)
	m.HandleFunc("GET /admin/stats", s.getStats)
	m.HandleFunc("POST /admin/reset", s.postReset)
	m.HandleFunc("GET /admin/requests", s.getRequests)
	m.HandleFunc("GET /admin/scenarios", s.getScenarios)
	m.HandleFunc("POST /admin/scenarios", s.postScenarios)
	m.HandleFunc("DELETE /admin/scenarios", s.deleteScenario)
	m.HandleFunc("POST /admin/chaos", s.postChaos)
	m.HandleFunc("GET /admin/chaos", s.getChaos)
	m.HandleFunc("DELETE /admin/chaos", s.deleteChaos)
	m.HandleFunc("POST /admin/chaos/next", s.postChaosNext)
	m.HandleFunc("GET /admin/entities", s.listEntities)
	m.HandleFunc("GET /admin/entities/{tipo}/{id}", s.getEntity)
	m.HandleFunc("POST /admin/entities/{tipo}/{id}/block", s.blockEntity)
	m.HandleFunc("POST /admin/entities/{tipo}/{id}/unblock", s.unblockEntity)
	m.HandleFunc("POST /admin/entities/{tipo}/{id}/ban", s.banEntity)
	m.HandleFunc("POST /admin/entities/{tipo}/{id}/unban", s.unbanEntity)
	m.HandleFunc("POST /admin/entities/{tipo}/{id}/credits", s.setCredits)
	m.HandleFunc("POST /admin/entities/{tipo}/{id}/tier", s.setTier)
	m.HandleFunc("POST /admin/entities/{tipo}/{id}/reset", s.resetEntity)
	m.HandleFunc("POST /admin/leak", s.postLeak)
	m.HandleFunc("GET /admin/resilience", s.getResilience)
	m.HandleFunc("POST /admin/conversations/reset", s.resetConversations)
	// Unknown paths get the mux's 404 and wrong methods its 405 with Allow.
	s.mux = m
}

// Health is the body of /health and /health/ready.
type Health struct {
	Status string `json:"status"`
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Health{Status: "ok"})
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	remaining := time.Duration(0)
	if cfg := s.eng.Config(); cfg != nil && cfg.Server.StartupDuration > 0 {
		remaining = s.eng.Started().Add(cfg.Server.StartupDuration).Sub(s.now())
	}
	if remaining > 0 || s.notReady.Load() {
		secs := int64(0)
		if remaining > 0 {
			secs = int64((remaining + time.Second - 1) / time.Second)
		}
		w.Header().Set("Retry-After", itoa(secs))
		writeJSON(w, http.StatusServiceUnavailable, Health{Status: "starting"})
		return
	}
	writeJSON(w, http.StatusOK, Health{Status: "ready"})
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if s.eng.M != nil && s.eng.M.Registry != nil {
		s.eng.M.Registry.Write(w)
	}
}

// Problem is the error body of the control plane.
type Problem struct {
	Error   string   `json:"error"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

func problem(w http.ResponseWriter, status int, code, msg string, details ...string) {
	writeJSON(w, status, Problem{Error: code, Message: msg, Details: details})
}

func invalid(w http.ResponseWriter, msg string, details ...string) {
	problem(w, http.StatusBadRequest, "invalid", msg, details...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// readBody reads at most maxBody bytes.
func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, maxBody+1))
}

// decodeJSON decodes an optional JSON body strictly (unknown fields fail).
// It reports whether a body was present.
func decodeJSON(r *http.Request, v any) (bool, error) {
	b, err := readBody(r)
	if err != nil {
		return false, err
	}
	if len(b) > maxBody {
		return true, errors.New("body too large")
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return false, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return true, err
	}
	if dec.More() {
		return true, errors.New("trailing data after JSON body")
	}
	return true, nil
}

// mediaType returns the lower-cased media type of the request ("" if absent).
func mediaType(r *http.Request) string {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return strings.ToLower(ct)
	}
	return mt
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// storeCtx returns a bounded context for store calls.
func storeCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}
