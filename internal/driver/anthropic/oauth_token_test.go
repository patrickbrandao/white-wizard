package anthropic

import (
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// newOAuthDriver is the anthropic driver built with opts.
func newOAuthDriver(t *testing.T, opts map[string]string) *Driver {
	t.Helper()
	d, err := driver.New("anthropic", opts)
	if err != nil {
		t.Fatal(err)
	}
	return d.(*Driver)
}

const testSecret = "wwz-oauth-secret-0001"

// testKeys finds the oauth_key entry "sub".
func testKeys(label string) *config.KeyConfig {
	if label == "sub" {
		return &config.KeyConfig{Key: testSecret, Label: "sub", Account: "acct-sub"}
	}
	return nil
}

// TestOAuthTokenLayout: the layout of spec 006 16.2.1 (version, scopes,
// expiry, issue, the code's binding, nonce, 16-byte MAC under the prefix)
// and what does not verify: another prefix or secret, a cut, a changed
// byte or version, an empty secret, a code as a token and back.
func TestOAuthTokenLayout(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s, ok := parseScopes("user:inference user:profile")
	if !ok || s.String() != "user:profile user:inference" || !s.has(scopeInference) || s.has(scopeCreateAPIKey) {
		t.Fatalf("scopes %v %q", ok, s)
	}
	tok := newOAuthToken(s, now, time.Hour).sign(testSecret, accessPrefix)
	body := strings.TrimPrefix(tok, accessPrefix)
	if raw, err := tokenB64.DecodeString(body); err != nil || len(raw) != tokenSize || raw[0] != 1 || raw[1] != byte(s) {
		t.Fatalf("layout %x %v", raw, err)
	}
	got, ok := verifyOAuth(testSecret, accessPrefix, tok, false)
	if !ok || got.scopes != s || !got.expires.Equal(now.Add(time.Hour)) || !got.issued.Equal(now) {
		t.Fatalf("verify %+v %v", got, ok)
	}
	if tok == newOAuthToken(s, now, time.Hour).sign(testSecret, accessPrefix) {
		t.Error("two tokens of the same second are equal (no nonce)")
	}
	raw, _ := tokenB64.DecodeString(body)
	badVersion := append([]byte{2}, raw[1:]...)
	code := newOAuthToken(s, now, codeTTL)
	code.binding = codeBinding("ch", "r", oauthClientID)
	signedCode := code.sign(testSecret, "")
	for name, c := range map[string]struct {
		secret, prefix, tok string
		binding             bool
	}{
		"other secret":   {"other", accessPrefix, tok, false},
		"empty secret":   {"", accessPrefix, tok, false},
		"refresh prefix": {testSecret, refreshPrefix, tok, false},
		"not base64":     {testSecret, accessPrefix, accessPrefix + "!!!", false},
		"cut":            {testSecret, accessPrefix, tok[:len(tok)-2], false},
		"version":        {testSecret, accessPrefix, accessPrefix + tokenB64.EncodeToString(badVersion), false},
		"code as token":  {testSecret, accessPrefix, accessPrefix + signedCode, false},
		"token as code":  {testSecret, "", body, true},
	} {
		if _, ok := verifyOAuth(c.secret, c.prefix, c.tok, c.binding); ok {
			t.Errorf("%s verified", name)
		}
	}
	if got, ok := verifyOAuth(testSecret, "", signedCode, true); !ok || string(got.binding) != string(codeBinding("ch", "r", oauthClientID)) {
		t.Errorf("code: %+v %v", got, ok)
	}
	if pkceChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk") != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" { // RFC 7636, appendix B
		t.Error("S256 is not RFC 7636's")
	}
	for _, bad := range []string{"", "user:root", "user:profile nope"} {
		if _, ok := parseScopes(bad); ok {
			t.Errorf("scope %q parsed", bad)
		}
	}
}

// TestCheckToken: the refusals CheckToken adds to the engine's (spec 006
// 16.2.1), the driver alone.
func TestCheckToken(t *testing.T) {
	d := newOAuthDriver(t, map[string]string{"oauth_key": "sub"})
	now := time.Unix(1_800_000_000, 0)
	all, _ := parseScopes(strings.Join(scopeNames[:], " "))
	profileOnly, _ := parseScopes(scopeProfile)
	valid := newOAuthToken(all, now, time.Hour).sign(testSecret, accessPrefix)
	expired := newOAuthToken(all, now.Add(-2*time.Hour), time.Hour).sign(testSecret, accessPrefix)
	narrow := newOAuthToken(profileOnly, now, time.Hour).sign(testSecret, accessPrefix)
	beta := driver.MapHeaders{"anthropic-beta": "claude-code-20250219, oauth-2025-04-20"}
	if label, ok := d.ResolveKey(valid, testKeys, now); !ok || label != "sub" {
		t.Fatalf("resolve %q %v", label, ok)
	}
	for _, tok := range []string{expired, "sk-ant-oat01-x"} {
		if _, ok := d.ResolveKey(tok, testKeys, now); ok {
			t.Errorf("resolved %q", tok)
		}
	}
	msg := func(e *core.Error) string {
		if e == nil {
			return "<nil>"
		}
		return string(e.Kind) + ": " + e.Message
	}
	for _, c := range []struct {
		name string
		kind driver.Kind
		cred driver.Credential
		h    driver.MapHeaders
		want string
	}{
		{"api key", driver.KindMessages, driver.Credential{Key: "sk-ant-api03-x", Scheme: "x-api-key"}, beta, "<nil>"},
		{"other bearer", driver.KindMessages, driver.Credential{Key: "sk-bad", Scheme: "bearer"}, beta, "<nil>"},
		{"invalid", driver.KindMessages, driver.Credential{Key: "sk-ant-oat01-x", Scheme: "bearer"}, beta, "invalid_api_key: " + msgTokenInvalid},
		{"refresh", driver.KindMessages, driver.Credential{Key: "sk-ant-ort01-x", Scheme: "bearer"}, beta, "invalid_api_key: " + msgTokenInvalid},
		{"expired", driver.KindMessages, driver.Credential{Key: expired, Scheme: "bearer"}, beta, "invalid_api_key: " + msgTokenExpired},
		{"token in x-api-key", driver.KindMessages, driver.Credential{Key: valid, Scheme: "x-api-key", KeyLabel: "sub"}, beta, "invalid_api_key: "},
		{"account route", driver.KindUsage, driver.Credential{Key: valid, Scheme: "bearer", KeyLabel: "sub"}, nil, "<nil>"},
		{"no beta", driver.KindCountTokens, driver.Credential{Key: valid, Scheme: "bearer", KeyLabel: "sub"}, nil, "invalid_api_key: " + msgNoBeta},
		{"no scope", driver.KindModel, driver.Credential{Key: narrow, Scheme: "bearer", KeyLabel: "sub"}, beta, "permission: " + msgScopePrefix + scopeInference},
		{"valid", driver.KindModels, driver.Credential{Key: valid, Scheme: "bearer", KeyLabel: "sub"}, beta, "<nil>"},
	} {
		if got := msg(d.CheckToken(c.kind, c.cred, c.h, testKeys, now)); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// Without oauth_key nothing verifies: every OAuth token is invalid.
	off := newOAuthDriver(t, nil)
	if got := msg(off.CheckToken(driver.KindMessages, driver.Credential{Key: valid, Scheme: "bearer"}, beta, testKeys, now)); got != "invalid_api_key: "+msgTokenInvalid {
		t.Errorf("without oauth_key: %s", got)
	}
	if off.oauthEntry(nil) != nil || d.oauthEntry(nil) != nil || off.oauthPlan != "max_5x" {
		t.Error("entry without a lookup or option, or the default plan")
	}
	if !d.Subscription(driver.KindMessages, driver.Credential{Key: valid, Scheme: "bearer"}) ||
		d.Subscription(driver.KindCountTokens, driver.Credential{Key: valid, Scheme: "bearer"}) ||
		d.Subscription(driver.KindMessages, driver.Credential{Key: valid, Scheme: "x-api-key"}) {
		t.Error("Subscription")
	}
}

// TestOAuthDriverSurface: the routes, public routes and Extra with and
// without oauth_key, the plans, the profile of an unknown key under
// server.auth: none and the options in the configuration (spec 003).
func TestOAuthDriverSurface(t *testing.T) {
	off := newOAuthDriver(t, nil)
	on := newOAuthDriver(t, map[string]string{"oauth_key": "sub", "oauth_plan": "pro"})
	// 4 inference routes and /api/hello on 7 methods (task 449), then the
	// 11 of the subscription.
	if len(off.Routes()) != 4+7 || len(on.Routes()) != 4+7+11 {
		t.Fatalf("routes %d %d", len(off.Routes()), len(on.Routes()))
	}
	for _, r := range off.Routes()[4:] {
		if !isHello(r) || !off.PublicRoute(r) || !on.PublicRoute(r) {
			t.Errorf("route %+v: /api/hello public with and without oauth_key", r)
		}
	}
	for _, r := range on.Routes()[4+7:] {
		if r.Kind != driver.KindUsage || on.PublicRoute(r) != (r.Path != pathCLIProfile) || off.PublicRoute(r) {
			t.Errorf("route %+v public %v", r, on.PublicRoute(r))
		}
	}
	if on.PublicRoute(driver.Route{Method: "POST", Path: "/v1/messages", Kind: driver.KindMessages}) {
		t.Error("messages public")
	}
	w := core.NewMemWriter()
	off.Extra(driver.KindUsage, w, driver.MapHeaders{}, nil, driver.ExtraContext{})
	if w.StatusCode() != 404 {
		t.Errorf("Extra without oauth_key: %d", w.StatusCode())
	}
	h := driver.MapHeaders{}
	driver.SetRoute(h, "GET", pathCLIProfile)
	w = core.NewMemWriter()
	on.Extra(driver.KindUsage, w, h, nil, driver.ExtraContext{})
	if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), `"email":"anonymous@wwz.invalid"`) || !strings.Contains(string(w.Body()), `"organization_type":"claude_pro","rate_limit_tier":"default_claude_ai"`) {
		t.Errorf("anonymous profile: %d %s", w.StatusCode(), w.Body())
	}
	for plan, want := range map[string]string{"max_5x": "claude_max", "max_20x": "claude_max", "team": "claude_team", "enterprise": "claude_enterprise", "nope": "claude_max"} {
		if got := newOAuthDriver(t, map[string]string{"oauth_plan": plan}).identity("a", "b").orgType; got != want {
			t.Errorf("plan %s: %s", plan, got)
		}
	}
	if u := shaUUID("x"); len(u) != 36 || u[14] != '4' || !strings.ContainsAny(u[19:20], "89ab") {
		t.Errorf("uuid %s", u)
	}
	base := "keys: [{key: k1, label: sub}]\ndrivers:\n  - name: anthropic\n    options: "
	for opts, want := range map[string]string{
		"{oauth_key: sub, oauth_plan: max_20x}": "",
		"{oauth_key: nobody}":                   `option oauth_key="nobody" is invalid`,
		"{oauth_key: sub, oauth_plan: free}":    `option oauth_plan="free" is invalid`,
	} {
		_, err := config.Parse([]byte(base + opts + "\n"))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", opts, err)
		}
	}
}
