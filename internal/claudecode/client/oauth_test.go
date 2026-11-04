package client

import (
	"context"
	"errors"
	"net"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFormEncodeAndQuery(t *testing.T) {
	if got := formEncode("a b*-._~/:é"); got != "a+b*-._%7E%2F%3A%C3%A9" {
		t.Errorf("formEncode %q", got)
	}
	got := AuthorizeQuery("http://localhost:5/callback", []string{"user:profile", "user:inference"}, "CH", "ST")
	want := "code=true&client_id=" + ClientID + "&response_type=code&redirect_uri=http%3A%2F%2Flocalhost%3A5%2Fcallback" +
		"&scope=user%3Aprofile+user%3Ainference&code_challenge=CH&code_challenge_method=S256&state=ST"
	if got != want {
		t.Errorf("query\n got %s\nwant %s", got, want)
	}
}

var b64url43 = `[A-Za-z0-9_-]{43}`

func TestLoginCallback(t *testing.T) {
	o := &fakeOAuth{}
	c, fs := newOAuthClient(t, o)
	c.now = func() time.Time { return fixedDay }
	res, err := c.Login(context.Background(), LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tk := res.Tokens
	if !strings.HasPrefix(tk.AccessToken, "sk-ant-oat01-") || !strings.HasPrefix(tk.RefreshToken, "sk-ant-ort01-") ||
		!slices.Equal(tk.Scopes, LoginScopes) || !tk.ExpiresAt.Equal(fixedDay.Add(8*time.Hour)) ||
		!tk.RefreshExpiresAt.Equal(fixedDay.Add(30*24*time.Hour)) || tk.Account.UUID != fakeAccount {
		t.Errorf("tokens %+v", tk)
	}
	p := res.Profile
	if p == nil || !p.Verified || p.Plan != "max" || p.Organization.RateLimitTier != "default_claude_max_5x" ||
		p.Account.Email != "wwz-key-01@wwz.invalid" || res.Roles.OrganizationRole != "admin" || res.Roles.WorkspaceRole != nil {
		t.Errorf("profile %+v roles %+v", p, res.Roles)
	}
	reqs := fs.requests()
	var routes []string
	for _, r := range reqs {
		routes = append(routes, r.Head.Method+" "+r.path())
	}
	if !slices.Equal(routes, []string{"GET /oauth/authorize", "POST /v1/oauth/token", "GET /api/oauth/profile", "GET /api/oauth/claude_cli/roles"}) {
		t.Fatalf("routes %v", routes)
	}
	query := regexp.MustCompile(`^code=true&client_id=` + ClientID + `&response_type=code&redirect_uri=http%3A%2F%2Flocalhost%3A\d+%2Fcallback` +
		`&scope=org%3Acreate_api_key\+user%3Aprofile\+user%3Ainference\+user%3Asessions%3Aclaude_code\+user%3Amcp_servers\+user%3Afile_upload\+user%3Aplugins` +
		`&code_challenge=` + b64url43 + `&code_challenge_method=S256&state=` + b64url43 + `$`)
	if !query.MatchString(reqs[0].query()) {
		t.Errorf("authorize query %s", reqs[0].query())
	}
	body := regexp.MustCompile(`^\{"grant_type":"authorization_code","code":"[^"]+","redirect_uri":"http://localhost:\d+/callback",` +
		`"client_id":"` + ClientID + `","code_verifier":"` + b64url43 + `","state":"` + b64url43 + `"\}$`)
	if !body.Match(reqs[1].Body) {
		t.Errorf("token body %s", reqs[1].Body)
	}
	if ct, _ := reqs[1].Head.Get("Content-Type"); ct != "application/json" {
		t.Errorf("token content-type %q", ct)
	}

	// The state keeps the login; refresh renews it.
	st := &State{DeviceID: NewDeviceID()}
	st.SetLogin("http://x", res)
	if st.AccessToken != tk.AccessToken || st.Plan != "max" || st.Account.UUID != fakeAccount || st.Organization.Type != "claude_max" ||
		st.ExpiresAt != tk.ExpiresAt.Unix() || st.RefreshExpiresAt == 0 {
		t.Errorf("state %+v", st)
	}
	nt, err := c.Refresh(context.Background(), st.RefreshToken, st.Scopes)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(fs.requests()[4].Body); got != `{"grant_type":"refresh_token","refresh_token":"`+st.RefreshToken+`","client_id":"`+ClientID+
		`","scope":"`+strings.Join(LoginScopes, " ")+`"}` {
		t.Errorf("refresh body %s", got)
	}
	st.SetTokens(nt)
	if st.Organization.Type != "claude_max" || st.AccessToken != nt.AccessToken {
		t.Errorf("refresh kept %+v", st.Organization)
	}
	// Without refresh_token nor its expiry in the answer.
	o.noRefresh, o.noRefreshExp = true, true
	nt, err = c.Refresh(context.Background(), st.RefreshToken, st.Scopes)
	if err != nil || nt.RefreshToken != st.RefreshToken || !nt.RefreshExpiresAt.IsZero() {
		t.Fatalf("refresh without refresh_token: %+v %v", nt, err)
	}
	st.SetTokens(&Tokens{AccessToken: "a", Organization: Organization{UUID: "other"}})
	if st.RefreshExpiresAt != 0 || st.Account.UUID != fakeAccount || st.Organization.UUID != "other" || st.Organization.Type != "" {
		t.Errorf("SetTokens %+v %+v", st, st.Organization)
	}
	var se *ServerError
	if _, err := c.Refresh(context.Background(), "sk-ant-ort01-bad", st.Scopes); !errors.As(err, &se) ||
		se.Code != "invalid_grant" || se.Description != "Refresh token not found or invalid" {
		t.Errorf("bad refresh: %v", err)
	}
}

func TestLoginManualAndSetupToken(t *testing.T) {
	c, fs := newOAuthClient(t, &fakeOAuth{})
	res, err := c.Login(context.Background(), LoginOptions{Manual: true})
	if err != nil || res.Profile == nil {
		t.Fatal(err)
	}
	reqs := fs.requests()
	if !strings.Contains(reqs[0].query(), "&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&") ||
		!strings.Contains(string(reqs[1].Body), `"redirect_uri":"`+ManualRedirectURL+`"`) {
		t.Errorf("manual: %s %s", reqs[0].query(), reqs[1].Body)
	}

	o := &fakeOAuth{noRefreshExp: true}
	c, fs = newOAuthClient(t, o)
	c.now = func() time.Time { return fixedDay }
	res, err = c.Login(context.Background(), LoginOptions{SetupToken: true})
	if err != nil {
		t.Fatal(err)
	}
	reqs = fs.requests()
	if len(reqs) != 2 || res.Profile != nil || res.Roles != nil || !strings.Contains(reqs[0].query(), "&scope=user%3Ainference&") ||
		!strings.HasSuffix(string(reqs[1].Body), `,"expires_in":31536000}`) ||
		!res.Tokens.ExpiresAt.Equal(fixedDay.Add(SetupTokenExpiresIn*time.Second)) || !slices.Equal(res.Tokens.Scopes, []string{ScopeInference}) {
		t.Errorf("setup token: %d requests, %+v, %s", len(reqs), res.Tokens, reqs[1].Body)
	}
	st := &State{}
	st.SetLogin("t", res)
	if st.Account.UUID != fakeAccount || st.Plan != "" {
		t.Errorf("setup token state %+v", st)
	}
}

// failingDial fails the n-th connection (1-based) to the OAuth server.
func failingDial(fs *fakeServer, n int32) Dialer {
	d := fs.dialer()
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if fs.dials.Load()+1 == n {
			fs.dials.Add(1)
			return nil, errors.New("dial refused")
		}
		return d(ctx, network, addr)
	}
}

type closingListener struct{ net.Listener }

func (l closingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		_ = c.Close()
	}
	return c, err
}

func TestLoginErrors(t *testing.T) {
	cases := []struct {
		name   string
		o      fakeOAuth
		opts   LoginOptions
		dialN  int32
		listen func(string, string) (net.Listener, error)
		want   string
	}{
		{name: "on hold", o: fakeOAuth{onHold: true}, want: "sign-in failed: access_denied (account_on_hold)"},
		{name: "on hold manual", o: fakeOAuth{onHold: true}, opts: LoginOptions{Manual: true}, want: "authorize: HTTP 400: account_on_hold"},
		{name: "other host", o: fakeOAuth{authorizeLoc: "https://evil.test/callback?code=x"}, want: "redirect not followed"},
		{name: "bad location", o: fakeOAuth{authorizeLoc: "%zz"}, want: "redirect not followed"},
		{name: "other path", o: fakeOAuth{authorizeLoc: "http://{host}/other?code=x"}, want: "not /callback"},
		{name: "no code", o: fakeOAuth{authorizeLoc: "http://{host}/callback?state=x"}, want: "authorization code not found"},
		{name: "bad state", o: fakeOAuth{authorizeLoc: "http://{host}/callback?code=x&state=y"}, want: "invalid state parameter"},
		{name: "authorize 200", o: fakeOAuth{authorizeResp: response(200, nil, "hello")}, want: "authorize: HTTP 200: hello"},
		{name: "manual no hash", o: fakeOAuth{manualBody: "nohash"}, opts: LoginOptions{Manual: true}, want: "invalid code"},
		{name: "manual state", o: fakeOAuth{manualBody: "code#other"}, opts: LoginOptions{Manual: true}, want: "not from this sign-in"},
		{name: "token refused", o: fakeOAuth{tokenStatus: 400}, want: "token: HTTP 400: invalid_grant: Invalid 'code' in request."},
		{name: "token not json", o: fakeOAuth{tokenBody: "nope"}, want: "no access_token"},
		{name: "token without access", o: fakeOAuth{tokenBody: `{}`}, want: "no access_token"},
		{
			name: "profile refused", o: fakeOAuth{tokenBody: `{"access_token":"bad","scope":"user:profile"}`},
			want: "profile: HTTP 401: authentication_error: OAuth access token is invalid.",
		},
		{name: "profile fields", o: fakeOAuth{profileBody: `{"account":{"uuid":"x"}}`}, want: "want account.uuid"},
		{name: "roles refused", o: fakeOAuth{rolesStatus: 500, rolesBody: "boom"}, want: "roles: HTTP 500: boom"},
		{name: "roles not json", o: fakeOAuth{rolesStatus: 200, rolesBody: "nope"}, want: "roles: invalid response"},
		{name: "authorize dial", dialN: 1, want: "authorize: dial refused"},
		{name: "manual dial", dialN: 1, opts: LoginOptions{Manual: true}, want: "authorize: dial refused"},
		{name: "token dial", dialN: 2, want: "token: dial refused"},
		{name: "profile dial", dialN: 3, want: "profile: dial refused"},
		{name: "roles dial", dialN: 4, want: "roles: dial refused"},
		{name: "listen", listen: func(string, string) (net.Listener, error) { return nil, errors.New("no port") }, want: "callback listener: no port"},
		{name: "callback hangs up", listen: func(n, a string) (net.Listener, error) {
			ln, err := net.Listen(n, a)
			return closingListener{ln}, err
		}, want: "login: callback:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, fs := newOAuthClient(t, &tc.o)
			if tc.dialN > 0 {
				c.o.Dial = failingDial(fs, tc.dialN)
			}
			if tc.listen != nil {
				old := listen
				listen = tc.listen
				defer func() { listen = old }()
			}
			_, err := c.Login(context.Background(), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
	// The no-profile path still checks the request-id.
	c, _ := newOAuthClient(t, &fakeOAuth{noRequestID: true})
	res, err := c.Login(context.Background(), LoginOptions{})
	if err != nil || res.Profile.Verified {
		t.Errorf("unstamped profile: %v %+v", err, res.Profile)
	}
	// A dead server: the exchange of the call fails.
	fs := newFake(t, func(rawReq) []byte { return hangUp })
	c, _ = New(Options{Target: mustTarget(t, "http://"+fs.addr())})
	if _, err := c.Refresh(context.Background(), "x", nil); err == nil || !strings.Contains(err.Error(), "token:") {
		t.Errorf("hang up: %v", err)
	}
}

func TestServerError(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"error":"invalid_grant","error_description":"d"}`, "r: HTTP 400: invalid_grant: d"},
		{`{"type":"error","error":{"type":"permission_error","message":"m"}}`, "r: HTTP 400: permission_error: m"},
		{`{"error":5}`, `r: HTTP 400: {"error":5}`},
		{"plain", "r: HTTP 400: plain"},
		{strings.Repeat("x", 300), "r: HTTP 400: " + strings.Repeat("x", 200) + "…"},
	}
	for _, tc := range cases {
		if got := serverError("r", 400, []byte(tc.body)).Error(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.body, got, tc.want)
		}
	}
}
