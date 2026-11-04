// Package server is the wwz data plane: a fasthttp server (optionally one
// SO_REUSEPORT listener per CPU) that resolves the client IP, routes the
// request to a driver by mount prefix and path, applies server.max_rps and
// hands the request to the engine with a context cancelled when the client
// goes away.
//
// The server follows the engine's current configuration: when the control
// plane swaps the config (engine.SetConfig) the route table and the trusted
// proxy set are rebuilt lazily on the next request. Update swaps the engine
// itself.
package server

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/clientip"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/ids"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// Options tunes the server. The zero value is valid.
type Options struct {
	// Logger receives server errors; nil uses slog.Default().
	Logger *slog.Logger
	// Listeners is the number of SO_REUSEPORT listeners ListenAndServe opens
	// when server.reuseport is on; 0 means runtime.GOMAXPROCS(0).
	Listeners int
	// HangFor is how long a "hang" transport fault holds the connection;
	// 0 keeps the writer default.
	HangFor time.Duration
}

// state is what one configuration derives: routes and trusted proxies.
type state struct {
	cfg    *config.Config
	routes *router
	ip     *clientip.Resolver
}

// Server is the data plane.
type Server struct {
	opts    Options
	log     *slog.Logger
	srv     *fasthttp.Server
	eng     atomic.Pointer[engine.Engine]
	st      atomic.Pointer[state]
	mu      sync.Mutex     // serialises rebuilds
	failed  *config.Config // last config whose rebuild failed (not retried)
	limiter gcra
	base    context.Context
	cancel  context.CancelFunc

	conns   atomic.Int64               // TCP connections open (wwz_connections_open)
	selfMu  sync.Mutex                 // serialises self-signed generation
	selfTLS atomic.Pointer[tls.Config] // server.tls.self_signed, nil until generated
}

// CheckConfig reports whether cfg can be served: every mounted driver is
// registered and every prefix is valid. The control plane calls it before
// swapping a configuration.
func CheckConfig(cfg *config.Config) error {
	_, err := newState(cfg)
	return err
}

func newState(cfg *config.Config) (*state, error) {
	if cfg == nil {
		return nil, errors.New("server: nil config")
	}
	rt, err := buildRouter(cfg)
	if err != nil {
		return nil, err
	}
	ip, err := clientip.Load(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	return &state{cfg: cfg, routes: rt, ip: ip}, nil
}

// New builds a data plane serving eng with its current configuration.
// Transport settings (concurrency, timeouts, body size) are read here and
// need a restart to change; routes, trusted proxies and max_rps follow
// reloads.
func New(eng *engine.Engine, opts Options) (*Server, error) {
	if eng == nil {
		return nil, errors.New("server: nil engine")
	}
	cfg := eng.Config()
	st, err := newState(cfg)
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{opts: opts, log: log}
	s.base, s.cancel = context.WithCancel(context.Background())
	s.eng.Store(eng)
	s.st.Store(st)

	bodyMB := max(cfg.Server.MaxRequestBodyMB*2, 8)
	s.srv = &fasthttp.Server{
		Handler:               s.Handle,
		Concurrency:           cfg.Server.Concurrency,
		ReadTimeout:           cfg.Server.ReadTimeout,
		WriteTimeout:          cfg.Server.WriteTimeout,
		IdleTimeout:           cfg.Server.IdleTimeout,
		MaxRequestBodySize:    bodyMB * 1024 * 1024,
		NoDefaultServerHeader: true,
		NoDefaultContentType:  true,
		CloseOnShutdown:       true,
		Logger:                fastLogger{log},
		ConnState:             s.connState,
	}
	if t := cfg.Server.TLS; t.SelfSigned && t.CertFile == "" {
		if _, err := s.selfSignedTLS(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Update swaps the engine (and with it the configuration) atomically.
// Requests already running finish on the engine they started with.
func (s *Server) Update(eng *engine.Engine) error {
	if eng == nil {
		return errors.New("server: nil engine")
	}
	st, err := newState(eng.Config())
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.eng.Store(eng)
	s.st.Store(st)
	s.failed = nil
	s.mu.Unlock()
	return nil
}

// OpenConnections returns the TCP connections open on the data plane now,
// across every listener (wwz_connections_open, spec 013). It is counted from
// the connection states: fasthttp's own open count includes one per Serve
// call, which is wrong with several SO_REUSEPORT listeners.
func (s *Server) OpenConnections() int64 { return s.conns.Load() }

func (s *Server) connState(_ net.Conn, st fasthttp.ConnState) {
	switch st {
	case fasthttp.StateNew:
		s.conns.Add(1)
	case fasthttp.StateClosed, fasthttp.StateHijacked:
		s.conns.Add(-1)
	}
}

// Engine returns the engine serving new requests.
func (s *Server) Engine() *engine.Engine { return s.eng.Load() }

// Config returns the configuration the current route table was built from.
func (s *Server) Config() *config.Config { return s.st.Load().cfg }

// current returns the engine and the state matching its configuration,
// rebuilding the state when the engine's config pointer changed.
func (s *Server) current() (*engine.Engine, *state) {
	eng := s.eng.Load()
	cfg := eng.Config()
	st := s.st.Load()
	if st.cfg == cfg {
		return eng, st
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st = s.st.Load(); st.cfg == cfg || s.failed == cfg {
		return eng, st
	}
	nst, err := newState(cfg)
	if err != nil {
		s.failed = cfg
		s.log.Error("data plane keeps the previous routes: new config rejected", "err", err)
		return eng, st
	}
	s.st.Store(nst)
	return eng, nst
}

// Serve accepts connections on ln until Shutdown. It may be called for
// several listeners concurrently. server.tls.cert_file/key_file win over
// server.tls.self_signed.
func (s *Server) Serve(ln net.Listener) error {
	t := s.Config().Server.TLS
	if t.CertFile != "" {
		return s.srv.ServeTLS(ln, t.CertFile, t.KeyFile)
	}
	if t.SelfSigned {
		tc, err := s.selfSignedTLS()
		if err != nil {
			return err
		}
		return s.srv.Serve(tls.NewListener(ln, tc))
	}
	return s.srv.Serve(ln)
}

// Listen opens the data plane listeners for addr ("" = server.listen): one
// per CPU (or Options.Listeners) with SO_REUSEPORT when server.reuseport is
// on and the platform supports it, otherwise one.
func (s *Server) Listen(addr string) ([]net.Listener, error) {
	cfg := s.Config()
	if addr == "" {
		addr = cfg.Server.Listen
	}
	if !cfg.Server.Reuseport || !reuseportSupported {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		return []net.Listener{ln}, nil
	}
	n := s.opts.Listeners
	if n <= 0 {
		n = runtime.GOMAXPROCS(0)
	}
	lc := net.ListenConfig{Control: reuseportControl}
	lns := make([]net.Listener, 0, n)
	for i := 0; i < n; i++ {
		ln, err := lc.Listen(context.Background(), "tcp", addr)
		if err != nil {
			for _, l := range lns {
				_ = l.Close()
			}
			return nil, err
		}
		if i == 0 {
			// ":0" picks a port; the other listeners must share it.
			if ta, ok := ln.Addr().(*net.TCPAddr); ok {
				host, _, _ := net.SplitHostPort(addr)
				addr = net.JoinHostPort(host, strconv.Itoa(ta.Port))
			}
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// ListenAndServe opens the listeners (see Listen) and serves them until
// Shutdown, returning the first error.
func (s *Server) ListenAndServe(addr string) error {
	lns, err := s.Listen(addr)
	if err != nil {
		return err
	}
	errc := make(chan error, len(lns))
	for _, ln := range lns {
		go func(ln net.Listener) { errc <- s.Serve(ln) }(ln)
	}
	return <-errc
}

// Shutdown stops accepting connections and waits for in-flight requests and
// streams until ctx ends; then it cancels whatever is still running.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.ShutdownWithContext(ctx)
	s.cancel()
	return err
}

// Handle is the fasthttp request handler.
func (s *Server) Handle(ctx *fasthttp.RequestCtx) {
	eng, st := s.current()
	h := readHeaders(&ctx.Request.Header)
	if q := ctx.QueryArgs().QueryString(); len(q) > 0 {
		h[driver.QueryHeader] = string(q)
	}
	h[driver.MethodHeader] = string(ctx.Method())

	// 1. Client IP, before anything else looks at the request.
	ipr := st.ip.Resolve(ctx.RemoteAddr().String(), h["x-forwarded-for"], h["x-real-ip"])

	// 2. Route by mount prefix and path; Sniff on shared paths.
	method := string(ctx.Method())
	path := string(ctx.Path())
	m, _ := st.routes.lookup(method, path, h)
	var miss driver.RouteMiss
	if m == nil {
		// A route miss: the driver that covers the path answers it when
		// it knows its provider's answer (task 235), else OpenAI's.
		c, rm, ok := st.routes.miss(method, path, h)
		if _, own := driver.As[driver.RouteMisser](c.drv); !ok || !own {
			writeNotFound(ctx, rm)
			return
		}
		m, miss = &match{drv: c.drv, kind: driver.KindRouteMiss, path: rm.Path, prefix: c.prefix}, rm
	}
	// The raw head as it arrived, only for a driver that asks for it
	// (spec 004 "Cabeça crua").
	storeRawHead(&ctx.Request.Header, h, m.drv)
	h["x-wwz-path"] = m.path
	// The origin of wwz for the URLs the driver returns (ADR 0073); always
	// overwritten, so a client never sets it.
	h[driver.OriginHeader] = origin(ctx, h, ipr.PeerTrusted, m.prefix)

	// 3. Global protection.
	if ok, retry := s.limiter.allow(st.cfg.Server.MaxRPS, time.Now()); !ok {
		s.rejectMaxRPS(ctx, eng, m.drv, retry)
		return
	}

	// 4. Engine, with a context cancelled when the client disconnects.
	rctx, cancel := context.WithCancel(s.base)
	fw := newFastWriter(ctx, cancel, s.opts.HangFor, anthropicDialect(m.kind, m.drv))
	stop := watchDisconnect(ctx.Conn(), cancel)
	fw.onClose = stop
	in := engine.Input{
		Driver:    m.drv,
		Kind:      m.kind,
		Method:    method,
		Path:      m.path,
		Headers:   h,
		Body:      append([]byte(nil), ctx.PostBody()...), // streams outlive ctx
		ClientIP:  ipr.ClientIP,
		PathParam: m.param,
		Miss:      miss,
	}
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("data plane panic", "panic", p, "path", path)
			if !fw.isStreaming() {
				ctx.Response.Reset()
				m.drv.WriteError(fw, nil, &core.Error{Kind: core.ErrServerError, Message: "internal error"}, nil)
			}
		}
		if !fw.isStreaming() {
			stop()
			cancel()
		}
	}()
	eng.Handle(rctx, in, fw)
}

// origin is <scheme>://<host><external prefix><mount prefix> of the
// request (ADR 0073, spec 004 "URLs do próprio wwz"). X-Forwarded-Proto,
// X-Forwarded-Host and X-Forwarded-Prefix count only when the TCP peer is
// a trusted proxy (the X-Forwarded-For rule, internal/clientip), each by
// its first comma-separated value. Otherwise the scheme is https over TLS
// and http without, and the host is Host. Without a host the origin is
// only the prefixes: the URLs come out relative to the root.
func origin(ctx *fasthttp.RequestCtx, h driver.MapHeaders, trusted bool, mount string) string {
	scheme := "http"
	if ctx.IsTLS() {
		scheme = "https"
	}
	host := firstValue(string(ctx.Host()))
	external := ""
	if trusted {
		if p := strings.ToLower(firstValue(h["x-forwarded-proto"])); p == "https" || p == "http" {
			scheme = p
		}
		if fh := firstValue(h["x-forwarded-host"]); fh != "" {
			host = fh
		}
		if p := firstValue(h["x-forwarded-prefix"]); strings.HasPrefix(p, "/") {
			external = strings.TrimRight(p, "/")
		}
	}
	if host == "" {
		return external + mount
	}
	return scheme + "://" + host + external + mount
}

// firstValue is the first of the comma-separated values of a header.
func firstValue(v string) string {
	v, _, _ = strings.Cut(v, ",")
	return strings.TrimSpace(v)
}

// anthropicDialect reports whether a matched route speaks the Anthropic
// Messages dialect. The route kind declared by the driver decides, not the
// request path or headers: a custom mount or route need not end in
// "/messages", and an OpenAI-shaped request may carry anthropic-version.
// Kinds without a dialect of their own fall back to the driver protocol.
func anthropicDialect(kind driver.Kind, drv driver.Driver) bool {
	switch kind {
	case driver.KindMessages, driver.KindCountTokens:
		return true
	case driver.KindChat, driver.KindCompletions, driver.KindResponses, driver.KindEmbeddings:
		return false
	}
	return drv.Protocol() == config.DialectAnthropic
}

// rejectMaxRPS answers in the driver's format when server.max_rps is hit:
// the simulated provider as a whole is over capacity.
func (s *Server) rejectMaxRPS(ctx *fasthttp.RequestCtx, eng *engine.Engine, drv driver.Driver, retry time.Duration) {
	if retry < time.Second {
		retry = time.Second
	}
	fw := newFastWriter(ctx, func() {}, 0, false)
	er := &core.Error{Kind: core.ErrOverloaded, Message: "The server is over its configured capacity (server.max_rps). Please retry later.", RetryAfter: retry}
	drv.WriteError(fw, nil, er, nil)
	ctx.Response.Header.Set("Retry-After", strconv.Itoa(int((retry+time.Second-1)/time.Second)))
	if eng.M != nil {
		eng.M.RateLimited.Inc(drv.Name(), "server_max_rps")
	}
}

// writeNotFound answers a route miss of a driver without a RouteMisser as
// OpenAI does (conferred on 2026-10-31, task 235,
// testdata/real/openai/routes/; the key is not looked at): an unknown path
// is a 404 without a body; on Chat Completions, Completions and Embeddings
// a GET is a 405 "Only POST requests are accepted." and another method a
// 404 "Invalid URL (...)", both indented with 4 spaces and a final
// newline; on the API's routes (models, Responses) a wrong method is a
// 405 "Invalid method for URL (...)" indented with 2 spaces. GET on the
// OpenAI chat route is a real route of the openai driver (task 315).
func writeNotFound(ctx *fasthttp.RequestCtx, miss driver.RouteMiss) {
	if len(miss.Allow) == 0 {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		return
	}
	switch miss.Kind {
	case driver.KindChat, driver.KindCompletions, driver.KindEmbeddings:
		status, msg, code := fasthttp.StatusNotFound, "Invalid URL ("+miss.Method+" "+miss.Path+")", "null"
		if miss.Method == "GET" {
			status, msg, code = fasthttp.StatusMethodNotAllowed, "Only POST requests are accepted.", `"method_not_supported"`
		}
		q, _ := json.Marshal(msg)
		ctx.SetStatusCode(status)
		ctx.Response.Header.Set("Content-Type", "application/json; charset=utf-8")
		ctx.Response.Header.Set("x-request-id", ids.OpenAIRequest(ids.FromToken(strconv.FormatUint(rand.Uint64(), 36))))
		ctx.Response.SetBodyString("{\n    \"error\": {\n        \"message\": " + string(q) + ",\n        \"type\": \"invalid_request_error\",\n        \"param\": null,\n        \"code\": " + code + "\n    }\n}\n")
	default:
		q, _ := json.Marshal("Invalid method for URL (" + miss.Method + " " + miss.Path + ")")
		ctx.SetStatusCode(fasthttp.StatusMethodNotAllowed)
		ctx.Response.Header.Set("Content-Type", "application/json")
		ctx.Response.Header.Set("openai-processing-ms", "1")
		ctx.Response.Header.Set("openai-version", "2020-10-01")
		rid := newUUID() // models: a UUID; Responses: req_ + 32 hex (observed)
		if miss.Kind == driver.KindResponses {
			rid = ids.OpenAIRequest(ids.FromToken(strconv.FormatUint(rand.Uint64(), 36)))
		}
		ctx.Response.Header.Set("x-request-id", rid)
		ctx.Response.SetBodyString("{\n  \"error\": {\n    \"message\": " + string(q) + ",\n    \"type\": \"invalid_request_error\",\n    \"param\": null,\n    \"code\": null\n  }\n}")
	}
}

// newUUID is a random version 4 UUID.
func newUUID() string {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[:8], rand.Uint64())
	binary.LittleEndian.PutUint64(b[8:], rand.Uint64())
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// readHeaders copies the request headers into a map with lower-cased names;
// repeated headers are joined with commas in arrival order.
func readHeaders(rh *fasthttp.RequestHeader) driver.MapHeaders {
	h := make(driver.MapHeaders, 16)
	rh.VisitAll(func(k, v []byte) {
		name := strings.ToLower(string(k))
		if prev, ok := h[name]; ok {
			h[name] = prev + "," + string(v)
			return
		}
		h[name] = string(v)
	})
	return h
}

// fastLogger routes fasthttp's own messages to slog.
type fastLogger struct{ l *slog.Logger }

func (f fastLogger) Printf(format string, args ...any) {
	f.l.Warn(fmt.Sprintf(format, args...), "component", "fasthttp")
}
