package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/claudecode"
)

// OAuth constants of Claude Code 2.1.308 (spec 006, 16.2.1;
// tmp/claude-code/oauth-shapes.md).
const (
	ClientID          = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	ManualRedirectURL = "https://platform.claude.com/oauth/code/callback"
	// SetupTokenExpiresIn is the expires_in `claude setup-token` asks for.
	SetupTokenExpiresIn = 31536000
	ScopeInference      = "user:inference"
	ScopeProfile        = "user:profile"
	// loginRefreshLife is the refresh token life Claude Code assumes when the
	// token endpoint does not say it.
	loginRefreshLife = 30 * 24 * time.Hour
)

// LoginScopes are the scopes of a claude.ai login, in order.
var LoginScopes = []string{
	"org:create_api_key", ScopeProfile, ScopeInference, "user:sessions:claude_code",
	"user:mcp_servers", "user:file_upload", "user:plugins",
}

// Account is the account of a token or a profile.
type Account struct {
	UUID  string `json:"uuid"`
	Email string `json:"email,omitempty"`
}

// Organization is the organization of a token or a profile.
type Organization struct {
	UUID          string `json:"uuid"`
	Name          string `json:"name,omitempty"`
	Type          string `json:"organization_type,omitempty"`
	RateLimitTier string `json:"rate_limit_tier,omitempty"`
}

// Tokens is the answer of the token endpoint.
type Tokens struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAt        time.Time
	RefreshExpiresAt time.Time // zero when unknown
	Scopes           []string
	Account          Account
	Organization     Organization
}

// Profile is GET /api/oauth/profile.
type Profile struct {
	Account      Account
	Organization Organization
	// Plan is max, pro, team or enterprise, from organization_type ("" for
	// another type).
	Plan string
	// Verified: the response had a request-id of the form Claude Code
	// trusts (^req_[A-Za-z0-9_-]{1,36}$); without it the plan is not
	// confirmed.
	Verified bool
}

// Roles is GET /api/oauth/claude_cli/roles.
type Roles struct {
	OrganizationRole string  `json:"organization_role"`
	WorkspaceRole    *string `json:"workspace_role"`
	OrganizationName string  `json:"organization_name"`
}

// LoginResult is the outcome of [Client.Login].
type LoginResult struct {
	Tokens  *Tokens
	Profile *Profile // nil without the user:profile scope (setup token)
	Roles   *Roles   // nil without the user:profile scope
}

// ServerError is a refusal of the server (status ≥ 400, or an unexpected
// status), with the OAuth or API error it carried.
type ServerError struct {
	Route       string
	Status      int
	Code        string // error (OAuth) or error.type (API)
	Description string // error_description or error.message
	Body        string
}

func (e *ServerError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: HTTP %d: %s: %s", e.Route, e.Status, e.Code, e.Description)
	}
	body := strings.TrimSpace(e.Body)
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Route, e.Status, body)
}

func serverError(route string, status int, body []byte) *ServerError {
	e := &ServerError{Route: route, Status: status, Body: string(body)}
	var v struct {
		Error       json.RawMessage `json:"error"`
		Description string          `json:"error_description"`
	}
	if json.Unmarshal(body, &v) != nil {
		return e
	}
	var code string
	var obj struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	switch {
	case json.Unmarshal(v.Error, &code) == nil:
		e.Code, e.Description = code, v.Description
	case json.Unmarshal(v.Error, &obj) == nil:
		e.Code, e.Description = obj.Type, obj.Message
	}
	return e
}

// ErrNotFollowed is returned when the authorize step redirects somewhere
// other than the local callback: the client never follows it.
var ErrNotFollowed = errors.New("redirect not followed")

// b64 is base64url without padding, as Claude Code encodes PKCE and state.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func random32() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return b64(b[:])
}

// formEncode encodes like URLSearchParams (application/x-www-form-urlencoded):
// alphanumerics and *-._ stay, space is +, the rest is %XX.
func formEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// AuthorizeQuery returns the query of GET /oauth/authorize in the order of
// Claude Code.
func AuthorizeQuery(redirectURI string, scopes []string, challenge, state string) string {
	params := [][2]string{
		{"code", "true"},
		{"client_id", ClientID},
		{"response_type", "code"},
		{"redirect_uri", redirectURI},
		{"scope", strings.Join(scopes, " ")},
		{"code_challenge", challenge},
		{"code_challenge_method", "S256"},
		{"state", state},
	}
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = p[0] + "=" + formEncode(p[1])
	}
	return strings.Join(parts, "&")
}

// call sends one request on a new connection and reads the whole response.
func (c *Client) call(ctx context.Context, d Dialer, t *Target, method, target string,
	headers []claudecode.Header, body []byte,
) (*Response, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.o.Timeout)
	defer cancel()
	cn, err := dial(ctx, d, t, c.o.TLSConfig)
	if err != nil {
		return nil, nil, err
	}
	defer cn.close()
	defer cn.arm(ctx)()
	h := claudecode.Head{Method: method, Target: target, Proto: "HTTP/1.1", Headers: headers}
	h.Headers = append(h.Headers, claudecode.Header{Name: "Host", Value: t.host})
	if body != nil {
		h.Headers = append(h.Headers, claudecode.Header{Name: "Content-Length", Value: strconv.Itoa(len(body))})
	}
	h.Headers = append(h.Headers, claudecode.Header{Name: "Connection", Value: "close"})
	resp, err := cn.exchange(append(h.Append(nil), body...), false)
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp, data, err
}

// browserHeaders are sent by the simulated browser of the login.
var browserHeaders = []claudecode.Header{
	{Name: "User-Agent", Value: "wwz-load claude-code"},
	{Name: "Accept", Value: "text/html,*/*"},
}

// tokenHeaders are the headers of the token endpoint (axios with JSON).
var tokenHeaders = []claudecode.Header{
	{Name: "Accept", Value: "application/json, text/plain, */*"},
	{Name: "Content-Type", Value: "application/json"},
}

// accountHeaders are the headers of the account routes of Claude Code.
func (c *Client) accountHeaders(access string) []claudecode.Header {
	return []claudecode.Header{
		{Name: "User-Agent", Value: "claude-code/" + c.p.Version},
		{Name: "Authorization", Value: "Bearer " + access},
		{Name: "Content-Type", Value: "application/json"},
		{Name: "Cache-Control", Value: "no-cache"},
	}
}

// LoginOptions select the login flow.
type LoginOptions struct {
	// Manual uses the manual redirect: the authorize page shows code#state
	// instead of redirecting to a local callback.
	Manual bool
	// SetupToken asks only user:inference and a one-year token, like
	// `claude setup-token`.
	SetupToken bool
}

// listen opens the callback listener (a seam for tests).
var listen = net.Listen

// callback is the local listener of the login (127.0.0.1, ephemeral port).
type callback struct {
	state, success string
	mu             sync.Mutex
	code           string
	err            error
}

func (cb *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	q := r.URL.Query()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	switch {
	case r.URL.Path != "/callback":
		cb.err = fmt.Errorf("login: the authorize redirect went to %s, not /callback", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	case q.Get("error") != "":
		cb.err = fmt.Errorf("login: sign-in failed: %s (%s)", q.Get("error"), q.Get("error_description"))
		_, _ = io.WriteString(w, "Sign-in was canceled or failed. You can close this window.")
	case q.Get("code") == "":
		cb.err = errors.New("login: authorization code not found")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Authorization code not found")
	case q.Get("state") != cb.state:
		cb.err = errors.New("login: invalid state parameter")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Invalid state parameter")
	default:
		cb.code = q.Get("code")
		w.Header().Set("Location", cb.success)
		w.WriteHeader(http.StatusFound)
	}
}

// Login runs the OAuth flow of Claude Code against the target: PKCE
// (verifier and state of 32 random bytes), GET /oauth/authorize, the local
// callback or the manual code, POST /v1/oauth/token and, with the
// user:profile scope, GET /api/oauth/profile and /api/oauth/claude_cli/roles.
func (c *Client) Login(ctx context.Context, o LoginOptions) (*LoginResult, error) {
	verifier, state := random32(), random32()
	sum := sha256.Sum256([]byte(verifier))
	challenge := b64(sum[:])
	scopes := LoginScopes
	if o.SetupToken {
		scopes = []string{ScopeInference}
	}
	var code, redirect string
	var err error
	if o.Manual {
		redirect = ManualRedirectURL
		code, err = c.manualCode(ctx, redirect, scopes, challenge, state)
	} else {
		code, redirect, err = c.callbackCode(ctx, scopes, challenge, state)
	}
	if err != nil {
		return nil, err
	}
	members := []claudecode.Member{
		claudecode.M("grant_type", claudecode.String("authorization_code")),
		claudecode.M("code", claudecode.String(code)),
		claudecode.M("redirect_uri", claudecode.String(redirect)),
		claudecode.M("client_id", claudecode.String(ClientID)),
		claudecode.M("code_verifier", claudecode.String(verifier)),
		claudecode.M("state", claudecode.String(state)),
	}
	if o.SetupToken {
		members = append(members, claudecode.M("expires_in", claudecode.Int(SetupTokenExpiresIn)))
	}
	t, err := c.token(ctx, members, "")
	if err != nil {
		return nil, err
	}
	res := &LoginResult{Tokens: t}
	if !slices.Contains(t.Scopes, ScopeProfile) {
		return res, nil
	}
	if res.Profile, err = c.Profile(ctx, t.AccessToken); err != nil {
		return nil, err
	}
	if res.Roles, err = c.Roles(ctx, t.AccessToken); err != nil {
		return nil, err
	}
	return res, nil
}

// authorize sends GET /oauth/authorize.
func (c *Client) authorize(ctx context.Context, redirect string, scopes []string, challenge, state string) (*Response, []byte, error) {
	target := c.o.Target.prefix + "/oauth/authorize?" + AuthorizeQuery(redirect, scopes, challenge, state)
	return c.call(ctx, c.o.Dial, c.o.Target, "GET", target, browserHeaders, nil)
}

// manualCode reads code#state from the manual authorize page.
func (c *Client) manualCode(ctx context.Context, redirect string, scopes []string, challenge, state string) (string, error) {
	resp, body, err := c.authorize(ctx, redirect, scopes, challenge, state)
	if err != nil {
		return "", fmt.Errorf("authorize: %w", err)
	}
	if resp.Status != http.StatusOK {
		return "", serverError("authorize", resp.Status, body)
	}
	code, got, _ := strings.Cut(strings.TrimSpace(string(body)), "#")
	switch {
	case code == "" || got == "":
		return "", errors.New("login: invalid code: the authorize page did not show code#state")
	case got != state:
		return "", errors.New("login: that code is not from this sign-in (state mismatch)")
	}
	return code, nil
}

// callbackCode runs the authorize step with the local callback: the
// redirect of the authorize page is delivered to a listener on 127.0.0.1
// (redirect_uri http://localhost:<port>/callback), as the browser would.
func (c *Client) callbackCode(ctx context.Context, scopes []string, challenge, state string) (code, redirect string, err error) {
	ln, err := listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("login: callback listener: %w", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	redirect = "http://localhost:" + port + "/callback"
	cb := &callback{state: state, success: c.o.Target.raw + "/oauth/code/success?app=claude-code"}
	srv := &http.Server{Handler: cb, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	resp, body, err := c.authorize(ctx, redirect, scopes, challenge, state)
	if err != nil {
		return "", "", fmt.Errorf("authorize: %w", err)
	}
	if resp.Status != http.StatusFound && resp.Status != http.StatusSeeOther && resp.Status != http.StatusTemporaryRedirect {
		return "", "", serverError("authorize", resp.Status, body)
	}
	loc, _ := resp.Get("Location")
	u, perr := url.Parse(loc)
	if perr != nil || u.Scheme != "http" || (u.Host != "localhost:"+port && u.Host != "127.0.0.1:"+port) {
		return "", "", fmt.Errorf("authorize: %w: Location %q is not the local callback", ErrNotFollowed, loc)
	}
	local := &Target{addr: "127.0.0.1:" + port, hostname: "localhost", host: u.Host}
	if _, _, err := c.call(ctx, nil, local, "GET", u.RequestURI(), browserHeaders, nil); err != nil {
		return "", "", fmt.Errorf("login: callback: %w", err)
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.code, redirect, cb.err
}

type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn *int64 `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
	Account               struct {
		UUID  string `json:"uuid"`
		Email string `json:"email_address"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

// token posts the body (members in the order of Claude Code) to
// /v1/oauth/token. oldRefresh is kept when the answer has no refresh token
// (refresh); "" marks a login.
func (c *Client) token(ctx context.Context, members []claudecode.Member, oldRefresh string) (*Tokens, error) {
	body := claudecode.Marshal(claudecode.Object(members...))
	resp, data, err := c.call(ctx, c.o.Dial, c.o.Target, "POST", c.o.Target.prefix+"/v1/oauth/token", tokenHeaders, body)
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, serverError("token", resp.Status, data)
	}
	var tr tokenResponse
	if json.Unmarshal(data, &tr) != nil || tr.AccessToken == "" {
		return nil, fmt.Errorf("token: %w: no access_token in %.200q", errParse, data)
	}
	now := c.now()
	t := &Tokens{
		AccessToken: tr.AccessToken, RefreshToken: cmpOr(tr.RefreshToken, oldRefresh),
		ExpiresAt:    now.Add(time.Duration(tr.ExpiresIn) * time.Second),
		Scopes:       strings.Fields(tr.Scope),
		Account:      Account{UUID: tr.Account.UUID, Email: tr.Account.Email},
		Organization: Organization{UUID: tr.Organization.UUID, Name: tr.Organization.Name},
	}
	switch {
	case tr.RefreshTokenExpiresIn != nil:
		t.RefreshExpiresAt = now.Add(time.Duration(*tr.RefreshTokenExpiresIn) * time.Second)
	case oldRefresh == "":
		t.RefreshExpiresAt = now.Add(loginRefreshLife)
	}
	return t, nil
}

// Refresh exchanges a refresh token (POST /v1/oauth/token with grant_type
// refresh_token and the saved scopes).
func (c *Client) Refresh(ctx context.Context, refreshToken string, scopes []string) (*Tokens, error) {
	return c.token(ctx, []claudecode.Member{
		claudecode.M("grant_type", claudecode.String("refresh_token")),
		claudecode.M("refresh_token", claudecode.String(refreshToken)),
		claudecode.M("client_id", claudecode.String(ClientID)),
		claudecode.M("scope", claudecode.String(strings.Join(scopes, " "))),
	}, refreshToken)
}

// requestIDForm is the request-id Claude Code trusts on the profile.
var requestIDForm = regexp.MustCompile(`^req_[A-Za-z0-9_-]{1,36}$`)

// plans maps organization_type to the plan of Claude Code.
var plans = map[string]string{
	"claude_max": "max", "claude_pro": "pro", "claude_enterprise": "enterprise", "claude_team": "team",
}

// Profile sends GET /api/oauth/profile.
func (c *Client) Profile(ctx context.Context, access string) (*Profile, error) {
	resp, data, err := c.call(ctx, c.o.Dial, c.o.Target, "GET", c.o.Target.prefix+"/api/oauth/profile",
		c.accountHeaders(access), nil)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, serverError("profile", resp.Status, data)
	}
	var v struct {
		Account      Account      `json:"account"`
		Organization Organization `json:"organization"`
	}
	if json.Unmarshal(data, &v) != nil || v.Account.UUID == "" || v.Account.Email == "" || v.Organization.UUID == "" {
		return nil, fmt.Errorf("profile: %w: want account.uuid, account.email and organization.uuid in %.200q", errParse, data)
	}
	rid, _ := resp.Get("request-id")
	return &Profile{
		Account: v.Account, Organization: v.Organization,
		Plan: plans[v.Organization.Type], Verified: requestIDForm.MatchString(rid),
	}, nil
}

// Roles sends GET /api/oauth/claude_cli/roles.
func (c *Client) Roles(ctx context.Context, access string) (*Roles, error) {
	resp, data, err := c.call(ctx, c.o.Dial, c.o.Target, "GET", c.o.Target.prefix+"/api/oauth/claude_cli/roles",
		c.accountHeaders(access)[:2], nil)
	if err != nil {
		return nil, fmt.Errorf("roles: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, serverError("roles", resp.Status, data)
	}
	r := &Roles{}
	if json.Unmarshal(data, r) != nil {
		return nil, fmt.Errorf("roles: %w: %.200q", errParse, data)
	}
	return r, nil
}

// SetTokens records t in the state (refresh keeps the account and the
// organization of the login when the answer has none).
func (s *State) SetTokens(t *Tokens) {
	s.AccessToken, s.RefreshToken = t.AccessToken, t.RefreshToken
	s.ExpiresAt, s.RefreshExpiresAt = t.ExpiresAt.Unix(), 0
	if !t.RefreshExpiresAt.IsZero() {
		s.RefreshExpiresAt = t.RefreshExpiresAt.Unix()
	}
	s.Scopes = t.Scopes
	if t.Account.UUID != "" {
		s.Account = &Account{UUID: t.Account.UUID, Email: t.Account.Email}
	}
	if t.Organization.UUID != "" {
		org := t.Organization
		if s.Organization != nil && s.Organization.UUID == org.UUID {
			org.Type, org.RateLimitTier = s.Organization.Type, s.Organization.RateLimitTier
		}
		s.Organization = &org
	}
}

// SetLogin records a login in the state.
func (s *State) SetLogin(target string, r *LoginResult) {
	s.Target, s.Plan = target, ""
	s.Account, s.Organization = nil, nil
	s.SetTokens(r.Tokens)
	if r.Profile != nil {
		s.Account = &r.Profile.Account
		s.Organization = &r.Profile.Organization
		s.Plan = r.Profile.Plan
	}
}
