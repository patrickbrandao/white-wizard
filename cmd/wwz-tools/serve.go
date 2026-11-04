package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/tools"
)

// maxBody is the request body limit (openapi.yaml: 1 MiB).
const maxBody = 1 << 20

// Seams for tests.
var (
	listen          = net.Listen
	shutdownTimeout = 5 * time.Second
)

// runServe starts the HTTP server and blocks until ctx ends (SIGINT or
// SIGTERM), then shuts down gracefully within shutdownTimeout.
func runServe(ctx context.Context, args []string, stderr io.Writer) int {
	fs := newFlagSet("serve", stderr)
	addr := fs.String("listen", "", "listen address (default :9200; overrides WWZ_TOOLS_LISTEN)")
	cfgPath := fs.String("config", "", "tools.yaml")
	seed := seedFlag(fs)
	if code, done := parseFlags(fs, args); done {
		return code
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := tools.Load(tools.Flags{Config: *cfgPath, Listen: *addr, Seed: *seed}, os.Getenv)
	if err != nil {
		log.Error("configuração inválida", "err", err.Error())
		return exitError
	}
	ex, _ := tools.New(cfg, execOptions...) // cfg was validated by Load
	ln, err := listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("listen", "addr", cfg.Listen, "err", err.Error())
		return exitError
	}
	srv := &http.Server{
		Handler:           newServer(ex, cfg.Token).handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("wwz-tools no ar", "addr", ln.Addr().String(), "auth", cfg.Token != "")
	select {
	case err := <-errc:
		log.Error("serve", "err", err.Error())
		return exitError
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	log.Info("wwz-tools encerrado")
	return exitOK
}

// server is the HTTP layer over a tools.Executor (cmd/wwz-tools/openapi.yaml).
type server struct {
	ex       *tools.Executor
	token    []byte
	reg      *metrics.Registry
	calls    *metrics.Counter
	duration *metrics.Histogram
	sessions *metrics.Gauge
}

func newServer(ex *tools.Executor, token string) *server {
	reg := metrics.New(0)
	return &server{
		ex:       ex,
		token:    []byte(token),
		reg:      reg,
		calls:    reg.NewCounter("wwz_tools_calls_total", "Tool calls by tool and outcome", "tool", "outcome"),
		duration: reg.NewHistogram("wwz_tools_call_duration_seconds", "Tool call duration, simulated latency included", metrics.DefaultBuckets, "tool"),
		sessions: reg.NewGauge("wwz_tools_sessions", "Live sessions"),
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /v1/tools", listTools)
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.deleteSession)
	mux.HandleFunc("POST /v1/call", s.call)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no route for "+r.Method+" "+r.URL.Path)
	})
	return s.auth(mux)
}

// auth requires the bearer token on every route except /healthz when a
// token is configured, compared in constant time.
func (s *server) auth(next http.Handler) http.Handler {
	want := append([]byte("Bearer "), s.token...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.token) > 0 && r.URL.Path != "/healthz" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok")
}

func (s *server) metrics(w http.ResponseWriter, _ *http.Request) {
	s.sessions.Set(int64(s.ex.Sessions()))
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.reg.Write(w)
}

func listTools(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	raw, err := tools.Catalog(tools.Profile(q.Get("profile")), tools.Format(q.Get("format")))
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(raw, '\n')) //nolint:gosec // G705: fixed catalogue JSON, not user input
}

// sessionResponse is the Session schema.
type sessionResponse struct {
	Session string        `json:"session"`
	Profile tools.Profile `json:"profile"`
	Seed    int64         `json:"seed"`
	Files   []string      `json:"files"`
}

func (s *server) createSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Profile string `json:"profile"`
		Seed    int64  `json:"seed"`
	}
	if err := decodeBody(w, r, &req, true); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Seed < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "seed: must be >= 0")
		return
	}
	seed, err := seedHeader(r, req.Seed)
	if err != nil {
		writeErr(w, err)
		return
	}
	// The id is generated, so the only possible error is the profile.
	info, err := s.ex.NewSession(tools.SessionOptions{Profile: tools.Profile(req.Profile), Seed: seed})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sessionResponse{info.ID, info.Profile, info.Seed, info.Files})
}

func (s *server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.ex.DeleteSession(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var sessionID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func (s *server) call(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Session    string          `json:"session"`
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		ToolCallID string          `json:"tool_call_id"`
	}
	bad := func(msg string) { writeError(w, http.StatusBadRequest, "invalid_request", msg) }
	if err := decodeBody(w, r, &req, false); err != nil {
		bad(err.Error())
		return
	}
	if !sessionID.MatchString(req.Session) {
		bad("session: required, 1-128 characters of [A-Za-z0-9._-]")
		return
	}
	if req.Name == "" {
		bad("name: required")
		return
	}
	// Object or string with JSON (bad JSON inside the string or arguments
	// outside the schema are a natural_error of the tool); any other JSON
	// type is an invalid body.
	if a := bytes.TrimSpace(req.Arguments); len(a) > 0 && a[0] != '{' && a[0] != '"' {
		bad("arguments: want a JSON object or a string with JSON")
		return
	}
	forced, err := tools.ParseOutcome(r.Header.Get("x-wwz-tools-outcome"))
	if err != nil {
		writeErr(w, err)
		return
	}
	seed, err := seedHeader(r, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	start := time.Now()
	res, err := s.ex.Call(r.Context(), tools.CallRequest{
		Session:      req.Session,
		Name:         req.Name,
		Arguments:    req.Arguments,
		ToolCallID:   req.ToolCallID,
		ForceOutcome: forced,
		Seed:         seed,
	})
	if err != nil {
		if r.Context().Err() == nil {
			writeErr(w, err)
		} // client gone: nothing is written
		return
	}
	s.calls.Inc(req.Name, string(res.Outcome))
	s.duration.Observe(time.Since(start).Seconds(), req.Name)
	if res.Outcome == tools.OutcomeTimeout {
		writeError(w, http.StatusGatewayTimeout, "timeout", res.Content)
		return
	}
	writeJSON(w, http.StatusOK, toCallResult(res))
}

// errInvalidRequest marks request errors detected by this layer.
var errInvalidRequest = errors.New("invalid request")

// seedHeader returns x-wwz-seed when present (it wins over def), else def.
func seedHeader(r *http.Request, def int64) (int64, error) {
	vs := r.Header.Values("x-wwz-seed")
	if len(vs) == 0 {
		return def, nil
	}
	n, err := strconv.ParseInt(vs[0], 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: x-wwz-seed: want an integer >= 0, got %q", errInvalidRequest, vs[0])
	}
	return n, nil
}

// decodeBody decodes a JSON body of at most maxBody bytes, rejecting
// unknown fields and trailing data. optional accepts an empty body.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any, optional bool) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			return fmt.Errorf("body larger than %d bytes", maxBody)
		case errors.Is(err, io.EOF) && optional:
			return nil
		case errors.Is(err, io.EOF):
			return errors.New("body: required")
		}
		return fmt.Errorf("body: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("body: trailing data after the JSON object")
	}
	return nil
}

// writeErr maps core errors to HTTP (task 112): unknown tool or session →
// 404; invalid profile, format, outcome or request → 400; anything else →
// 500.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tools.ErrUnknownTool), errors.Is(err, tools.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, tools.ErrInvalidProfile), errors.Is(err, tools.ErrInvalidFormat),
		errors.Is(err, tools.ErrInvalidOutcome), errors.Is(err, errInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	var b errorBody
	b.Error.Type, b.Error.Message = typ, msg
	writeJSON(w, status, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
