package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// The strict client verification over TCP (spec 020, ADR 0074): the
// anthropic driver with client_verification: claude-code at the root and
// the same driver with off at /off, the subscription of the key
// "assinante", the API key of the captures (one request a minute short of
// what the test sends, so a refusal that counted would show), the fake
// key of the official SDKs and the models the captures name.
const strictCfg = `
server: {auth: keys, seed: 7}
store: {kind: memory}
drivers:
  - {name: anthropic, options: {oauth_key: assinante, client_verification: claude-code}}
  - {name: anthropic, prefix: /off, options: {oauth_key: assinante}}
keys:
  - {key: wwz-oauth-secret-0001, label: assinante}
  - {key: CCKEY, label: cc, limits: {rpm: 4}}
  - {key: sk-ant-api03-WWZFAKE, label: sdk}
profiles:
  latency: {zero: {}}
  generation:
    text: {mode: lorem, output_tokens: {dist: fixed, value: 8}}
models:
  - {id: claude-sonnet-5-5, latency: zero, generation: text}
  - {id: claude-opus-5-5, latency: zero, generation: text}
`

// strictServer serves strictCfg on loopback TCP; ccKey is the API key of
// the captures.
func strictServer(t *testing.T, ccKey string) (*engine.Engine, string) {
	t.Helper()
	eng := newEngine(t, strings.Replace(strictCfg, "CCKEY", ccKey, 1))
	s, err := New(eng, Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	return eng, ln.Addr().String()
}

// strictAnswer is one response read off the connection.
type strictAnswer struct {
	status int
	header map[string]string // lower-cased names
	body   string
}

// strictDo writes raw on a new connection and reads the response (no body
// for HEAD).
func strictDo(t *testing.T, addr, raw string) strictAnswer {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	var resp fasthttp.Response
	resp.SkipBody = strings.HasPrefix(raw, "HEAD ")
	if err := resp.Read(bufio.NewReader(c)); err != nil {
		t.Fatal(err)
	}
	a := strictAnswer{status: resp.StatusCode(), header: map[string]string{}, body: string(resp.Body())}
	resp.Header.VisitAll(func(k, v []byte) { a.header[strings.ToLower(string(k))] = string(v) })
	return a
}

// strictLogin is Claude Code's subscription login against the strict
// mount (the OAuth routes are not verified): authorize with PKCE S256,
// then the authorization code grant; it returns the access token.
func strictLogin(t *testing.T, addr string) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	const client, redirect = "9d1c250a-e61b-44d9-88ed-5944d1962f5e", "http://localhost:54545/callback"
	q := url.Values{
		"code": {"true"}, "client_id": {client}, "response_type": {"code"}, "redirect_uri": {redirect},
		"scope":          {"user:profile user:inference user:sessions:claude_code"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "state": {"st4te"},
	}
	cl := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := cl.Get("http://" + addr + "/cai/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	loc, err := url.Parse(r.Header.Get("Location"))
	if err != nil || r.StatusCode != 302 || loc.Query().Get("code") == "" {
		t.Fatalf("authorize: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	body, _ := json.Marshal(map[string]string{
		"grant_type": "authorization_code", "code": loc.Query().Get("code"), "redirect_uri": redirect,
		"client_id": client, "code_verifier": verifier, "state": "st4te",
	})
	r, err = cl.Post("http://"+addr+"/v1/oauth/token", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var tk struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&tk); err != nil || r.StatusCode != 200 || !strings.HasPrefix(tk.AccessToken, "sk-ant-oat01-") {
		t.Fatalf("token: %d %v %q", r.StatusCode, err, tk.AccessToken)
	}
	return tk.AccessToken
}

// ccRequest is a raw request of the captures: its head lines (request
// line first) and body; String sends it with Content-Length recomputed
// when it has one.
type ccRequest struct {
	lines []string
	body  string
}

func parseCC(t *testing.T, raw string) ccRequest {
	t.Helper()
	head, body, ok := strings.Cut(raw, "\r\n\r\n")
	if !ok {
		t.Fatal("no end of head")
	}
	return ccRequest{lines: strings.Split(head, "\r\n"), body: body}
}

func (r ccRequest) String() string {
	lines := append([]string(nil), r.lines...)
	for i, l := range lines {
		if strings.HasPrefix(strings.ToLower(l), "content-length:") {
			name, _, _ := strings.Cut(l, ":")
			lines[i] = name + ": " + strconv.Itoa(len(r.body))
		}
	}
	return strings.Join(lines, "\r\n") + "\r\n\r\n" + r.body
}

// index is the head line of header name (exact spelling), -1 without it.
func (r ccRequest) index(name string) int {
	for i, l := range r.lines[1:] {
		if strings.HasPrefix(l, name+":") {
			return i + 1
		}
	}
	return -1
}

func (r ccRequest) clone() ccRequest {
	return ccRequest{lines: append([]string(nil), r.lines...), body: r.body}
}

// set replaces the value of header name.
func (r ccRequest) set(t *testing.T, name, value string) ccRequest {
	t.Helper()
	i := r.index(name)
	if i < 0 {
		t.Fatalf("no %s", name)
	}
	c := r.clone()
	c.lines[i] = name + ": " + value
	return c
}

// remove drops header name.
func (r ccRequest) remove(t *testing.T, name string) ccRequest {
	t.Helper()
	i := r.index(name)
	if i < 0 {
		t.Fatalf("no %s", name)
	}
	c := r.clone()
	c.lines = append(c.lines[:i], c.lines[i+1:]...)
	return c
}

// insert puts a header line at head line at.
func (r ccRequest) insert(at int, line string) ccRequest {
	c := r.clone()
	c.lines = append(c.lines[:at], append([]string{line}, c.lines[at:]...)...)
	return c
}

// line edits the request line.
func (r ccRequest) line(old, new string) ccRequest {
	c := r.clone()
	c.lines[0] = strings.Replace(c.lines[0], old, new, 1)
	return c
}

// edit replaces old by new once in the body.
func (r ccRequest) edit(t *testing.T, old, new string) ccRequest {
	t.Helper()
	if !strings.Contains(r.body, old) {
		t.Fatalf("no %q in the body", old)
	}
	c := r.clone()
	c.body = strings.Replace(c.body, old, new, 1)
	return c
}

// withToken swaps the fake OAuth token of a capture for token.
func (r ccRequest) withToken(t *testing.T, token string) ccRequest {
	if i := r.index("Authorization"); i >= 0 && strings.HasPrefix(r.lines[i], "Authorization: Bearer sk-ant-oat01-") {
		return r.set(t, "Authorization", "Bearer "+token)
	}
	return r
}

// strictFixtures reads testdata/real/claude-code/2.1.308/ by file name.
func strictFixtures(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("../../testdata/real/claude-code/2.1.308/*.raw")
	if err != nil || len(files) != 9 {
		t.Fatalf("%d fixtures: %v", len(files), err)
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = string(b)
	}
	return out
}

// sdkStrictHeads are the heads of messages.create of the official
// anthropic SDKs (Python 2.3.0, Node 1.3.0; spec 020, capture of
// 2026-11-03), with the bodies they serialised: neither adds ?beta=true.
// beta.messages.create adds anthropic-beta before the body's length.
var sdkStrictHeads = map[string][2]string{
	"python": {"POST /v1/messages HTTP/1.1\r\nHost: 127.0.0.1:18610\r\nAccept-Encoding: gzip, deflate\r\nConnection: keep-alive\r\n" +
		"x-stainless-timeout: 600\r\nx-stainless-retry-count: 0\r\nx-stainless-read-timeout: 600\r\nAccept: application/json\r\n" +
		"Content-Type: application/json\r\nUser-Agent: Anthropic/Python 2.3.0\r\nX-Stainless-Lang: python\r\n" +
		"X-Stainless-Package-Version: 2.3.0\r\nX-Stainless-OS: MacOS\r\nX-Stainless-Arch: arm64\r\nX-Stainless-Runtime: CPython\r\n" +
		"X-Stainless-Runtime-Version: 3.13.18\r\nAuthorization: Bearer sk-ant-api03-WWZFAKE\r\nX-Stainless-Async: false\r\n" +
		"anthropic-version: 2026-10-21\r\nContent-Length: 0\r\n\r\n" +
		`{"max_tokens":16,"messages":[{"role":"user","content":"Hello there, reply with ok"}],"model":"claude-sonnet-5-5"}`},
	"node": {"POST /v1/messages HTTP/1.1\r\nhost: 127.0.0.1:18610\r\nconnection: keep-alive\r\nAccept: application/json\r\n" +
		"User-Agent: Anthropic/JS 1.3.0\r\nX-Stainless-Retry-Count: 0\r\nX-Stainless-Timeout: 600\r\nX-Stainless-Lang: js\r\n" +
		"X-Stainless-Package-Version: 1.3.0\r\nX-Stainless-OS: MacOS\r\nX-Stainless-Arch: arm64\r\nX-Stainless-Runtime: node\r\n" +
		"X-Stainless-Runtime-Version: v26.8.1\r\nanthropic-version: 2026-10-21\r\nauthorization: Bearer sk-ant-api03-WWZFAKE\r\n" +
		"content-type: application/json\r\naccept-language: *\r\nsec-fetch-mode: cors\r\naccept-encoding: gzip, deflate\r\n" +
		"content-length: 0\r\n\r\n" +
		`{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"Hello there, reply with ok"}]}`},
}

var strictReqID = regexp.MustCompile(`^req_[0-9A-Za-z]+$`)

// checkForged asserts the refusal of spec 020 with reason.
func checkForged(t *testing.T, a strictAnswer, reason string) {
	t.Helper()
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(a.body), &env); err != nil {
		t.Fatalf("%d %s: %v", a.status, a.body, err)
	}
	const prefix = "Client verification failed: this request does not match Claude Code 2.1.308; the client is forging Claude Code requests ("
	if a.status != 403 || env.Type != "error" || env.Error.Type != "forged_request_error" ||
		!strings.HasPrefix(env.Error.Message, prefix+reason+": ") || !strings.HasSuffix(env.Error.Message, ").") ||
		!strictReqID.MatchString(env.RequestID) || a.header["request-id"] != env.RequestID {
		t.Errorf("want %s: %d %s", reason, a.status, a.body)
	}
	if a.header["x-wwz-client-verification"] != "failed; reason="+reason || a.header["x-should-retry"] != "false" ||
		a.header["content-type"] != "application/json" {
		t.Errorf("want %s: headers %v", reason, a.header)
	}
}

// TestClaudeCodeStrictOverTCP: with client_verification: claude-code each
// capture of Claude Code 2.1.308 sent raw over TCP gets its 200 (the OAuth
// ones with a token wwz issued; HEAD /api/hello on the public route, not
// verified), each mutation of the spec 020 table and the official SDKs a
// 403 forged_request_error with its reason, and a plain curl request too;
// the refusals count in wwz_client_verification_failures_total and
// wwz_requests_total{status="403"} and take nothing from the store (the
// key of the captures still has its four requests a minute). With off the
// same requests are served as before.
func TestClaudeCodeStrictOverTCP(t *testing.T) {
	fx := strictFixtures(t)
	f002, f004, f013 := parseCC(t, fx["002-sdk-cli-oauth-turn.raw"]), parseCC(t, fx["004-sdk-cli-apikey-turn.raw"]), parseCC(t, fx["013-sdk-cli-oauth-count-tokens.raw"])
	ccKey := strings.TrimPrefix(f004.lines[f004.index("x-api-key")], "x-api-key: ")
	eng, addr := strictServer(t, ccKey)
	token := strictLogin(t, addr)
	tok002 := f002.withToken(t, token)
	betas := strings.TrimPrefix(f002.lines[f002.index("anthropic-beta")], "anthropic-beta: ")
	const device = "68159782ff84a80342f7e86ed7cddfaaf32e783f7a2a3e29e09814638ebb1f79"
	const session = "ce90e450-43fc-4765-8110-6aed6d650ca1"
	chunked := tok002.clone()
	chunked.lines[chunked.index("Content-Length")] = "Transfer-Encoding: chunked"
	chunked.body = strconv.FormatInt(int64(len(f002.body)), 16) + "\r\n" + f002.body + "\r\n0\r\n\r\n"
	swapped := tok002.clone()
	i, j := swapped.index("X-Stainless-Arch"), swapped.index("X-Stainless-Lang")
	swapped.lines[i], swapped.lines[j] = swapped.lines[j], swapped.lines[i]
	apiOAuthBeta := tok002.remove(t, "Authorization")
	apiOAuthBeta = apiOAuthBeta.insert(apiOAuthBeta.index("anthropic-version")+1, "x-api-key: "+ccKey)

	mutations := []struct {
		name   string
		raw    string
		reason string
	}{
		// The table "Mutações de teste" of spec 020.
		{"without ?beta=true", tok002.line("?beta=true", "").String(), "query"},
		{"Transfer-Encoding instead of Content-Length", strings.Join(chunked.lines, "\r\n") + "\r\n\r\n" + chunked.body, "transfer_encoding"},
		{"without X-Stainless-Timeout", tok002.remove(t, "X-Stainless-Timeout").String(), "header_missing"},
		{"X-Stainless-Arch swapped with X-Stainless-Lang", swapped.String(), "header_order"},
		{"user-agent in lowercase", strings.Replace(tok002.String(), "\r\nUser-Agent: ", "\r\nuser-agent: ", 1), "header_order"},
		{"extra X-Forwarded-For", tok002.insert(tok002.index("Connection"), "X-Forwarded-For: 10.0.0.1").String(), "header_order"},
		{"User-Agent sdk-py", tok002.set(t, "User-Agent", "claude-cli/2.1.308 (external, sdk-py)").String(), "user_agent"},
		{"User-Agent 2.1.309", tok002.set(t, "User-Agent", "claude-cli/2.1.309 (external, sdk-cli)").String(), "unknown_version"},
		{"X-Stainless-Package-Version 0.129.0", tok002.set(t, "X-Stainless-Package-Version", "0.129.0").String(), "header_value"},
		{"betas out of order", tok002.set(t, "anthropic-beta", strings.Replace(betas, "claude-code-20250219,oauth-2025-04-20", "oauth-2025-04-20,claude-code-20250219", 1)).String(), "beta"},
		{"invented beta", tok002.set(t, "anthropic-beta", betas+",wwz-invented-2026-01-01").String(), "beta"},
		{"oauth beta with x-api-key", apiOAuthBeta.String(), "beta"},
		{"space after : in the body", tok002.edit(t, `"model":`, `"model": `).String(), "body_encoding"},
		{"< escaped", tok002.edit(t, "<total_tokens>", "\\"+"u003ctotal_tokens>").String(), "body_encoding"},
		{"stream before model", tok002.edit(t, `,"stream":true}`, `}`).edit(t, `{"model":`, `{"stream":true,"model":`).String(), "body_key_order"},
		{"device_id with 63 hex", tok002.edit(t, device, device[:63]).String(), "metadata"},
		{"session_id differs from the header", tok002.edit(t, `\"session_id\":\"`+session, `\"session_id\":\"`+session[:35]+"2").String(), "session_mismatch"},
		{"cch in the billing header", tok002.edit(t, "cc_entrypoint=sdk-cli;", "cc_entrypoint=sdk-cli; cch=00000;").String(), "billing_header"},
		{"cc_version 2.1.307", tok002.edit(t, "cc_version=2.1.308.9fe", "cc_version=2.1.307.9fe").String(), "billing_header"},
		{"fingerprint 000", tok002.edit(t, "cc_version=2.1.308.9fe", "cc_version=2.1.308.000").String(), "fingerprint"},
		{"cli identity in sdk-cli", tok002.edit(t, "You are a Claude agent, built on Anthropic's Claude Agent SDK.", "You are Claude Code, Anthropic's official CLI for Claude.").String(), "identity"},
		// The API key capture and count_tokens are verified the same way.
		{"API key capture without ?beta=true", f004.line("?beta=true", "").String(), "query"},
		{"API key capture with X-Stainless-OS Linux", f004.set(t, "X-Stainless-OS", "Plan9").String(), "header_value"},
		{"count_tokens with X-Stainless-Timeout", f013.withToken(t, token).insert(f013.index("anthropic-beta"), "X-Stainless-Timeout: 600").String(), "header_order"},
		// A plain curl request.
		{"curl", "POST /v1/messages HTTP/1.1\r\nHost: 127.0.0.1\r\nUser-Agent: curl/8.7.1\r\nAccept: */*\r\nx-api-key: " + ccKey +
			"\r\nanthropic-version: 2023-06-01\r\ncontent-type: application/json\r\nContent-Length: 0\r\n\r\n", "query"},
	}
	// The official SDKs: messages.create and beta.messages.create (query),
	// with ?beta=true forced (header_missing) and then with the missing
	// headers added (header_order).
	for name, sdk := range sdkStrictHeads {
		r := parseCC(t, sdk[0]+sdk[1])
		mutations = append(mutations, struct {
			name   string
			raw    string
			reason string
		}{name + " messages.create", r.String(), "query"},
			struct {
				name   string
				raw    string
				reason string
			}{name + " beta.messages.create", r.insert(len(r.lines)-1, "anthropic-beta: claude-code-20250219").String(), "query"})
		forced := r.line("/v1/messages ", "/v1/messages?beta=true ")
		mutations = append(mutations, struct {
			name   string
			raw    string
			reason string
		}{name + " with ?beta=true", forced.String(), "header_missing"})
		for _, h := range []string{"X-Claude-Code-Session-Id: " + session, "anthropic-beta: claude-code-20250219", "anthropic-dangerous-direct-browser-access: true", "x-app: cli"} {
			forced = forced.insert(len(forced.lines), h)
		}
		mutations = append(mutations, struct {
			name   string
			raw    string
			reason string
		}{name + " with the missing headers", forced.String(), "header_order"})
	}
	reasons := map[string]int64{}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) { checkForged(t, strictDo(t, addr, m.raw), m.reason) })
		reasons[m.reason]++
	}
	var total int64
	for reason, n := range reasons {
		if got := eng.M.ClientVerificationFailures.Value("anthropic", reason); got != n {
			t.Errorf("wwz_client_verification_failures_total{reason=%q} = %d, want %d", reason, got, n)
		}
		total += n
	}
	if got := eng.M.Requests.Value("anthropic", "", "messages", "403") + eng.M.Requests.Value("anthropic", "", "count_tokens", "403"); got != total {
		t.Errorf("wwz_requests_total{status=403} = %d, want %d", got, total)
	}
	evs, err := eng.Store.Events(context.Background(), 1)
	if err != nil || len(evs) != 1 || evs[0].Error != "forged_request" || evs[0].Fields["reason"] != "header_order" || evs[0].Fields["denied"] != nil {
		t.Errorf("last event %v %+v", err, evs)
	}

	// Every capture passes the strict mount, then the off mount: the key
	// of the captures spends exactly its four requests a minute.
	for _, mount := range []string{"", "/off"} {
		for name, raw := range fx {
			r := parseCC(t, raw).withToken(t, token)
			r.lines[0] = strings.Replace(r.lines[0], " /", " "+mount+"/", 1)
			t.Run(mount+"/"+name, func(t *testing.T) {
				a := strictDo(t, addr, r.String())
				if a.status != 200 || a.header["x-wwz-client-verification"] != "" {
					t.Errorf("%d %v %s", a.status, a.header, a.body)
				}
			})
		}
	}
	if a := strictDo(t, addr, f004.String()); a.status != 429 {
		t.Errorf("fifth request of the key: %d %s", a.status, a.body)
	}
	// With off, a forged request and the SDKs are served as before.
	for name, raw := range map[string]string{
		"without ?beta=true": tok002.line("POST /v1/messages?beta=true", "POST /off/v1/messages").String(),
		"python":             parseCC(t, sdkStrictHeads["python"][0]+sdkStrictHeads["python"][1]).line(" /", " /off/").String(),
		"node":               parseCC(t, sdkStrictHeads["node"][0]+sdkStrictHeads["node"][1]).line(" /", " /off/").String(),
	} {
		if a := strictDo(t, addr, raw); a.status != 200 {
			t.Errorf("off, %s: %d %s", name, a.status, a.body)
		}
	}
	if got := eng.M.ClientVerificationFailures.Total(); got != total {
		t.Errorf("refusals moved with off: %d, want %d", got, total)
	}
}

// TestClaudeCodeStrictNormalUseOverTCP: the shapes of the real Claude Code
// 2.1.308 in normal use (task 457), made from capture 002 with a token wwz
// issued, get their 200 on the strict mount: meta blocks (the attribution
// reminder, CLAUDE.md) before the prompt in the first user message, the
// flag betas inline-tools and advisor-tool, and a subagent (agent id,
// cc_is_subagent, 5-minute cache). A fingerprint of no block of the first
// user message is still refused.
func TestClaudeCodeStrictNormalUseOverTCP(t *testing.T) {
	fx := strictFixtures(t)
	f002 := parseCC(t, fx["002-sdk-cli-oauth-turn.raw"])
	_, addr := strictServer(t, "sk-ant-api03-unused")
	tok := f002.withToken(t, strictLogin(t, addr))
	betas := strings.TrimPrefix(tok.lines[tok.index("anthropic-beta")], "anthropic-beta: ")
	meta := tok.edit(t, `"content":"Hello there, reply with ok"`,
		`"content":[{"type":"text","text":"<system-reminder>Attribution</system-reminder>"},`+
			`{"type":"text","text":"<system-reminder>Contents of CLAUDE.md</system-reminder>"},`+
			`{"type":"text","text":"Hello there, reply with ok"}]`)
	sub := tok.insert(tok.index("x-app")+1, "x-claude-code-agent-id: a67195a5d9d363c76").
		set(t, "anthropic-beta", strings.Replace(betas, ",extended-cache-ttl-2025-04-11", "", 1)).
		edit(t, "cc_entrypoint=sdk-cli;", "cc_entrypoint=sdk-cli; cc_is_subagent=true;")
	sub.body = strings.ReplaceAll(sub.body, `"cache_control":{"type":"ephemeral","ttl":"1h"}`, `"cache_control":{"type":"ephemeral"}`)
	for name, raw := range map[string]string{
		"meta blocks before the prompt": meta.String(),
		"flag betas": tok.set(t, "anthropic-beta", strings.Replace(betas, ",effort-",
			",inline-tools-2026-09-15,advisor-tool-2026-03-01,effort-", 1)).String(),
		"subagent": sub.String(),
	} {
		if a := strictDo(t, addr, raw); a.status != 200 {
			t.Errorf("%s: %d %s", name, a.status, a.body)
		}
	}
	other := tok.edit(t, `"content":"Hello there, reply with ok"`,
		`"content":[{"type":"text","text":"<system-reminder>Attribution</system-reminder>"},{"type":"text","text":"say ok"}]`)
	checkForged(t, strictDo(t, addr, other.String()), "fingerprint")
}
