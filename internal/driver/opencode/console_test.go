package opencode_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Task 424 (ADR 0072, item 17; spec 004 "OpenCode: modo public e
// Console"): the Console under <prefix>/console, through the engine, with
// a controlled clock. Shapes and times are the wwz's (not conferred). The
// secrets are test keys of the configuration, never real ones.

// consoleCfg is billCfg with the Console approving go-key (account acme).
const consoleCfg = billCfg + `
drivers:
  - {name: opencode, options: {console_key: go-key}}
`

var consoleOpts = map[string]string{"console_key": "go-key"}

// t0 is the clock of the Console tests.
var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// clock is a settable engine clock.
type clock struct{ now time.Time }

func (c *clock) at(d time.Duration) { c.now = t0.Add(d) }

func newConsole(t *testing.T, cfg string, opts map[string]string) (*billEngine, *clock) {
	t.Helper()
	b := newBillEngine(t, cfg, opts)
	c := &clock{now: t0}
	b.e.Now = func() time.Time { return c.now }
	return b, c
}

// consoleJSON decodes a Console answer, checking its status and type.
func consoleJSON(t *testing.T, w *core.MemWriter, status int) map[string]any {
	t.Helper()
	if w.StatusCode() != status {
		t.Fatalf("status %d, want %d: %s", w.StatusCode(), status, w.Body())
	}
	if ct := w.Headers()["Content-Type"]; ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body(), &m); err != nil {
		t.Fatalf("%s: %v", w.Body(), err)
	}
	return m
}

func (b *billEngine) deviceCode(t *testing.T) map[string]any {
	t.Helper()
	w := b.do(t, "POST", "/console/auth/device/code", map[string]string{driver.OriginHeader: "http://wwz.test", "content-type": "application/json"}, `{"client_id":"opencode-cli","supports_org_scope":true}`)
	return consoleJSON(t, w, 200)
}

// poll posts the device code grant, as JSON or as a form.
func (b *billEngine) poll(t *testing.T, code string, form bool) *core.MemWriter {
	t.Helper()
	if form {
		return b.do(t, "POST", "/console/auth/device/token", map[string]string{"content-type": "application/x-www-form-urlencoded"},
			"grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Adevice_code&device_code="+code)
	}
	return b.do(t, "POST", "/console/auth/device/token", map[string]string{"content-type": "application/json"},
		`{"grant_type":"urn:ietf:params:oauth:grant-type:device_code","device_code":"`+code+`"}`)
}

func (b *billEngine) refresh(t *testing.T, tok string) *core.MemWriter {
	t.Helper()
	return b.do(t, "POST", "/console/auth/device/token", nil, `{"grant_type":"refresh_token","refresh_token":"`+tok+`"}`)
}

// api is a GET of a Console api/* route with bearer ("" sends none).
func (b *billEngine) api(t *testing.T, path, bearer string) *core.MemWriter {
	t.Helper()
	hdr := map[string]string{"x-org-id": "wrk_ignored"}
	if bearer != "" {
		hdr["authorization"] = "Bearer " + bearer
	}
	return b.do(t, "GET", "/console/api/"+path, hdr, "")
}

func wantOAuthError(t *testing.T, name string, w *core.MemWriter, code string) {
	t.Helper()
	if w.StatusCode() != 400 || string(w.Body()) != `{"error":"`+code+`"}` {
		t.Errorf("%s: %d %s, want 400 %s", name, w.StatusCode(), w.Body(), code)
	}
}

// login runs the device flow to the tokens at t0+11s.
func login(t *testing.T, b *billEngine, c *clock) map[string]any {
	t.Helper()
	c.at(0)
	code := b.deviceCode(t)["device_code"].(string)
	c.at(11 * time.Second)
	return consoleJSON(t, b.poll(t, code, false), 200)
}

func TestConsoleDeviceFlow(t *testing.T) {
	b, c := newConsole(t, consoleCfg, consoleOpts)
	dc := b.deviceCode(t)
	code, _ := dc["device_code"].(string)
	user, _ := dc["user_code"].(string)
	if !regexp.MustCompile(`^[A-Z]{4}-[A-Z]{4}$`).MatchString(user) || code == "" {
		t.Fatalf("device code: %v", dc)
	}
	if dc["verification_uri_complete"] != "http://wwz.test/console/device?code="+user || dc["expires_in"] != 900.0 || dc["interval"] != 5.0 {
		t.Fatalf("device code: %v", dc)
	}

	steps := []struct {
		at   time.Duration
		want string
	}{{0, "slow_down"}, {4 * time.Second, "slow_down"}, {5 * time.Second, "authorization_pending"}, {9 * time.Second, "authorization_pending"}}
	for i, s := range steps {
		c.at(s.at)
		wantOAuthError(t, s.want, b.poll(t, code, i%2 == 1), s.want)
	}
	c.at(10 * time.Second)
	tok := consoleJSON(t, b.poll(t, code, true), 200)
	if tok["token_type"] != "Bearer" || tok["expires_in"] != 3600.0 || tok["access_token"] == "" || tok["refresh_token"] == "" {
		t.Fatalf("tokens: %v", tok)
	}
	orgs := b.api(t, "orgs", "oc_sk_go")
	var list []map[string]any
	if err := json.Unmarshal(orgs.Body(), &list); err != nil || len(list) != 1 || list[0]["id"] != tok["org_id"] || list[0]["name"] != "acme" {
		t.Fatalf("org_id %v, orgs %s", tok["org_id"], orgs.Body())
	}
	if !regexp.MustCompile(`^wrk_[0-9A-Z]{26}$`).MatchString(tok["org_id"].(string)) {
		t.Fatalf("org_id %v", tok["org_id"])
	}
	c.at(900 * time.Second)
	wantOAuthError(t, "expired", b.poll(t, code, false), "expired_token")

	// A tampered code, a missing one and an unknown grant.
	wantOAuthError(t, "tampered", b.poll(t, "x"+code, false), "invalid_grant")
	wantOAuthError(t, "no signature", b.poll(t, "abc", false), "invalid_grant")
	wantOAuthError(t, "missing", b.poll(t, "", false), "invalid_request")
	wantOAuthError(t, "refresh missing", b.refresh(t, ""), "invalid_request")
	wantOAuthError(t, "grant", b.do(t, "POST", "/console/auth/device/token", nil, `grant_type=password`), "unsupported_grant_type")
	wantOAuthError(t, "not json", b.do(t, "POST", "/console/auth/device/token", map[string]string{"content-type": "application/json"}, `{`), "unsupported_grant_type")
}

// TestConsoleWithoutKey: without console_key the flow never approves and
// expires; no refresh is valid.
func TestConsoleWithoutKey(t *testing.T) {
	b, c := newConsole(t, billCfg, nil)
	code := b.deviceCode(t)["device_code"].(string)
	c.at(2 * time.Second)
	wantOAuthError(t, "early", b.poll(t, code, false), "slow_down")
	c.at(11 * time.Second)
	wantOAuthError(t, "never approved", b.poll(t, code, false), "authorization_pending")
	c.at(899 * time.Second)
	wantOAuthError(t, "never approved", b.poll(t, code, false), "authorization_pending")
	c.at(900 * time.Second)
	wantOAuthError(t, "expired", b.poll(t, code, false), "expired_token")

	// Tokens of a configuration with console_key mean nothing here.
	other, oc := newConsole(t, consoleCfg, consoleOpts)
	tok := login(t, other, oc)
	wantOAuthError(t, "refresh", b.refresh(t, tok["refresh_token"].(string)), "invalid_grant")
	if w := b.api(t, "user", tok["access_token"].(string)); w.StatusCode() != 401 {
		t.Errorf("access token without console_key: %d", w.StatusCode())
	}
}

func TestConsoleRefresh(t *testing.T) {
	b, c := newConsole(t, consoleCfg, consoleOpts)
	tok := login(t, b, c)
	c.at(time.Hour)
	fresh := consoleJSON(t, b.refresh(t, tok["refresh_token"].(string)), 200)
	if fresh["access_token"] == tok["access_token"] || fresh["refresh_token"] == tok["refresh_token"] || fresh["org_id"] != tok["org_id"] {
		t.Fatalf("refresh: %v after %v", fresh, tok)
	}
	// The access token is not a refresh token, nor the reverse.
	wantOAuthError(t, "access as refresh", b.refresh(t, tok["access_token"].(string)), "invalid_grant")
	if w := b.api(t, "user", tok["refresh_token"].(string)); w.StatusCode() != 401 {
		t.Errorf("refresh token as bearer: %d", w.StatusCode())
	}
	// Tampered: one character of the payload, then of the signature.
	rt := tok["refresh_token"].(string)
	payload, sig, _ := strings.Cut(rt, ".")
	wantOAuthError(t, "tampered payload", b.refresh(t, flip(payload)+"."+sig), "invalid_grant")
	wantOAuthError(t, "tampered signature", b.refresh(t, payload+"."+flip(sig)), "invalid_grant")
	// Expired after 30 days.
	c.at(30*24*time.Hour + 11*time.Second)
	wantOAuthError(t, "expired", b.refresh(t, rt), "invalid_grant")

	// Another secret: the same label with another key value.
	changed, cc := newConsole(t, strings.Replace(consoleCfg, "{key: oc_sk_go, label: go-key", "{key: oc_sk_go_rotated, label: go-key", 1), consoleOpts)
	cc.at(time.Hour)
	wantOAuthError(t, "other secret", changed.refresh(t, rt), "invalid_grant")
	if w := changed.chat(t, "/inference/go", tok["access_token"].(string), "deepseek-v4.1-flash", false); w.StatusCode() != 401 {
		t.Errorf("access token under another secret: %d %s", w.StatusCode(), w.Body())
	}
}

// flip changes the first character of s.
func flip(s string) string {
	if s[0] == 'A' {
		return "B" + s[1:]
	}
	return "A" + s[1:]
}

// TestConsoleAccessTokenInference: the access token stands for the
// console_key on inference (Chat and Messages) until it expires.
func TestConsoleAccessTokenInference(t *testing.T) {
	b, c := newConsole(t, consoleCfg, consoleOpts)
	access := login(t, b, c)["access_token"].(string)
	c.at(time.Minute)
	if w := b.chat(t, "/inference/go", access, "deepseek-v4.1-flash", false); w.StatusCode() != 200 {
		t.Fatalf("chat with the access token: %d %s", w.StatusCode(), w.Body())
	}
	msg := b.do(t, "POST", "/inference/go/anthropic/v1/messages", map[string]string{"authorization": "Bearer " + access, "x-opencode-session": "ses_1"},
		`{"model":"deepseek-v4.1-flash","max_tokens":16,"messages":[{"role":"user","content":"oi"}]}`)
	if msg.StatusCode() != 200 {
		t.Fatalf("messages with the access token: %d %s", msg.StatusCode(), msg.Body())
	}
	// The token is the console_key's account: the quota usage is acme's.
	if w := b.do(t, "GET", "/inference/go/v1/usage", map[string]string{"authorization": "Bearer " + access}, ""); w.StatusCode() != 200 {
		t.Fatalf("usage with the access token: %d %s", w.StatusCode(), w.Body())
	}
	user := consoleJSON(t, b.api(t, "user", access), 200)
	if user["email"] != "go-key@wwz.invalid" || !regexp.MustCompile(`^usr_[0-9A-Z]{26}$`).MatchString(user["id"].(string)) {
		t.Fatalf("user: %v", user)
	}
	// Expired after an hour: an invalid credential.
	c.at(time.Hour + 11*time.Second)
	w := b.chat(t, "/inference/go", access, "deepseek-v4.1-flash", false)
	if w.StatusCode() != 401 || !strings.Contains(string(w.Body()), "Invalid credential") {
		t.Fatalf("expired access token: %d %s", w.StatusCode(), w.Body())
	}
}

// TestConsoleTwoInstances: two engines with the same configuration accept
// each other's device codes and tokens.
func TestConsoleTwoInstances(t *testing.T) {
	a, ca := newConsole(t, consoleCfg, consoleOpts)
	b, cb := newConsole(t, consoleCfg, consoleOpts)
	code := a.deviceCode(t)["device_code"].(string)
	cb.at(11 * time.Second)
	tok := consoleJSON(t, b.poll(t, code, false), 200)
	ca.at(time.Minute)
	if w := a.chat(t, "/inference/go", tok["access_token"].(string), "deepseek-v4.1-flash", false); w.StatusCode() != 200 {
		t.Fatalf("a with b's token: %d %s", w.StatusCode(), w.Body())
	}
	fresh := consoleJSON(t, a.refresh(t, tok["refresh_token"].(string)), 200)
	cb.at(time.Minute)
	if w := b.api(t, "user", fresh["access_token"].(string)); w.StatusCode() != 200 {
		t.Fatalf("b with a's refreshed token: %d %s", w.StatusCode(), w.Body())
	}
}

func TestConsoleAPIAuth(t *testing.T) {
	b, _ := newConsole(t, consoleCfg, consoleOpts)
	for _, bearer := range []string{"", "oc_sk_nope", "public", "a.b"} {
		for _, path := range []string{"user", "orgs", "v2/config", "policies/current-budgets"} {
			w := b.api(t, path, bearer)
			if m := consoleJSON(t, w, 401); m["_tag"] != "Unauthorized" || len(m) != 1 {
				t.Errorf("%s with %q: %s", path, bearer, w.Body())
			}
		}
	}
	// A suspended key is refused too.
	if w := b.api(t, "user", "oc_sk_susp"); w.StatusCode() != 401 {
		t.Errorf("suspended: %d", w.StatusCode())
	}
	// Any key of keys[] is accepted, not only console_key's.
	user := consoleJSON(t, b.api(t, "user", "oc_sk_plus"), 200)
	if user["email"] != "plus-key@wwz.invalid" {
		t.Errorf("user: %v", user)
	}
}

func TestConsoleConfig(t *testing.T) {
	b, _ := newConsole(t, consoleCfg, consoleOpts)
	if w := b.api(t, "v2/config", "oc_sk_go"); string(w.Body()) != `{"providers":{"opencode":{},"opencode-go":{}}}` {
		t.Errorf("go: %s", w.Body())
	}
	if w := b.api(t, "v2/config", "oc_sk_nogo"); string(w.Body()) != `{"providers":{"opencode":{}}}` {
		t.Errorf("plan none: %s", w.Body())
	}
}

// budgetCfg: budgets of coverage all and models on the account, the
// member (project) and the key.
const budgetCfg = `
server: {seed: 42, auth: keys}
tiers:
  bud:
    limits: {credits_usd: 10, spend_monthly_usd: 100, spend_monthly_models_usd: {glm-one: 20}}
accounts:
  - {id: org, tier: bud}
projects:
  - {id: alice, limits: {spend_monthly_usd: 50}}
keys:
  - {key: oc_sk_b, label: b-key, account: org, project: alice, limits: {spend_monthly_usd: 10, spend_monthly_models_usd: {opencode-go/glm-one: 3}}}
  - {key: oc_sk_svc, label: svc-key, account: org, service_account: true}
models:
  - {id: glm-one, protocols: [openai], drivers: [opencode], pricing: {input_per_m: 1, output_per_m: 2}}
`

func TestConsoleCurrentBudgets(t *testing.T) {
	b, _ := newConsole(t, budgetCfg, nil)
	w := b.api(t, "policies/current-budgets", "oc_sk_b")
	if w.StatusCode() != 200 {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
	var rules []map[string]any
	if err := json.Unmarshal(w.Body(), &rules); err != nil {
		t.Fatal(err)
	}
	type want struct {
		limit           float64
		scope, coverage string
		models          string
		picked, shared  bool
	}
	wants := []want{
		{100, "workspace", "all", "", false, true},
		{20, "workspace", "models", "opencode/glm-one,opencode-go/glm-one", false, true},
		{50, "member", "all", "", false, false},
		{10, "key", "all", "", false, false},
		{3, "key", "models", "opencode-go/glm-one", true, false},
	}
	if len(rules) != len(wants) {
		t.Fatalf("rules: %s", w.Body())
	}
	ids := map[string]bool{}
	for i, r := range rules {
		wt := wants[i]
		var models []string
		for _, m := range r["models"].([]any) {
			models = append(models, m.(string))
		}
		if r["limitDollars"] != wt.limit || r["spentDollars"] != 0.0 || r["scope"] != wt.scope || r["coverage"] != wt.coverage ||
			strings.Join(models, ",") != wt.models || r["picked"] != wt.picked || r["shared"] != wt.shared ||
			r["per"] != "month" || len(r["vendors"].([]any)) != 0 || r["resetsAt"] != "2026-12-01T00:00:00Z" {
			t.Errorf("rule %d: %v, want %+v", i, r, wt)
		}
		id, _ := r["ruleID"].(string)
		if !strings.HasPrefix(id, "rul_") || ids[id] {
			t.Errorf("rule %d: ruleID %q", i, id)
		}
		ids[id] = true
	}
	// A service account has no access to the budgets.
	if w := b.api(t, "policies/current-budgets", "oc_sk_svc"); string(w.Body()) != `{"_tag":"Forbidden"}` || w.StatusCode() != 403 {
		t.Errorf("service account: %d %s", w.StatusCode(), w.Body())
	}
}

// TestConsoleRouteMiss: a wrong method or an unknown path under /console
// is the site's HTML 404, as every route the gateway does not serve; the
// device flow is public, api/* is not.
func TestConsoleRouteMiss(t *testing.T) {
	d, err := driver.New("opencode", consoleOpts)
	if err != nil {
		t.Fatal(err)
	}
	rm, _ := driver.As[driver.RouteMisser](d)
	for _, p := range []string{"/console/nope", "/console/auth/device/code"} {
		w := core.NewMemWriter()
		rm.WriteRouteMiss(w, nil, driver.RouteMiss{Method: "GET", Path: p}, nil)
		if w.StatusCode() != 404 || w.Headers()["Content-Type"] != "text/html" || !strings.Contains(string(w.Body()), p) {
			t.Errorf("%s: %d %v", p, w.StatusCode(), w.Headers())
		}
	}
	for _, r := range []struct {
		method, path string
		public       bool
	}{
		{"POST", "/console/auth/device/code", true},
		{"POST", "/console/auth/device/token", true},
		{"GET", "/console/auth/device/code", false},
		{"GET", "/console/api/user", false},
	} {
		if got := driver.IsPublicRoute(d, driver.Route{Method: r.method, Path: r.path}); got != r.public {
			t.Errorf("%s %s public = %v", r.method, r.path, got)
		}
	}
	// Extra on a Console path the driver does not route answers nothing
	// but the 401 of api/* without a key.
	w := core.NewMemWriter()
	h := driver.MapHeaders{}
	driver.SetRoute(h, "GET", "/console/api/other")
	d.Extra(driver.KindUsage, w, h, nil, driver.ExtraContext{})
	if w.StatusCode() != 401 {
		t.Errorf("unrouted console path: %d", w.StatusCode())
	}
	w = core.NewMemWriter()
	d.Extra(driver.KindUsage, w, h, nil, driver.ExtraContext{Key: &config.KeyConfig{Label: "k", Account: "a"}})
	if w.StatusCode() != 404 || w.Headers()["Content-Type"] != "text/html" {
		t.Errorf("unrouted console path with a key: %d", w.StatusCode())
	}
}

// TestConsoleSiteURL: verification_uri_complete is the origin the server
// computed (driver.OriginHeader, ADR 0073) + /console/device; Host and
// X-Forwarded-* are not read, and without an origin the URL is relative.
func TestConsoleSiteURL(t *testing.T) {
	b, c0 := newConsole(t, consoleCfg, consoleOpts)
	for _, c := range []struct {
		h    map[string]string
		want string
	}{
		{map[string]string{driver.OriginHeader: "https://wwz.test:8443/opencode"}, "https://wwz.test:8443/opencode/console/device?code="},
		{map[string]string{"x-forwarded-proto": "https", "x-forwarded-host": "evil", "host": "wwz.test"}, "/console/device?code="},
	} {
		w := b.do(t, "POST", "/console/auth/device/code", c.h, `{}`)
		dc := consoleJSON(t, w, 200)
		if got := dc["verification_uri_complete"].(string); !strings.HasPrefix(got, c.want) || len(got) != len(c.want)+9 {
			t.Errorf("%v: %v", c.h, got)
		}
	}
	// A token signed with the secret whose payload is not base64 or not
	// JSON never verifies; neither does a signature that is not base64.
	c0.at(time.Minute)
	for _, payload := range []string{"!!", base64.RawURLEncoding.EncodeToString([]byte("not json"))} {
		m := hmac.New(sha256.New, []byte("oc_sk_go")) // the test key of consoleCfg
		m.Write([]byte("wwz-console." + payload))
		wantOAuthError(t, "bad payload", b.refresh(t, payload+"."+base64.RawURLEncoding.EncodeToString(m.Sum(nil))), "invalid_grant")
	}
	wantOAuthError(t, "bad signature", b.refresh(t, "abc.!!"), "invalid_grant")
}
