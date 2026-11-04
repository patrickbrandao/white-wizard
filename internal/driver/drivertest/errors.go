package drivertest

import (
	"encoding/base64"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// AllErrorKinds lists every core.ErrorKind; WriteError must render each.
var AllErrorKinds = []core.ErrorKind{
	core.ErrInvalidRequest, core.ErrContextLength, core.ErrInvalidAPIKey, core.ErrPermission,
	core.ErrModelNotFound, core.ErrRequestTooLarge, core.ErrRateLimit, core.ErrQuota,
	core.ErrSpendLimit, core.ErrServerError, core.ErrBadGateway, core.ErrOverloaded,
	core.ErrGatewayTimeout, core.ErrContentFilter, core.ErrSlowDown, core.ErrGeoBlocked,
	core.ErrAccountSuspended, core.ErrAccountBanned, core.ErrNotFound, core.ErrMissingSession,
	core.ErrBudget, core.ErrMethodNotAllowed,
}

// testRL is the limiter state passed where a test wants rate-limit headers.
func testRL() *core.RateLimitState {
	return &core.RateLimitState{
		Present:       true,
		RequestsLimit: 100, RequestsRemaining: 99, RequestsReset: 600 * time.Millisecond,
		TokensLimit: 10000, TokensRemaining: 9500, TokensReset: 6 * time.Minute,
		InputTokensLimit: 8000, InputTokensRemain: 7600,
		OutputTokensLimit: 2000, OutputTokensRem: 1500,
	}
}

func cloneErr(e core.Error) *core.Error {
	c := e
	if e.Extra != nil {
		c.Extra = make(map[string]any, len(e.Extra))
		for k, v := range e.Extra {
			c.Extra[k] = v
		}
	}
	return &c
}

func (s *suite) errReq(k driver.Kind) *core.Request {
	return &core.Request{Driver: s.d.Name(), Endpoint: endpointOf(k), Model: "gpt-x", RequestID: s.requestID(k, "err-1")}
}

func (s *suite) testErrors(t *testing.T) {
	for _, k := range s.surfaces() {
		sf := s.surface(k)
		s.run(t, string(k), func(t *testing.T) {
			s.run(t, "AllKinds", func(t *testing.T) {
				for _, kind := range AllErrorKinds {
					s.run(t, string(kind), func(t *testing.T) {
						req := s.errReq(k)
						e := &core.Error{Kind: kind}
						w := core.NewMemWriter()
						s.d.WriteError(w, req, e, nil)
						st := w.StatusCode()
						if st < 400 || st > 599 {
							t.Fatalf("status %d, want 4xx/5xx", st)
						}
						checkContentLength(t, w)
						s.checkEnvelope(t, k, w, req, e)
					})
				}
			})
			// Real providers never send an empty message: an engine error
			// without one gets the dialect's default text.
			s.run(t, "DefaultMessage", func(t *testing.T) {
				var empty []string
				for _, kind := range AllErrorKinds {
					w := core.NewMemWriter()
					s.d.WriteError(w, s.errReq(k), &core.Error{Kind: kind}, nil)
					if ct, _ := header(w.Headers(), "Content-Type"); !strings.HasPrefix(ct, "application/json") {
						continue
					}
					body := decodeJSON(t, w.Body())
					if str(body, "error.message") == "" && str(body, "message") == "" {
						empty = append(empty, string(kind))
					}
				}
				if len(empty) > 0 {
					t.Errorf("empty error message for %v", empty)
				}
			})
			s.run(t, "StatusOverride", func(t *testing.T) {
				w := core.NewMemWriter()
				s.d.WriteError(w, s.errReq(k), &core.Error{Kind: core.ErrServerError, Status: 418, Message: "teapot"}, nil)
				if w.StatusCode() != 418 {
					t.Errorf("status %d, want the Error.Status override 418", w.StatusCode())
				}
			})
			s.run(t, "Blocked", func(t *testing.T) { s.blockedCase(t, k) })
			s.run(t, "Cases", func(t *testing.T) {
				for _, c := range sf.Errors {
					s.run(t, c.Name, func(t *testing.T) { s.errorCase(t, k, c) })
				}
			})
			if len(s.p.Billing) > 0 {
				s.run(t, "Billing", func(t *testing.T) {
					for _, c := range s.p.Billing {
						s.run(t, c.Name, func(t *testing.T) { s.billingCase(t, k, c) })
					}
				})
			}
			s.run(t, "RetryAfter", func(t *testing.T) {
				for _, c := range sf.RetryAfter {
					s.run(t, c.Name, func(t *testing.T) {
						w := core.NewMemWriter()
						s.d.WriteError(w, s.errReq(k), cloneErr(c.Err), nil)
						got, ok := header(w.Headers(), "retry-after")
						if !ok || got != c.Want {
							t.Errorf("retry-after = %q (present %v), want %q", got, ok, c.Want)
						}
					})
				}
			})
		})
	}
	if s.strict {
		s.run(t, "PublicRoutes", s.testPublicRoutes)
	}
}

// testPublicRoutes: the routes of Profile.PublicRoutes answer without a
// credential (driver.PublicRouter), and no other route does (spec 006 names
// it PublicRoute; the engine skips the key check only there).
func (s *suite) testPublicRoutes(t *testing.T) {
	public := map[driver.Route]bool{}
	for _, r := range s.p.PublicRoutes {
		public[r] = true
		if !driver.IsPublicRoute(s.d, r) {
			t.Errorf("route %s %s (%s) must answer without a credential", r.Method, r.Path, r.Kind)
		}
	}
	for _, r := range s.routes() {
		if !public[r] && driver.IsPublicRoute(s.d, r) {
			t.Errorf("route %s %s (%s) answers without a credential; the profile lists it nowhere", r.Method, r.Path, r.Kind)
		}
	}
}

// billingCase asks the driver's BillingDecider the case on the surface's
// route under c.Base and checks the source, or the refusal as WriteError
// renders it on that surface (ADR 0072).
func (s *suite) billingCase(t *testing.T, k driver.Kind, c BillingCase) {
	bd, ok := driver.As[driver.BillingDecider](s.d)
	if !ok {
		t.Fatal("the profile has billing cases; the driver does not implement driver.BillingDecider (interface (d) of spec 005)")
	}
	var route driver.Route
	for _, r := range s.routes() {
		if r.Kind == k && strings.HasPrefix(r.Path, c.Base+"/") {
			route = r
			break
		}
	}
	if route.Path == "" {
		t.Fatalf("no %s route under %s", k, c.Base)
	}
	h := driver.MapHeaders{}
	for name, v := range c.Headers {
		h[strings.ToLower(name)] = v
	}
	cred := s.d.Authenticate(h)
	cat := driver.RouteCatalog(s.d, route)
	driver.SetCatalog(h, cat)
	acct := driver.Account{Key: cred.Key}
	if c.Key != nil {
		acct.Account, acct.Project = c.Key.Account, c.Key.Project
	}
	driver.SetAccount(h, acct)
	m := c.Model
	req := &core.Request{Driver: s.d.Name(), Endpoint: endpointOf(k), Model: m.ID, RequestID: s.requestID(k, "bill-1"), Headers: h}
	q := driver.BillingQuery{Route: route, Catalog: cat, Request: req, Credential: cred, Key: c.Key, Account: c.Account, Plan: c.Plan, Model: &m}
	if c.Key != nil {
		q.AccountID = c.Key.Account
	}
	b, e := bd.Billing(q)
	if c.Status == 0 {
		if e != nil {
			t.Fatalf("refused (%s %d %q); want the billing %+v", e.Kind, e.Status, e.Message, c.Want)
		}
		if b != c.Want {
			t.Errorf("billing %+v, want %+v", b, c.Want)
		}
		return
	}
	if e == nil {
		t.Fatalf("accepted as %+v; want the refusal %d", b, c.Status)
	}
	w := core.NewMemWriter()
	s.d.WriteError(w, req, e, nil)
	if w.StatusCode() != c.Status {
		t.Errorf("status %d, want %d; body %s", w.StatusCode(), c.Status, w.Body())
	}
	checkContentLength(t, w)
	s.checkEnvelope(t, k, w, req, e)
	if want, ok := c.Bodies[k]; ok && !jsonEq(string(w.Body()), want) {
		t.Errorf("body %s, want %s", w.Body(), want)
	}
	if c.RetryAfterMidnight {
		expectRetryAfterMidnight(t, w.Headers())
	}
}

// expectRetryAfterMidnight: retry-after is the whole seconds from now to
// the next 00:00 UTC (a second of slack each way).
func expectRetryAfterMidnight(t *testing.T, h map[string]string) {
	t.Helper()
	v, _ := header(h, "retry-after")
	n, err := strconv.Atoi(v)
	now := time.Now().UTC()
	want := int(math.Ceil(time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC).Sub(now).Seconds()))
	if err != nil || n < want-2 || n > want+1 {
		t.Errorf("retry-after %q, want ~%d (the seconds to 00:00 UTC)", v, want)
	}
}

// expectNextMonth: the RFC 3339 instant at p is the 1st of the next month,
// 00:00 UTC.
func expectNextMonth(t *testing.T, body any, p string) {
	t.Helper()
	got, err := time.Parse(time.RFC3339, str(body, p))
	now := time.Now().UTC()
	want := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	// Around midnight of the last day the month can turn between the
	// driver's clock and this one.
	later := time.Date(now.Year(), now.Month()+2, 1, 0, 0, 0, 0, time.UTC)
	if err != nil || !got.Equal(want) && (!got.Equal(later) || !now.Add(time.Minute).After(want)) {
		t.Errorf("%s = %q, want %s (the 1st of the next month, 00:00 UTC)", p, str(body, p), want.Format(time.RFC3339))
	}
}

// caseReq is the request of an error case: errReq with the catalog, the
// account and the headers the case gives.
func (s *suite) caseReq(k driver.Kind, c ErrorCase) *core.Request {
	req := s.errReq(k)
	if c.Catalog == "" && c.Account == (driver.Account{}) && len(c.ReqHeaders) == 0 {
		return req
	}
	h := driver.MapHeaders{}
	for name, v := range c.ReqHeaders {
		h[strings.ToLower(name)] = v
	}
	driver.SetCatalog(h, c.Catalog)
	if c.Account != (driver.Account{}) {
		driver.SetAccount(h, c.Account)
	}
	req.Headers = h
	return req
}

func (s *suite) errorCase(t *testing.T, k driver.Kind, c ErrorCase) {
	req := s.caseReq(k, c)
	e := cloneErr(c.Err)
	var rl *core.RateLimitState
	if c.WithRateLimit {
		rl = testRL()
	}
	w := core.NewMemWriter()
	s.d.WriteError(w, req, e, rl)
	if w.StatusCode() != c.Status {
		t.Errorf("status %d, want %d; body %s", w.StatusCode(), c.Status, w.Body())
	}
	checkContentLength(t, w)
	body := s.checkEnvelope(t, k, w, req, e)
	for p, want := range c.Fields {
		expectPath(t, body, p, want)
	}
	if c.Body != "" && !jsonEq(string(w.Body()), c.Body) {
		t.Errorf("body %s, want %s", w.Body(), c.Body)
	}
	for _, p := range c.Absent {
		expectAbsent(t, body, p)
	}
	for p, re := range c.Patterns {
		if v := str(body, p); !regexp.MustCompile(re).MatchString(v) {
			t.Errorf("%s = %q, want /%s/", p, v, re)
		}
	}
	for _, p := range c.NextMonth {
		expectNextMonth(t, body, p)
	}
	h := w.Headers()
	if c.RetryAfterMidnight {
		expectRetryAfterMidnight(t, h)
	}
	for name, want := range c.Headers {
		expectHeader(t, h, name, want)
	}
	for _, name := range c.NoHeaders {
		if v, ok := header(h, name); ok {
			t.Errorf("header %s = %q, want absent", name, v)
		}
	}
	if v, ok := header(h, "retry-after"); ok {
		if n, err := strconv.Atoi(v); err != nil || n <= 0 {
			t.Errorf("retry-after %q must be a positive integer of seconds", v)
		}
	}
	if rl != nil {
		s.checkRateLimitHeaders(t, k, h)
	}
}

// blockedCase covers a blocked entity (spec 012, openapi.yaml, task 041): the
// engine hands WriteError an account_suspended carrying the block's remaining
// time in RetryAfter, and every driver must answer 403 without retry-after or
// retry-after-ms, as real providers do, so SDKs do not retry. The end of the
// block is visible only through the admin API and the panel.
func (s *suite) blockedCase(t *testing.T, k driver.Kind) {
	for _, withRL := range []bool{false, true} {
		name := "no_ratelimit_state"
		var rl *core.RateLimitState
		if withRL {
			name, rl = "with_ratelimit_state", testRL()
		}
		s.run(t, name, func(t *testing.T) {
			// Same shape as engine.denialError for a block decision.
			e := &core.Error{Kind: core.ErrAccountSuspended, RetryAfter: time.Hour, Message: "blocked by wwz", Extra: map[string]any{"limit": "block"}}
			req := s.errReq(k)
			w := core.NewMemWriter()
			s.d.WriteError(w, req, e, rl)
			if w.StatusCode() != 403 {
				t.Errorf("status %d, want 403; body %s", w.StatusCode(), w.Body())
			}
			checkContentLength(t, w)
			s.checkEnvelope(t, k, w, req, e)
			for _, h := range []string{"retry-after", "retry-after-ms"} {
				if v, ok := header(w.Headers(), h); ok {
					t.Errorf("header %s = %q, want absent on a blocked entity", h, v)
				}
			}
		})
	}
}

// checkEnvelope validates the error body shape of the surface and returns it
// decoded (nil for the HTML 502 page).
func (s *suite) checkEnvelope(t *testing.T, k driver.Kind, w *core.MemWriter, req *core.Request, e *core.Error) map[string]any {
	t.Helper()
	h := w.Headers()
	ct, _ := header(h, "Content-Type")
	if strings.HasPrefix(ct, "text/html") {
		// Cloudflare's 502 page, as the real providers serve it.
		if e.Kind != core.ErrBadGateway {
			t.Errorf("HTML body for %s; only bad_gateway may be HTML", e.Kind)
		}
		return nil
	}
	sf := s.surface(k)
	if sf.AuthErrorText && e.Kind == core.ErrInvalidAPIKey && w.StatusCode() == 401 {
		// OpenAI's key check: text/plain, status in the body, the body
		// again in x-error-json (spec 006 16.7).
		if ct != "text/plain" {
			t.Errorf("Content-Type %q, want text/plain", ct)
		}
		if x, _ := header(h, "x-error-json"); x != base64.StdEncoding.EncodeToString(w.Body()) {
			t.Errorf("x-error-json %q is not the base64 of the body %q", x, w.Body())
		}
		expectHeader(t, h, "x-openai-authorization-error", "401")
		expectHeader(t, h, "x-openai-ide-error-code", "invalid_api_key")
		expectPath(t, decodeJSON(t, w.Body()), "status", 401)
	} else if !strings.HasPrefix(ct, "application/json") && (sf.Envelope != EnvAny || ct != "text/plain") {
		// A neutral profile accepts JSON sent as text/plain (OpenAI's 401).
		t.Fatalf("Content-Type %q, want application/json", ct)
	}
	body := decodeJSON(t, w.Body())
	switch sf.Envelope {
	case EnvOpenAI:
		for _, p := range []string{"error.message", "error.type"} {
			if _, ok := mustPath(body, p).(string); !ok {
				t.Errorf("%s must be a string: %v", p, body)
			}
		}
		expectPresent(t, body, "error.param")
		expectPresent(t, body, "error.code")
	case EnvAnthropic:
		s.checkAnthropicEnvelope(t, sf, body, h, req)
	case EnvAzure:
		checkAzureEnvelope(t, body, w.StatusCode(), e)
	case EnvOpenCode:
		if _, ok := body["model"].(string); ok && len(body) == 1 && w.StatusCode() == 400 {
			break // a Messages body without max_tokens: {"model": ...} (conferred)
		}
		if str(body, "error.type") == "" {
			t.Errorf("error.type must be a non-empty string: %v", body)
		}
		if _, ok := mustPath(body, "error.message").(string); !ok {
			t.Errorf("error.message must be a string: %v", body)
		}
		if m, ok := body["metadata"]; ok {
			if _, isObj := m.(map[string]any); !isObj {
				t.Errorf("metadata must be an object: %v", body)
			}
		}
		if _, ok := body["request_id"]; ok {
			t.Errorf("OpenCode sends no request_id: %v", body)
		}
		// Upstream failures on Chat Completions have no top-level type; the
		// gateway's own errors and the Messages route have type "error".
		if v, ok := body["type"]; ok && v != "error" {
			t.Errorf("type = %v, want error: %v", v, body)
		}
		if _, ok := body["type"]; !ok && (k == driver.KindMessages || len(body) != 1) {
			t.Errorf("body is not the OpenCode envelope: %v", body)
		}
	case EnvOpenRouter:
		expectPath(t, body, "error.code", w.StatusCode())
		if _, ok := mustPath(body, "error.message").(string); !ok {
			t.Errorf("error.message must be a string: %v", body)
		}
		if _, ok := path(body, "error.metadata"); !ok {
			// The gateway's own envelope (invalid model ID, validation):
			// {error: {message, code}, user_id} and nothing else; the 401
			// has no user_id either (spec 006 16.7).
			e, _ := body["error"].(map[string]any)
			if w.StatusCode() == 401 {
				if len(body) != 1 || len(e) != 2 {
					t.Errorf("the 401 must be {error: {message, code}}: %v", body)
				}
				break
			}
			if len(body) != 2 || len(e) != 2 || str(body, "user_id") == "" {
				t.Errorf("without error.metadata the body must be the gateway envelope: %v", body)
			}
			break
		}
		if str(body, "error.metadata.error_type") == "" {
			t.Errorf("error.metadata.error_type must be set: %v", body)
		}
	}
	return body
}

func (s *suite) checkAnthropicEnvelope(t *testing.T, sf Surface, body map[string]any, h map[string]string, req *core.Request) {
	t.Helper()
	expectPath(t, body, "type", "error")
	for _, p := range []string{"error.type", "error.message"} {
		if _, ok := mustPath(body, p).(string); !ok {
			t.Errorf("%s must be a string: %v", p, body)
		}
	}
	rid := str(body, "request_id")
	hv, has := header(h, "request-id")
	if sf.AuthRequestIDNull && str(body, "error.type") == "authentication_error" {
		expectPath(t, body, "request_id", nil)
		if has {
			t.Errorf("request-id header %q on a 401 whose request_id is null", hv)
		}
		return
	}
	switch {
	case sf.RequestIDFormat != nil && !sf.RequestIDFormat.MatchString(rid):
		t.Errorf("request_id %q does not match %s", rid, sf.RequestIDFormat)
	case sf.RequestIDFormat == nil && !strings.HasPrefix(rid, "req_"):
		t.Errorf("request_id %q, want req_…", rid)
	}
	// The header goes only where the real sends it (driver.Conventions):
	// Anthropic yes, the gateways no (spec 006 16.7), except OpenRouter's
	// Messages route, whose request-id is the generation id.
	switch {
	case sf.RequestIDHeader && hv != rid:
		t.Errorf("request-id header %q != body request_id %q", hv, rid)
	case !sf.RequestIDHeader && has:
		t.Errorf("request-id header %q on a surface whose real provider sends none", hv)
	}
	minted := req != nil && (strings.HasPrefix(req.RequestID, "req_") || sf.RequestIDFormat != nil && sf.RequestIDFormat.MatchString(req.RequestID))
	if minted && rid != req.RequestID {
		t.Errorf("request_id %q, want the minted %q", rid, req.RequestID)
	}
}
