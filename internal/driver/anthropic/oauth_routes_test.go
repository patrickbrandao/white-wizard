package anthropic_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

// oauthConfig mounts the anthropic driver at the root with the
// subscription of the key "assinante" (ADR 0074, spec 006 16.2.1): its
// account has a quota_usd of $5 in 5 h and $30 a week, and every answer
// costs $1 (the 20 output tokens Claude counts for the 8 generated, at $50 000 per million). The models are the
// ones the Claude Code 2.1.308 fixtures name.
const oauthConfig = `
server: {listen: "127.0.0.1:0", auth: keys, seed: 7}
store: {kind: memory}
logging: {level: error}
drivers: [{name: anthropic, options: {oauth_key: assinante, oauth_plan: max_20x}}]
accounts: [{id: acct-sub, limits: {quota_usd: {monthly_usd: 100, rolling_5h_pct: 5, weekly_pct: 30}}}]
keys:
  - {key: wwz-oauth-secret-0001, label: assinante, account: acct-sub}
  - {key: wwz-key-api, label: api}
profiles:
  latency: {zero: {}}
  generation:
    text: {mode: lorem, output_tokens: {dist: fixed, value: 8}}
models:
  - {id: claude-sonnet-5-5, latency: zero, generation: text, pricing: {input_per_m: 0, output_per_m: 50000}}
  - {id: claude-opus-5-5, latency: zero, generation: text, pricing: {input_per_m: 0, output_per_m: 50000}}
  - {id: claude-haiku-5-5, latency: zero, generation: text, pricing: {input_per_m: 0, output_per_m: 50000}}
`

// wwzClock is the engine's clock of a test server, moved by the test.
type wwzClock struct{ ns atomic.Int64 }

func (c *wwzClock) now() time.Time             { return time.Unix(0, c.ns.Load()) }
func (c *wwzClock) add(d time.Duration)        { c.ns.Add(int64(d)) }
func newClock(at time.Time) *wwzClock          { c := &wwzClock{}; c.ns.Store(at.UnixNano()); return c }
func (c *wwzClock) set(at time.Time) *wwzClock { c.ns.Store(at.UnixNano()); return c }

// startOAuth serves cfg with the engine on clock and returns its base URL.
func startOAuth(t *testing.T, cfg string, clock *wwzClock) string {
	t.Helper()
	c, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := engine.NewMetrics(metrics.New(c.Metrics.MaxSeries), c.Metrics.Buckets)
	e := engine.New(c, limits.NewMemory(), scenario.NewRegistry(), m, log)
	e.Now = clock.now
	srv, err := server.New(e, server.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	return "http://" + ln.Addr().String()
}

// noRedirect is a client that shows the authorize redirect.
var noRedirect = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type answer struct {
	status int
	header http.Header
	body   string
}

// send does one request; hdr are its headers, body a JSON (or "").
func send(t *testing.T, method, u string, hdr map[string]string, body string) answer {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	r, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{r.StatusCode, r.Header, string(b)}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// pkce is a code_verifier (32 random bytes, as Claude Code draws it) and
// its S256 challenge.
func pkce(t *testing.T) (verifier, challenge string) {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	verifier = b64(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, b64(sum[:])
}

// claudeScopes is the scope of Claude Code 2.1.308's claude.ai login.
const claudeScopes = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins"

// authorizeURL is Claude Code's authorize URL, its parameters in the
// client's order.
func authorizeURL(base, path, redirect, scope, challenge, state string) string {
	q := []string{
		"code=true", "client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e", "response_type=code",
		"redirect_uri=" + url.QueryEscape(redirect), "scope=" + url.QueryEscape(scope),
		"code_challenge=" + challenge, "code_challenge_method=S256", "state=" + state,
	}
	return base + path + "?" + strings.Join(q, "&")
}

// tokens is the token route's 200.
type tokens struct {
	TokenType      string `json:"token_type"`
	AccessToken    string `json:"access_token"`
	ExpiresIn      int    `json:"expires_in"`
	RefreshToken   string `json:"refresh_token"`
	RefreshExpires int    `json:"refresh_token_expires_in"`
	Scope          string `json:"scope"`
	Organization   struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
	Account struct {
		UUID  string `json:"uuid"`
		Email string `json:"email_address"`
	} `json:"account"`
}

func decode(t *testing.T, a answer, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(a.body), v); err != nil {
		t.Fatalf("%d %s: %v", a.status, a.body, err)
	}
}

// login runs Claude Code's login against wwz at base: authorize with PKCE
// S256 on the local callback, then the authorization code grant.
func login(t *testing.T, base, scope string) tokens {
	t.Helper()
	verifier, challenge := pkce(t)
	const redirect = "http://localhost:54545/callback"
	a := send(t, "GET", authorizeURL(base, "/cai/oauth/authorize", redirect, scope, challenge, "st4te"), nil, "")
	if a.status != 302 {
		t.Fatalf("authorize: %d %s", a.status, a.body)
	}
	loc, err := url.Parse(a.header.Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), redirect+"?code=") || loc.Query().Get("state") != "st4te" {
		t.Fatalf("authorize location %q", a.header.Get("Location"))
	}
	body, _ := json.Marshal(map[string]any{
		"grant_type": "authorization_code", "code": loc.Query().Get("code"), "redirect_uri": redirect,
		"client_id": "9d1c250a-e61b-44d9-88ed-5944d1962f5e", "code_verifier": verifier, "state": "st4te",
	})
	a = send(t, "POST", base+"/v1/oauth/token", map[string]string{"Content-Type": "application/json"}, string(body))
	if a.status != 200 {
		t.Fatalf("token: %d %s", a.status, a.body)
	}
	var tk tokens
	decode(t, a, &tk)
	return tk
}

// messages is a Messages request with the OAuth token as Claude Code
// sends it; beta "" leaves anthropic-beta out.
func messages(t *testing.T, base, token, beta string) answer {
	t.Helper()
	hdr := map[string]string{
		"Authorization": "Bearer " + token, "anthropic-version": "2023-06-01", "Content-Type": "application/json",
		"User-Agent": "claude-cli/2.1.308 (external, cli)", "x-app": "cli",
	}
	if beta != "" {
		hdr["anthropic-beta"] = beta
	}
	return send(t, "POST", base+"/v1/messages", hdr, `{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
}

const ccBetas = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14"

var reProfileReqID = regexp.MustCompile(`^req_[A-Za-z0-9_-]{1,36}$`)

func apiError(t *testing.T, a answer) (typ, msg string) {
	t.Helper()
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	decode(t, a, &env)
	if env.Type != "error" || env.RequestID == "" || env.RequestID != a.header.Get("request-id") {
		t.Errorf("not the API envelope with request-id: %v %s", a.header, a.body)
	}
	return env.Error.Type, env.Error.Message
}

// TestOAuthLoginEndToEnd: the whole subscription login of Claude Code
// against wwz (ADR 0074, spec 006 16.2.1): authorize with PKCE S256 → code
// → POST /v1/oauth/token → GET /api/oauth/profile with a request-id the
// client trusts → POST /v1/messages with the access token and the beta →
// 200 with the anthropic-ratelimit-unified-* growing with the spend →
// refresh → the old token, expired, is the 401 of expiry; a tampered one
// the invalid token's, one without the beta the beta's.
func TestOAuthLoginEndToEnd(t *testing.T) {
	start := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC) // a Wednesday
	clock := newClock(start)
	base := startOAuth(t, oauthConfig, clock)

	tk := login(t, base, claudeScopes)
	if tk.TokenType != "Bearer" || !strings.HasPrefix(tk.AccessToken, "sk-ant-oat01-") || !strings.HasPrefix(tk.RefreshToken, "sk-ant-ort01-") ||
		tk.ExpiresIn != 28800 || tk.RefreshExpires != 2592000 || tk.Scope != claudeScopes ||
		tk.Account.Email != "assinante@wwz.invalid" || tk.Organization.Name != "acct-sub" {
		t.Fatalf("tokens %+v", tk)
	}
	if !regexp.MustCompile(`^sk-ant-oat01-[\w-]{20,}$`).MatchString(tk.AccessToken) {
		t.Errorf("the client's secret pattern does not match %q", tk.AccessToken)
	}

	// The profile, as Claude Code reads it.
	a := send(t, "GET", base+"/api/oauth/profile", map[string]string{
		"Authorization": "Bearer " + tk.AccessToken, "User-Agent": "claude-code/2.1.308", "Content-Type": "application/json", "Cache-Control": "no-cache",
	}, "")
	if a.status != 200 || !reProfileReqID.MatchString(a.header.Get("request-id")) {
		t.Fatalf("profile: %d request-id %q %s", a.status, a.header.Get("request-id"), a.body)
	}
	var prof struct {
		Account struct {
			UUID  string `json:"uuid"`
			Email string `json:"email"`
		} `json:"account"`
		Organization struct {
			UUID          string `json:"uuid"`
			Type          string `json:"organization_type"`
			RateLimitTier string `json:"rate_limit_tier"`
		} `json:"organization"`
	}
	decode(t, a, &prof)
	if prof.Account.UUID != tk.Account.UUID || prof.Organization.UUID != tk.Organization.UUID || prof.Account.Email != tk.Account.Email ||
		prof.Organization.Type != "claude_max" || prof.Organization.RateLimitTier != "default_claude_max_20x" {
		t.Fatalf("profile %+v (tokens %+v)", prof, tk)
	}

	// Inference with the access token: 200 with the unified headers,
	// whose 5 h use follows the account's quota ($1 of $5 per answer).
	for i, want := range []string{"0", "0.2", "0.4"} {
		a = messages(t, base, tk.AccessToken, ccBetas)
		if a.status != 200 || !strings.Contains(a.body, `"type":"message"`) {
			t.Fatalf("messages %d: %d %s", i, a.status, a.body)
		}
		if got := a.header.Get("anthropic-ratelimit-unified-5h-utilization"); got != want {
			t.Errorf("messages %d: 5h-utilization %q, want %q", i, got, want)
		}
		if a.header.Get("anthropic-ratelimit-unified-status") != "allowed" || a.header.Get("anthropic-ratelimit-requests-limit") != "" ||
			a.header.Get("anthropic-ratelimit-unified-representative-claim") != "five_hour" || a.header.Get("anthropic-organization-id") == "" {
			t.Errorf("messages %d headers %v", i, a.header)
		}
	}
	if got := a.header.Get("anthropic-ratelimit-unified-7d-reset"); got != strconv.FormatInt(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC).Unix(), 10) {
		t.Errorf("7d-reset %q, want next Monday", got)
	}

	// The refresh, as Claude Code asks it.
	body, _ := json.Marshal(map[string]any{
		"grant_type": "refresh_token", "refresh_token": tk.RefreshToken, "client_id": "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		"scope": "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins",
	})
	clock.add(9 * time.Hour) // the first access token is past its 8 h
	a = send(t, "POST", base+"/v1/oauth/token", map[string]string{"Content-Type": "application/json"}, string(body))
	var fresh tokens
	decode(t, a, &fresh)
	if a.status != 200 || fresh.AccessToken == tk.AccessToken || fresh.Scope != "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins" {
		t.Fatalf("refresh: %d %s", a.status, a.body)
	}
	if a = messages(t, base, fresh.AccessToken, ccBetas); a.status != 200 {
		t.Fatalf("messages after refresh: %d %s", a.status, a.body)
	}

	// The refusals: expired (the old token), tampered, without the beta.
	tampered := []byte(fresh.AccessToken)
	tampered[len(tampered)-3] ^= 1
	for _, c := range []struct {
		name, token, beta, typ, msg string
		status                      int
	}{
		{"expired", tk.AccessToken, ccBetas, "authentication_error", "OAuth token has expired. Please obtain a new token or refresh your existing token.", 401},
		{"tampered", string(tampered), ccBetas, "authentication_error", "OAuth access token is invalid.", 401},
		{"without the beta", fresh.AccessToken, "claude-code-20250219", "authentication_error", "OAuth authentication is currently not supported.", 401},
		{"refresh token as access", fresh.RefreshToken, ccBetas, "authentication_error", "OAuth access token is invalid.", 401},
	} {
		a = messages(t, base, c.token, c.beta)
		typ, msg := apiError(t, a)
		if a.status != c.status || typ != c.typ || msg != c.msg {
			t.Errorf("%s: %d %s %q", c.name, a.status, typ, msg)
		}
		if a.header.Get("x-should-retry") != "false" || a.header.Get("cache-control") != "no-store" {
			t.Errorf("%s: headers %v", c.name, a.header)
		}
	}
	if !regexp.MustCompile(`^OAuth (?:access )?token has expired\b`).MatchString("OAuth token has expired. Please obtain a new token or refresh your existing token.") {
		t.Error("the client would not refresh on the expiry text")
	}
}

// TestOAuthManualCode: the manual redirect shows code#state (200
// text/plain) and the code it carries is exchanged on that redirect only;
// claude setup-token's expires_in sets the access token's life and the
// inference-only scope is refused the profile.
func TestOAuthManualCode(t *testing.T) {
	base := startOAuth(t, oauthConfig, newClock(time.Now()))
	verifier, challenge := pkce(t)
	const manual = "https://platform.claude.com/oauth/code/callback"
	a := send(t, "GET", authorizeURL(base, "/oauth/authorize", manual, "user:inference", challenge, "s1"), nil, "")
	code, state, ok := strings.Cut(a.body, "#")
	if a.status != 200 || !strings.HasPrefix(a.header.Get("Content-Type"), "text/plain") || !ok || state != "s1" {
		t.Fatalf("manual authorize: %d %q %s", a.status, a.header.Get("Content-Type"), a.body)
	}
	exchange := func(redirect, verifier string) answer {
		body, _ := json.Marshal(map[string]any{
			"grant_type": "authorization_code", "code": code, "redirect_uri": redirect,
			"client_id": "9d1c250a-e61b-44d9-88ed-5944d1962f5e", "code_verifier": verifier, "state": "s1", "expires_in": 31536000,
		})
		return send(t, "POST", base+"/v1/oauth/token", map[string]string{"Content-Type": "application/json"}, string(body))
	}
	for _, c := range []struct{ name, redirect, verifier string }{
		{"other redirect", "http://localhost:1/callback", verifier},
		{"other verifier", manual, verifier + "x"},
	} {
		if a := exchange(c.redirect, c.verifier); a.status != 400 || a.body != `{"error":"invalid_grant","error_description":"Invalid 'code' in request."}` {
			t.Errorf("%s: %d %s", c.name, a.status, a.body)
		}
	}
	a = exchange(manual, verifier)
	var tk tokens
	decode(t, a, &tk)
	if a.status != 200 || tk.ExpiresIn != 31536000 || tk.Scope != "user:inference" {
		t.Fatalf("setup-token exchange: %d %s", a.status, a.body)
	}
	if a := messages(t, base, tk.AccessToken, ccBetas); a.status != 200 {
		t.Errorf("inference-only token: %d %s", a.status, a.body)
	}
	a = send(t, "GET", base+"/api/oauth/profile", map[string]string{"Authorization": "Bearer " + tk.AccessToken}, "")
	if typ, msg := apiError(t, a); a.status != 403 || typ != "permission_error" || msg != "OAuth token does not meet scope requirement user:profile" {
		t.Errorf("profile without user:profile: %d %s %q", a.status, typ, msg)
	}
}

// TestOAuthQuotaRejected: past the 5 h window of the account's quota the
// answer is the subscription's 429: rate_limit_error, retry-after and
// status rejected with the claim, which Claude Code shows as the session
// limit without retrying.
func TestOAuthQuotaRejected(t *testing.T) {
	base := startOAuth(t, oauthConfig, newClock(time.Now()))
	tk := login(t, base, claudeScopes)
	for i := range 5 {
		if a := messages(t, base, tk.AccessToken, ccBetas); a.status != 200 {
			t.Fatalf("messages %d: %d %s", i, a.status, a.body)
		}
	}
	a := messages(t, base, tk.AccessToken, ccBetas)
	typ, msg := apiError(t, a)
	if a.status != 429 || typ != "rate_limit_error" || msg != "This request would exceed your account's rate limit. Please try again later." {
		t.Fatalf("over the quota: %d %s %q", a.status, typ, msg)
	}
	retry, _ := strconv.Atoi(a.header.Get("retry-after"))
	if a.header.Get("anthropic-ratelimit-unified-status") != "rejected" || a.header.Get("anthropic-ratelimit-unified-representative-claim") != "five_hour" ||
		a.header.Get("anthropic-ratelimit-unified-5h-utilization") != "1" || retry < 1 || retry > 5*3600 {
		t.Errorf("429 headers %v", a.header)
	}
	// /api/oauth/usage reports the same window, in percent.
	a = send(t, "GET", base+"/api/oauth/usage", map[string]string{"Authorization": "Bearer " + tk.AccessToken}, "")
	if a.status != 200 || !strings.HasPrefix(a.body, `{"five_hour":{"utilization":100,"resets_at":"`) ||
		!strings.Contains(a.body, `"seven_day":{"utilization":16.7,`) ||
		!strings.HasSuffix(a.body, `"seven_day_oauth_apps":null,"seven_day_opus":null,"seven_day_sonnet":null,"extra_usage":{"is_enabled":false,"monthly_limit":null,"used_credits":null,"utilization":null}}`) {
		t.Errorf("usage: %d %s", a.status, a.body)
	}
}

// TestOAuthAuthorizeRefusals: what the authorize route refuses (400
// text/plain with the reason, not conferred) and the account on hold
// (access_denied on the local callback, 400 on the manual one).
func TestOAuthAuthorizeRefusals(t *testing.T) {
	base := startOAuth(t, oauthConfig, newClock(time.Now()))
	_, ch := pkce(t)
	const local = "http://127.0.0.1:8123/callback"
	good := authorizeURL(base, "/oauth/authorize", local, claudeScopes, ch, "s")
	for _, c := range []struct{ name, url, want string }{
		{"client", strings.Replace(good, "client_id=9d1c", "client_id=0d1c", 1), "Invalid client_id"},
		{"response type", strings.Replace(good, "response_type=code", "response_type=token", 1), "Unsupported response_type"},
		{"no challenge", strings.Replace(good, "code_challenge="+ch, "code_challenge=", 1), "Missing code_challenge"},
		{"plain", strings.Replace(good, "code_challenge_method=S256", "code_challenge_method=plain", 1), "Unsupported code_challenge_method"},
		{"https", authorizeURL(base, "/oauth/authorize", "https://localhost:8123/callback", claudeScopes, ch, "s"), "Invalid redirect_uri"},
		{"other host", authorizeURL(base, "/oauth/authorize", "http://evil.test:8123/callback", claudeScopes, ch, "s"), "Invalid redirect_uri"},
		{"no port", authorizeURL(base, "/oauth/authorize", "http://localhost/callback", claudeScopes, ch, "s"), "Invalid redirect_uri"},
		{"port 0", authorizeURL(base, "/oauth/authorize", "http://localhost:0/callback", claudeScopes, ch, "s"), "Invalid redirect_uri"},
		{"query", authorizeURL(base, "/oauth/authorize", local+"?x=1", claudeScopes, ch, "s"), "Invalid redirect_uri"},
		{"unparsable", authorizeURL(base, "/oauth/authorize", "http://[::1/callback", claudeScopes, ch, "s"), "Invalid redirect_uri"},
		{"unknown scope", authorizeURL(base, "/oauth/authorize", local, "user:inference user:root", ch, "s"), "Invalid scope"},
		{"no scope", authorizeURL(base, "/oauth/authorize", local, "", ch, "s"), "Invalid scope"},
	} {
		a := send(t, "GET", c.url, nil, "")
		if a.status != 400 || a.body != c.want || !strings.HasPrefix(a.header.Get("Content-Type"), "text/plain") {
			t.Errorf("%s: %d %q", c.name, a.status, a.body)
		}
	}
	if a := send(t, "GET", good, nil, ""); a.status != 302 || !strings.HasPrefix(a.header.Get("Location"), local+"?code=") {
		t.Errorf("127.0.0.1 callback: %d %v", a.status, a.header)
	}

	hold := startOAuth(t, strings.Replace(oauthConfig, "label: assinante,", "label: assinante, status: suspended,", 1), newClock(time.Now()))
	a := send(t, "GET", authorizeURL(hold, "/oauth/authorize", local, claudeScopes, ch, "s9"), nil, "")
	if a.status != 302 || a.header.Get("Location") != local+"?error=access_denied&error_description=account_on_hold&state=s9" {
		t.Errorf("on hold: %d %q", a.status, a.header.Get("Location"))
	}
	a = send(t, "GET", authorizeURL(hold, "/oauth/authorize", "https://platform.claude.com/oauth/code/callback", claudeScopes, ch, "s9"), nil, "")
	if a.status != 400 || a.body != "account_on_hold" {
		t.Errorf("on hold, manual: %d %q", a.status, a.body)
	}
}

// TestOAuthTokenRefusals: the token route's OAuth errors (spec 006
// 16.2.1): the client, the grant, the code (expired too), the refresh
// token (invalid, expired, a scope beyond its own) and the account on
// hold, which a code issued before the hold still meets (tokens are
// stateless: any instance with the same key verifies them).
func TestOAuthTokenRefusals(t *testing.T) {
	clock := newClock(time.Now())
	base := startOAuth(t, oauthConfig, clock)
	post := func(base string, body map[string]any) answer {
		b, _ := json.Marshal(body)
		return send(t, "POST", base+"/v1/oauth/token", map[string]string{"Content-Type": "application/json"}, string(b))
	}
	const cid = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	verifier, ch := pkce(t)
	const local = "http://localhost:9/callback"
	codeOf := func() string {
		a := send(t, "GET", authorizeURL(base, "/oauth/authorize", local, claudeScopes, ch, "s"), nil, "")
		u, _ := url.Parse(a.header.Get("Location"))
		return u.Query().Get("code")
	}
	code := codeOf()
	tk := login(t, base, "user:profile user:inference")
	hold := startOAuth(t, strings.Replace(oauthConfig, "label: assinante,", "label: assinante, status: banned,", 1), newClock(time.Now()))
	for _, c := range []struct {
		name   string
		base   string
		body   map[string]any
		status int
		want   string
	}{
		{"client", base, map[string]any{"grant_type": "refresh_token", "client_id": "x"}, 401, `{"error":"invalid_client","error_description":"Invalid client_id"}`},
		{"grant", base, map[string]any{"grant_type": "password", "client_id": cid}, 400, `{"error":"unsupported_grant_type","error_description":"Unsupported grant_type"}`},
		{"code", base, map[string]any{"grant_type": "authorization_code", "code": "nope", "redirect_uri": local, "client_id": cid, "code_verifier": verifier}, 400, `{"error":"invalid_grant","error_description":"Invalid 'code' in request."}`},
		{"no verifier", base, map[string]any{"grant_type": "authorization_code", "code": code, "redirect_uri": local, "client_id": cid}, 400, `{"error":"invalid_grant","error_description":"Invalid 'code' in request."}`},
		{"refresh", base, map[string]any{"grant_type": "refresh_token", "refresh_token": "sk-ant-ort01-nope", "client_id": cid}, 400, `{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`},
		{"access as refresh", base, map[string]any{"grant_type": "refresh_token", "refresh_token": tk.AccessToken, "client_id": cid}, 400, `{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`},
		{"scope beyond", base, map[string]any{"grant_type": "refresh_token", "refresh_token": tk.RefreshToken, "client_id": cid, "scope": "user:profile org:create_api_key"}, 400, `{"error":"invalid_scope","error_description":"The requested scope is invalid, unknown, malformed, or exceeds the scope granted by the resource owner."}`},
		{"unknown scope", base, map[string]any{"grant_type": "refresh_token", "refresh_token": tk.RefreshToken, "client_id": cid, "scope": "user:root"}, 400, `{"error":"invalid_scope","error_description":"The requested scope is invalid, unknown, malformed, or exceeds the scope granted by the resource owner."}`},
		{"on hold, code", hold, map[string]any{"grant_type": "authorization_code", "code": code, "redirect_uri": local, "client_id": cid, "code_verifier": verifier}, 400, `{"error":"invalid_grant","error_description":"account_on_hold"}`},
		{"on hold, refresh", hold, map[string]any{"grant_type": "refresh_token", "refresh_token": tk.RefreshToken, "client_id": cid}, 400, `{"error":"invalid_grant","error_description":"account_on_hold"}`},
	} {
		if a := post(c.base, c.body); a.status != c.status || a.body != c.want {
			t.Errorf("%s: %d %s", c.name, a.status, a.body)
		}
	}
	if a := send(t, "POST", base+"/v1/oauth/token", nil, "grant_type=refresh_token"); a.status != 400 || a.body != `{"error":"invalid_request","error_description":"Invalid request body"}` {
		t.Errorf("form body: %d %s", a.status, a.body)
	}
	// Without a scope the refresh keeps the token's; revoke answers {}.
	if a := post(base, map[string]any{"grant_type": "refresh_token", "refresh_token": tk.RefreshToken, "client_id": cid, "scope": " "}); a.status != 200 || !strings.Contains(a.body, `"scope":"user:profile user:inference"`) {
		t.Errorf("refresh without scope: %d %s", a.status, a.body)
	}
	if a := post(base, map[string]any{"token": tk.RefreshToken, "token_type_hint": "refresh_token", "client_id": cid}); a.status != 400 {
		t.Errorf("revoke body on the token route: %d", a.status)
	}
	if a := send(t, "POST", base+"/v1/oauth/token/revoke", map[string]string{"Content-Type": "application/json"}, `{"token":"x"}`); a.status != 200 || a.body != "{}" {
		t.Errorf("revoke: %d %s", a.status, a.body)
	}
	// Expired code (600 s) and refresh token (30 days).
	clock.add(11 * time.Minute)
	if a := post(base, map[string]any{"grant_type": "authorization_code", "code": code, "redirect_uri": local, "client_id": cid, "code_verifier": verifier}); a.status != 400 {
		t.Errorf("expired code: %d %s", a.status, a.body)
	}
	clock.add(31 * 24 * time.Hour)
	if a := post(base, map[string]any{"grant_type": "refresh_token", "refresh_token": tk.RefreshToken, "client_id": cid}); a.status != 400 || a.body != `{"error":"invalid_grant","error_description":"Refresh token expired"}` {
		t.Errorf("expired refresh: %d %s", a.status, a.body)
	}
}

// TestOAuthAccountRoutes: the account routes Claude Code reads after the
// login, the scopes they want and the API key forms.
func TestOAuthAccountRoutes(t *testing.T) {
	clock := newClock(time.Now())
	base := startOAuth(t, oauthConfig, clock)
	full := login(t, base, claudeScopes)
	narrow := login(t, base, "user:profile")
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	var prof struct {
		Account struct {
			UUID string `json:"uuid"`
		} `json:"account"`
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	decode(t, send(t, "GET", base+"/api/oauth/profile", bearer(full.AccessToken), ""), &prof)
	for _, c := range []struct {
		name, method, path string
		hdr                map[string]string
		status             int
		want               string
	}{
		{"validate", "POST", "/api/oauth/validate", bearer(full.AccessToken), 200, `{"organization_uuid":"` + prof.Organization.UUID + `","account_uuid":"` + prof.Account.UUID + `"}`},
		{"roles", "GET", "/api/oauth/claude_cli/roles", bearer(narrow.AccessToken), 200, `{"organization_role":"admin","workspace_role":null,"organization_name":"acct-sub"}`},
		{"create key", "POST", "/api/oauth/claude_cli/create_api_key", bearer(full.AccessToken), 200, `{"raw_key":"wwz-oauth-secret-0001"}`},
		{"bootstrap", "GET", "/api/claude_cli/bootstrap?entrypoint=cli&model=claude-sonnet-5-5", bearer(full.AccessToken), 200, `{"oauth_account":{"account_uuid":"` + prof.Account.UUID + `","account_email":"assinante@wwz.invalid","organization_uuid":"` + prof.Organization.UUID + `","organization_name":"acct-sub","organization_type":"claude_max","organization_rate_limit_tier":"default_claude_max_20x","user_rate_limit_tier":"default_claude_max_20x","seat_tier":null}}`},
		{"bootstrap with a key", "GET", "/api/claude_cli/bootstrap", map[string]string{"x-api-key": "wwz-key-api"}, 200, `{}`},
		{"first token date", "GET", "/api/organization/claude_code_first_token_date", bearer(full.AccessToken), 200, `{"first_token_date":null}`},
	} {
		a := send(t, c.method, base+c.path, c.hdr, "")
		if a.status != c.status || a.body != c.want || a.header.Get("Content-Type") != "application/json" || !reProfileReqID.MatchString(a.header.Get("request-id")) {
			t.Errorf("%s: %d %s %v", c.name, a.status, a.body, a.header)
		}
	}
	a := send(t, "GET", base+"/api/claude_cli_profile?account_uuid=x", map[string]string{"x-api-key": "wwz-key-api", "anthropic-beta": "oauth-2025-04-20"}, "")
	if a.status != 200 || !strings.Contains(a.body, `"email":"api@wwz.invalid"`) || !strings.Contains(a.body, `"name":"acct-api"`) {
		t.Errorf("claude_cli_profile: %d %s", a.status, a.body)
	}
	if a := send(t, "GET", base+"/api/claude_cli_profile", map[string]string{"x-api-key": "sk-ant-api03-bad"}, ""); a.status != 401 {
		t.Errorf("claude_cli_profile, bad key: %d %s", a.status, a.body)
	}
	clock.add(9 * time.Hour)
	expired := full.AccessToken
	fresh := login(t, base, "user:inference user:profile")
	for _, c := range []struct {
		name, method, path string
		hdr                map[string]string
		status             int
		typ, msg           string
	}{
		{"profile, invalid", "GET", "/api/oauth/profile", bearer("sk-ant-oat01-invalid"), 401, "authentication_error", "OAuth access token is invalid."},
		{"profile, expired", "GET", "/api/oauth/profile", bearer(expired), 401, "authentication_error", "OAuth token has expired. Please obtain a new token or refresh your existing token."},
		{"usage, no token", "GET", "/api/oauth/usage", nil, 401, "authentication_error", "OAuth access token is invalid."},
		{"create key without the scope", "POST", "/api/oauth/claude_cli/create_api_key", bearer(fresh.AccessToken), 403, "permission_error", "OAuth token does not meet scope requirement org:create_api_key"},
		{"bootstrap without a key", "GET", "/api/claude_cli/bootstrap", nil, 401, "authentication_error", "x-api-key header is required"},
		{"bootstrap with a bad key", "GET", "/api/claude_cli/bootstrap", map[string]string{"x-api-key": "sk-ant-api03-bad"}, 401, "authentication_error", "invalid x-api-key"},
		{"messages with the token in x-api-key", "POST", "/v1/messages", map[string]string{"x-api-key": fresh.AccessToken, "anthropic-version": "2023-06-01", "anthropic-beta": oauthBetaForTest}, 401, "authentication_error", "invalid x-api-key"},
	} {
		a := send(t, c.method, base+c.path, c.hdr, "")
		if typ, msg := apiError(t, a); a.status != c.status || typ != c.typ || msg != c.msg {
			t.Errorf("%s: %d %s %q", c.name, a.status, typ, msg)
		}
	}
	// A token without user:inference cannot infer.
	a = messages(t, base, login(t, base, "user:profile").AccessToken, ccBetas)
	if typ, msg := apiError(t, a); a.status != 403 || typ != "permission_error" || msg != "OAuth token does not meet scope requirement user:inference" {
		t.Errorf("inference without the scope: %d %s %q", a.status, typ, msg)
	}
	// GET /v1/models with the token wants the beta too.
	a = send(t, "GET", base+"/v1/models", map[string]string{"Authorization": "Bearer " + fresh.AccessToken, "anthropic-version": "2023-06-01"}, "")
	if _, msg := apiError(t, a); a.status != 401 || msg != "OAuth authentication is currently not supported." {
		t.Errorf("models without the beta: %d %q", a.status, msg)
	}
	a = send(t, "GET", base+"/v1/models", map[string]string{"Authorization": "Bearer " + fresh.AccessToken, "anthropic-version": "2023-06-01", "anthropic-beta": oauthBetaForTest}, "")
	if a.status != 200 {
		t.Errorf("models: %d %s", a.status, a.body)
	}
}

const oauthBetaForTest = "oauth-2025-04-20"

// TestOAuthWithoutKey: without oauth_key there is no subscription (spec
// 006 16.2.1): every Bearer sk-ant-oat… (a keys[] entry aside) is the
// invalid token's 401, on inference and on the account routes, and the
// OAuth routes are unknown routes; /v1/oauth/hello and the first token
// date still answer. With server.auth: none any Bearer passes, and an
// sk-ant-oat one gets the unified headers.
func TestOAuthWithoutKey(t *testing.T) {
	base := startOAuth(t, strings.Replace(ccConfig, "keys: [{key: wwz-key-cc}]", "keys: [{key: wwz-key-cc}, {key: sk-ant-oat01-literal-key-in-keys}]", 1), newClock(time.Now()))
	tok := map[string]string{"Authorization": "Bearer sk-ant-oat01-WWZFAKEtoken", "anthropic-version": "2023-06-01", "anthropic-beta": oauthBetaForTest}
	for _, path := range []string{"/v1/messages", "/api/oauth/profile", "/api/oauth/usage"} {
		a := send(t, "POST", base+path, tok, `{"model":"claude-sonnet-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
		if _, msg := apiError(t, a); a.status != 401 || msg != "OAuth access token is invalid." {
			t.Errorf("%s: %d %q", path, a.status, msg)
		}
	}
	for _, c := range []struct {
		method, path string
		hdr          map[string]string
		status       int
		body         string
	}{
		{"GET", "/oauth/authorize?client_id=x", nil, 404, ""},
		{"GET", "/api/oauth/profile", nil, 404, ""},
		{"POST", "/v1/oauth/token", map[string]string{"Content-Type": "application/json"}, 404, `{"type":"error","error":{"type":"not_found_error","message":"Not found"},"request_id":null}`},
		{"GET", "/v1/oauth/hello", nil, 200, `{"message":"hello"}`},
		{"GET", "/api/organization/claude_code_first_token_date", nil, 200, `{"first_token_date":null}`},
	} {
		if a := send(t, c.method, base+c.path, c.hdr, ""); a.status != c.status || a.body != c.body {
			t.Errorf("%s %s: %d %q", c.method, c.path, a.status, a.body)
		}
	}
	a := send(t, "POST", base+"/v1/oauth/hello", nil, "")
	if typ, msg := apiError(t, a); a.status != 405 || typ != "invalid_request_error" || msg != "Method Not Allowed" || a.header.Get("Allow") != "GET" || a.header.Get("x-should-retry") != "false" {
		t.Errorf("POST /v1/oauth/hello: %d %v", a.status, a.header)
	}
	// A keys[] entry whose value looks like a token is a key: 200, with
	// the unified headers (no quota: utilization 0).
	a = messages(t, base, "sk-ant-oat01-literal-key-in-keys", "")
	if a.status != 200 || a.header.Get("anthropic-ratelimit-unified-5h-utilization") != "0" {
		t.Errorf("literal key: %d %v", a.status, a.header)
	}
	open := startOAuth(t, strings.Replace(ccConfig, "auth: keys", "auth: none", 1), newClock(time.Now()))
	a = messages(t, open, "sk-ant-oat01-anything", "")
	if a.status != 200 || a.header.Get("anthropic-ratelimit-unified-status") != "allowed" || a.header.Get("anthropic-ratelimit-requests-limit") != "" {
		t.Errorf("auth none: %d %v", a.status, a.header)
	}
}
