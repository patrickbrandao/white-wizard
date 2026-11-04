package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/claudecode"
)

// rawReq is one request as the fake server read it from the socket.
type rawReq struct {
	Raw  []byte
	Head claudecode.Head
	Body []byte
}

func (r rawReq) path() string {
	p, _, _ := strings.Cut(r.Head.Target, "?")
	return p
}

func (r rawReq) query() string {
	_, q, _ := strings.Cut(r.Head.Target, "?")
	return q
}

// Sentinel responses of a handler.
var (
	hangUp   = []byte("<hang up>")   // close the connection without answering
	hangOpen = []byte("<hang open>") // never answer; wait for the client to go
)

// fakeServer is a raw TCP HTTP/1.1 server on 127.0.0.1: it records the
// bytes of every request exactly as they arrived (nothing canonicalizes
// them) and answers with the raw bytes its handler returns.
type fakeServer struct {
	ln     net.Listener
	handle func(rawReq) []byte
	mu     sync.Mutex
	hmu    sync.Mutex
	reqs   []rawReq
	conns  atomic.Int32
	dials  atomic.Int32
	closed chan struct{}
}

func newFake(t *testing.T, handle func(rawReq) []byte) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeServer{ln: ln, handle: handle, closed: make(chan struct{})}
	go fs.serve()
	t.Cleanup(func() { close(fs.closed); _ = ln.Close() })
	return fs
}

func (fs *fakeServer) serve() {
	for {
		c, err := fs.ln.Accept()
		if err != nil {
			return
		}
		fs.conns.Add(1)
		go fs.conn(c)
	}
}

func (fs *fakeServer) conn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		var head bytes.Buffer
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				return
			}
			head.Write(line)
			if bytes.Equal(line, []byte("\r\n")) {
				break
			}
		}
		h, _, err := claudecode.ParseHead(head.Bytes())
		if err != nil {
			return
		}
		var body []byte
		if cl, ok := h.Get("Content-Length"); ok {
			n, _ := strconv.Atoi(cl)
			body = make([]byte, n)
			if _, err := io.ReadFull(br, body); err != nil {
				return
			}
		}
		r := rawReq{Raw: append(head.Bytes(), body...), Head: h, Body: body}
		fs.mu.Lock()
		fs.reqs = append(fs.reqs, r)
		fs.mu.Unlock()
		fs.hmu.Lock() // handlers keep state: one at a time
		resp := fs.handle(r)
		fs.hmu.Unlock()
		switch {
		case bytes.Equal(resp, hangUp):
			return
		case bytes.Equal(resp, hangOpen):
			_, _ = io.Copy(io.Discard, c)
			return
		}
		if _, err := c.Write(resp); err != nil {
			return
		}
		framed := bytes.Contains(resp, []byte("Content-Length:")) || bytes.Contains(resp, []byte("Transfer-Encoding:"))
		if bytes.Contains(resp, []byte("\r\nConnection: close\r\n")) || !bytes.HasPrefix(resp, []byte("HTTP/1.1")) || !framed {
			return
		}
	}
}

func (fs *fakeServer) requests() []rawReq {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return slices.Clone(fs.reqs)
}

func (fs *fakeServer) addr() string { return fs.ln.Addr().String() }

// dialer sends every connection to the fake server, whatever the address
// (so a target can keep the Host of a capture, 127.0.0.1:18556).
func (fs *fakeServer) dialer() Dialer {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		fs.dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, fs.addr())
	}
}

// ---- responses ----

func response(status int, headers []string, body string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, statusText(status))
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return b.Bytes()
}

func statusText(status int) string {
	switch status {
	case 200:
		return "OK"
	case 302:
		return "Found"
	}
	return "Status"
}

func jsonResp(status int, body string, headers ...string) []byte {
	return response(status, append([]string{"Content-Type: application/json"}, headers...), body)
}

// sseEvents is a stream answering text, with usage.
func sseEvents(text string) string {
	var b strings.Builder
	ev := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	ev("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":1}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	for _, r := range text {
		d, _ := json.Marshal(string(r))
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(d)+`}}`)
	}
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":`+strconv.Itoa(len(text))+`}}`)
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

// chunked frames body in chunks of n bytes.
func chunked(status int, headers []string, body string, n int) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, statusText(status))
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("Transfer-Encoding: chunked\r\n\r\n")
	for len(body) > 0 {
		k := min(n, len(body))
		fmt.Fprintf(&b, "%x\r\n%s\r\n", k, body[:k])
		body = body[k:]
	}
	b.WriteString("0\r\n\r\n")
	return b.Bytes()
}

func sseResp(text string) []byte {
	return chunked(200, []string{"Content-Type: text/event-stream; charset=utf-8", "request-id: req_fake"}, sseEvents(text), 64)
}

// quotaResp answers the quota probe (non-stream).
func quotaResp() []byte {
	return jsonResp(200, `{"type":"message","content":[{"type":"text","text":"q"}],"stop_reason":"max_tokens","usage":{"input_tokens":8,"output_tokens":1}}`)
}

// messagesHandler answers the quota probe with JSON and turns with SSE.
func messagesHandler(text string) func(rawReq) []byte {
	return func(r rawReq) []byte {
		if bytes.HasPrefix(r.Body, []byte(`{"model":`)) && bytes.Contains(r.Body, []byte(`"max_tokens":1,`)) {
			return quotaResp()
		}
		return sseResp(text)
	}
}

// ---- OAuth (spec 006, 16.2.1) ----

// fakeOAuth is the OAuth side of the anthropic driver as the table of
// spec 006 16.2.1 describes it (the driver itself is task 443): authorize
// with 302 to a local callback or code#state for the manual redirect,
// token in JSON with PKCE S256, profile with request-id, roles.
type fakeOAuth struct {
	secret []byte
	// Knobs.
	onHold        bool   // the account is on hold
	authorizeLoc  string // replaces the Location of the authorize redirect ({host} is the callback host)
	authorizeResp []byte // replaces the whole authorize response
	tokenStatus   int    // refuses the token request with this status
	manualBody    string // replaces the code#state page
	noRequestID   bool   // profile without request-id
	profileBody   string // replaces the profile body
	rolesStatus   int
	rolesBody     string
	tokenBody     string // replaces a 200 token body
	noRefreshExp  bool   // token answers without refresh_token_expires_in
	noRefresh     bool   // refresh answers without refresh_token
}

const fakeAccount = "6b1c7a50-8d3e-4c8a-9b1f-1f2d3c4b5a69"

func (o *fakeOAuth) mac(parts ...string) string {
	m := hmac.New(sha256.New, o.secret)
	m.Write([]byte(strings.Join(parts, "\n")))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:12])
}

// authorizeKeys is the order of the authorize query of Claude Code.
var authorizeKeys = []string{"code", "client_id", "response_type", "redirect_uri", "scope", "code_challenge", "code_challenge_method", "state"}

func (o *fakeOAuth) handle(r rawReq) []byte {
	switch {
	case r.Head.Method == "GET" && r.path() == "/oauth/authorize":
		return o.authorize(r)
	case r.Head.Method == "POST" && r.path() == "/v1/oauth/token":
		return o.token(r)
	case r.Head.Method == "GET" && r.path() == "/api/oauth/profile":
		return o.profile(r)
	case r.Head.Method == "GET" && r.path() == "/api/oauth/claude_cli/roles":
		if o.rolesStatus != 0 {
			return jsonResp(o.rolesStatus, o.rolesBody)
		}
		return jsonResp(200, `{"organization_role":"admin","workspace_role":null,"organization_name":"acct-1"}`)
	case r.Head.Method == "POST" && r.path() == "/v1/messages":
		return sseResp("ok")
	}
	return jsonResp(404, `{"type":"error","error":{"type":"not_found_error","message":"Not found"}}`)
}

func text400(msg string) []byte { return response(400, []string{"Content-Type: text/plain"}, msg) }

func (o *fakeOAuth) authorize(r rawReq) []byte {
	if o.authorizeResp != nil {
		return o.authorizeResp
	}
	var keys []string
	for _, kv := range strings.Split(r.query(), "&") {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	q, _ := url.ParseQuery(r.query())
	redirect := q.Get("redirect_uri")
	u, _ := url.Parse(redirect)
	manual := redirect == ManualRedirectURL
	local := u != nil && u.Scheme == "http" && (strings.HasPrefix(u.Host, "localhost:") || strings.HasPrefix(u.Host, "127.0.0.1:")) && u.Path == "/callback"
	switch {
	case !slices.Equal(keys, authorizeKeys):
		return text400("parameters out of order: " + strings.Join(keys, ","))
	case q.Get("client_id") != ClientID:
		return text400("invalid client_id")
	case q.Get("response_type") != "code" || q.Get("code") != "true":
		return text400("invalid response_type")
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		return text400("PKCE S256 required")
	case !manual && !local:
		return text400("invalid redirect_uri")
	}
	for _, s := range strings.Fields(q.Get("scope")) {
		if !slices.Contains(LoginScopes, s) {
			return text400("unknown scope " + s)
		}
	}
	state := q.Get("state")
	if o.onHold {
		if manual {
			return text400("account_on_hold")
		}
		return response(302, []string{"Location: " + redirect + "?error=access_denied&error_description=account_on_hold&state=" + url.QueryEscape(state)}, "")
	}
	code := "c." + base64.RawURLEncoding.EncodeToString([]byte(q.Get("scope"))) + "." + o.mac(q.Get("code_challenge"), redirect, ClientID, q.Get("scope"))
	o.codes().Store(code, [3]string{q.Get("code_challenge"), redirect, q.Get("scope")})
	if manual {
		body := code + "#" + state
		if o.manualBody != "" {
			body = o.manualBody
		}
		return response(200, []string{"Content-Type: text/plain"}, body)
	}
	loc := redirect + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
	if o.authorizeLoc != "" {
		loc = strings.ReplaceAll(o.authorizeLoc, "{host}", u.Host)
	}
	return response(302, []string{"Location: " + loc}, "")
}

var codeStore sync.Map

func (o *fakeOAuth) codes() *sync.Map { return &codeStore }

// tokenKeys are the orders of the token body of Claude Code.
var (
	codeKeys    = []string{"grant_type", "code", "redirect_uri", "client_id", "code_verifier", "state"}
	refreshKeys = []string{"grant_type", "refresh_token", "client_id", "scope"}
)

func oauthErr(status int, code, desc string) []byte {
	return jsonResp(status, `{"error":"`+code+`","error_description":"`+desc+`"}`)
}

func (o *fakeOAuth) token(r rawReq) []byte {
	v, err := claudecode.Parse(r.Body)
	if err != nil || !bytes.Equal(claudecode.Marshal(v), r.Body) {
		return oauthErr(400, "invalid_request", "body is not compact JSON")
	}
	get := func(k string) string { x, _ := v.Get(k); return x.Str() }
	keys := v.Keys()
	if get("client_id") != ClientID {
		return oauthErr(401, "invalid_client", "Invalid client_id")
	}
	if o.onHold {
		return oauthErr(400, "invalid_grant", "account_on_hold")
	}
	if o.tokenStatus != 0 {
		return oauthErr(o.tokenStatus, "invalid_grant", "Invalid 'code' in request.")
	}
	var scope string
	var expiresIn int64 = 28800
	switch get("grant_type") {
	case "authorization_code":
		if !slices.Equal(keys, codeKeys) && !slices.Equal(keys, append(slices.Clone(codeKeys), "expires_in")) {
			return oauthErr(400, "invalid_request", "keys out of order: "+strings.Join(keys, ","))
		}
		got, ok := o.codes().Load(get("code"))
		sum := sha256.Sum256([]byte(get("code_verifier")))
		if !ok || got.([3]string)[0] != base64.RawURLEncoding.EncodeToString(sum[:]) || got.([3]string)[1] != get("redirect_uri") {
			return oauthErr(400, "invalid_grant", "Invalid 'code' in request.")
		}
		scope = got.([3]string)[2]
		if e, ok := v.Get("expires_in"); ok {
			expiresIn = int64(e.Num())
		}
	case "refresh_token":
		if !slices.Equal(keys, refreshKeys) {
			return oauthErr(400, "invalid_request", "keys out of order: "+strings.Join(keys, ","))
		}
		rt, ok := strings.CutPrefix(get("refresh_token"), "sk-ant-ort01-")
		scope = get("scope")
		if !ok || rt != o.mac("refresh", scope) {
			return oauthErr(400, "invalid_grant", "Refresh token not found or invalid")
		}
	default:
		return oauthErr(400, "unsupported_grant_type", "grant_type")
	}
	if o.tokenBody != "" {
		return jsonResp(200, o.tokenBody)
	}
	refresh := `,"refresh_token":"sk-ant-ort01-` + o.mac("refresh", scope) + `"`
	if o.noRefresh && get("grant_type") == "refresh_token" {
		refresh = ""
	}
	refreshExp := `,"refresh_token_expires_in":2592000`
	if o.noRefreshExp {
		refreshExp = ""
	}
	return jsonResp(200, `{"token_type":"Bearer","access_token":"sk-ant-oat01-`+o.mac("access", scope)+`","expires_in":`+
		strconv.FormatInt(expiresIn, 10)+refresh+refreshExp+`,"scope":"`+scope+
		`","organization":{"uuid":"0d1e2f3a-4b5c-4d6e-8f70-8192a3b4c5d6","name":"acct-1"},"account":{"uuid":"`+fakeAccount+
		`","email_address":"wwz-key-01@wwz.invalid"}}`)
}

func (o *fakeOAuth) profile(r rawReq) []byte {
	auth, _ := r.Head.Get("Authorization")
	ua, _ := r.Head.Get("User-Agent")
	if !strings.HasPrefix(auth, "Bearer sk-ant-oat01-") || ua != "claude-code/2.1.308" {
		return jsonResp(401, `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token is invalid."},"request_id":"req_x"}`,
			"request-id: req_x")
	}
	body := `{"account":{"uuid":"` + fakeAccount + `","email":"wwz-key-01@wwz.invalid","display_name":"wwz-key-01","full_name":"wwz-key-01",` +
		`"created_at":"2026-01-01T00:00:00Z"},"organization":{"uuid":"0d1e2f3a-4b5c-4d6e-8f70-8192a3b4c5d6","name":"acct-1",` +
		`"organization_type":"claude_max","rate_limit_tier":"default_claude_max_5x","seat_tier":null,"has_extra_usage_enabled":false,` +
		`"billing_type":null,"subscription_created_at":"2026-01-01T00:00:00Z"}}`
	if o.profileBody != "" {
		body = o.profileBody
	}
	if o.noRequestID {
		return jsonResp(200, body)
	}
	return jsonResp(200, body, "request-id: req_011CVfakeProfile")
}

// newOAuthClient returns a client and the fake OAuth server it talks to.
func newOAuthClient(t *testing.T, o *fakeOAuth) (*Client, *fakeServer) {
	t.Helper()
	if o.secret == nil {
		o.secret = []byte("wwz-key-01")
	}
	fs := newFake(t, o.handle)
	tg, err := ParseTarget("http://" + fs.addr())
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Target: tg, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c, fs
}
