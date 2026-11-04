package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/claudecode/client"
	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

const ccFixtures = "../../testdata/real/claude-code/2.1.308"

// ccFake is a wwz-like server for the command: the OAuth routes of spec
// 006 16.2.1 (without the checks the package tests do) and /v1/messages.
type ccFake struct {
	mu          sync.Mutex
	bodies      []string // bodies of /v1/messages
	auths       []string // Authorization of /v1/messages
	tokens      int      // POST /v1/oauth/token
	status      int      // /v1/messages answers this error when ≠ 0
	tokenStatus int      // /v1/oauth/token answers this error when ≠ 0
	noRequestID bool     // the profile has no request-id
	quota       bool     // /v1/messages answers the subscription 429
	orgType     string   // organization_type of the profile ("" = claude_max)
}

func (f *ccFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case "/wwz/oauth/authorize":
		q := r.URL.Query()
		if q.Get("redirect_uri") == client.ManualRedirectURL {
			_, _ = io.WriteString(w, "code1#"+q.Get("state"))
			return
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=code1&state="+q.Get("state"), http.StatusFound)
	case "/wwz/v1/oauth/token":
		f.tokens++
		if f.tokenStatus != 0 {
			w.WriteHeader(f.tokenStatus)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Refresh token expired"}`)
			return
		}
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		scope, _ := v["scope"].(string)
		if v["grant_type"] == "authorization_code" {
			scope = strings.Join(client.LoginScopes, " ")
			if _, ok := v["expires_in"]; ok {
				scope = client.ScopeInference
			}
		}
		_, _ = fmt.Fprintf(w, `{"access_token":"sk-ant-oat01-T%d","refresh_token":"sk-ant-ort01-R","expires_in":28800,"scope":%q,`+
			`"account":{"uuid":"6b1c7a50-8d3e-4c8a-9b1f-1f2d3c4b5a69","email_address":"k@wwz.invalid"},`+
			`"organization":{"uuid":"0d1e2f3a-4b5c-4d6e-8f70-8192a3b4c5d6","name":"acct-1"}}`, f.tokens, scope)
	case "/wwz/api/oauth/profile":
		if !f.noRequestID {
			w.Header().Set("request-id", "req_011CVprofile")
		}
		org := f.orgType
		if org == "" {
			org = "claude_max"
		}
		_, _ = io.WriteString(w, `{"account":{"uuid":"6b1c7a50-8d3e-4c8a-9b1f-1f2d3c4b5a69","email":"k@wwz.invalid"},`+
			`"organization":{"uuid":"0d1e2f3a-4b5c-4d6e-8f70-8192a3b4c5d6","name":"acct-1","organization_type":"`+org+`",`+
			`"rate_limit_tier":"default_claude_max_5x"}}`)
	case "/wwz/api/oauth/claude_cli/roles":
		_, _ = io.WriteString(w, `{"organization_role":"admin","workspace_role":null,"organization_name":"acct-1"}`)
	case "/wwz/v1/messages":
		f.bodies = append(f.bodies, string(body))
		f.auths = append(f.auths, r.Header.Get("Authorization")+r.Header.Get("X-Api-Key"))
		if f.quota {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-should-retry", "true")
			w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
			w.Header().Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"quota"}}`)
			return
		}
		if f.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-should-retry", "false")
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"forged_request_error","message":"Client verification failed"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"content_block_delta","delta":{"type":"text_delta","text":"o"}}`,
			`{"type":"content_block_delta","delta":{"type":"text_delta","text":"k"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	default:
		http.NotFound(w, r)
	}
}

func newCCFake(t *testing.T) (*ccFake, string) {
	t.Helper()
	f := &ccFake{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL + "/wwz"
}

func cc(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{"claude-code"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// TestClaudeCodeRefusesRealHosts: every action refuses a target on a real
// Anthropic host with exit 2 and the message of spec 015, before any
// connection.
func TestClaudeCodeRefusesRealHosts(t *testing.T) {
	var dials atomic.Int32
	ccDial = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("no")
	}
	defer func() { ccDial = nil }()
	state := filepath.Join(t.TempDir(), "s.json")
	fixture := filepath.Join(ccFixtures, "002-sdk-cli-oauth-turn.raw")
	for _, host := range []string{"api.anthropic.com", "claude.ai", "x.claude.com", "claude.com", "console.anthropic.com"} {
		for _, args := range [][]string{
			{"login"},
			{"login", "--manual"},
			{"refresh"},
			{"ask", "hi", "--token", "t"},
			{"run", "--token", "t", "--requests", "1"},
			{"replay", fixture, "--send"},
		} {
			code, out, errOut := cc(t, append(args, "--target", "https://"+strings.ToUpper(host)+"/", "--state", state)...)
			want := "wwz-load claude-code: refusing real Anthropic host " + host + ": this client only talks to the simulator\n"
			if code != exitUsage || errOut != want || out != "" {
				t.Errorf("%v on %s: %d %q %q", args, host, code, out, errOut)
			}
		}
	}
	if dials.Load() != 0 {
		t.Errorf("%d connections opened", dials.Load())
	}
	if _, err := os.Stat(state); err == nil {
		t.Error("state written for a refused target")
	}
}

func TestClaudeCodeUsage(t *testing.T) {
	_, u := newCCFake(t)
	state := filepath.Join(t.TempDir(), "s.json")
	s := []string{"--state", state}
	cases := []struct {
		args []string
		code int
	}{
		{nil, exitUsage},
		{[]string{"help"}, exitOK},
		{[]string{"-h"}, exitOK},
		{[]string{"login", "-h"}, exitOK},
		{[]string{"nope"}, exitUsage},
		{[]string{"login", "--bogus"}, exitUsage},
		{[]string{"ask"}, exitUsage},
		{[]string{"ask", "a", "b", "--target", u}, exitUsage},
		{[]string{"ask", "x"}, exitUsage},
		{[]string{"replay", "f", "--send"}, exitUsage},
		{[]string{"login", "--target", "ftp://x"}, exitUsage},
		{[]string{"ask", "x", "--target", u, "--auth", "nope"}, exitUsage},
		{[]string{"ask", "x", "--target", u, "--key", "k"}, exitUsage},
		{[]string{"ask", "x", "--target", u, "--auth", "api-key", "--token", "t"}, exitUsage},
		{[]string{"ask", "x", "--target", u, "--auth", "api-key"}, exitUsage},
		{[]string{"run", "--target", u, "--output", "xml"}, exitUsage},
		{[]string{"run", "--target", u, "--rps", "1", "--concurrency", "2"}, exitUsage},
		{append([]string{"login", "--target", u, "--version", "2.1.309"}, s...), exitUsage},
		{append([]string{"refresh", "--target", u, "--arch", "ppc"}, s...), exitUsage},
		{append([]string{"ask", "x", "--target", u, "--token", "t", "--entrypoint", "sdk-py"}, s...), exitUsage},
		{append([]string{"run", "--target", u, "--token", "t", "--os", "Plan9"}, s...), exitUsage},
		{append([]string{"run", "--target", u, "--token", "t", "--turns", "0"}, s...), exitUsage},
		{append([]string{"run", "--target", u, "--token", "t", "--rps", "-1"}, s...), exitUsage},
		{append([]string{"ask", "x", "--target", u}, s...), exitUsage}, // no token in the state
		{append([]string{"run", "--target", u}, s...), exitUsage},
	}
	for _, tc := range cases {
		if code, _, errOut := cc(t, tc.args...); code != tc.code {
			t.Errorf("%v: code %d, want %d (%s)", tc.args, code, tc.code, errOut)
		}
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	if code, _, _ := cc(t, "login", "--target", u); code != exitUsage {
		t.Errorf("no state path: %d", code)
	}
}

func TestClaudeCodeFlow(t *testing.T) {
	f, u := newCCFake(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "state", "claude-code.json")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", dir)
	common := []string{"--target", u, "--state", state}
	code, out, errOut := cc(t, append([]string{"login"}, common...)...)
	if code != exitOK || !strings.Contains(out, "plan          max (default_claude_max_5x)") || !strings.Contains(out, "account       k@wwz.invalid") {
		t.Fatalf("login: %d %q %q", code, out, errOut)
	}
	fi, err := os.Stat(state)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state %v %v", fi, err)
	}
	dump := filepath.Join(dir, "dump.raw")
	code, out, errOut = cc(t, append([]string{"ask", "Hello", "--dump", dump}, common...)...)
	if code != exitOK || out != "ok\n" {
		t.Fatalf("ask: %d %q %q", code, out, errOut)
	}
	raw, _ := os.ReadFile(dump)
	if !bytes.HasPrefix(raw, []byte("POST /wwz/v1/messages?beta=true HTTP/1.1\r\nAccept: application/json\r\nAuthorization: Bearer sk-ant-oat01-T1\r\n")) ||
		!strings.Contains(f.bodies[0], `\"account_uuid\":\"6b1c7a50-8d3e-4c8a-9b1f-1f2d3c4b5a69\"`) {
		t.Errorf("dump %q / body %s", raw[:min(len(raw), 120)], f.bodies[0])
	}
	if code, out, _ = cc(t, append([]string{"ask", "Again", "--resume"}, common...)...); code != exitOK || out != "ok\n" ||
		!strings.Contains(f.bodies[1], `{"role":"assistant","content":[{"type":"text","text":"ok"}]},{"role":"user","content":"Again"}`) {
		t.Errorf("resume: %d %s", code, f.bodies[1])
	}
	if code, _, _ = cc(t, append([]string{"ask", "x", "--resume", "--entrypoint", "cli"}, common...)...); code != exitUsage {
		t.Errorf("resume of another entrypoint: %d", code)
	}
	if code, out, _ = cc(t, append([]string{"refresh"}, common...)...); code != exitOK || !strings.Contains(out, "expires") {
		t.Errorf("refresh: %d %q", code, out)
	}
	jsonOut := filepath.Join(dir, "run.json")
	if code, _, errOut = cc(t, append([]string{"run", "--requests", "2", "--duration", "0", "--output", "json", "--out", jsonOut, "--turns", "2"}, common...)...); code != exitOK {
		t.Fatalf("run: %d %s", code, errOut)
	}
	var rep struct {
		Config struct{ Protocol, Path string } `json:"config"`
		Total  struct{ OK int64 }              `json:"total"`
	}
	data, _ := os.ReadFile(jsonOut)
	if err := json.Unmarshal(data, &rep); err != nil || rep.Config.Protocol != "claude-code" || rep.Config.Path != "/wwz/v1/messages" || rep.Total.OK != 4 {
		t.Errorf("report %+v %v %s", rep, err, data)
	}
	if code, out, _ = cc(t, append([]string{"run", "--rps", "50", "--requests", "2", "--output", "csv", "--entrypoint", "cli"}, common...)...); code != exitOK ||
		!strings.HasPrefix(out, strings.Join(loadgen.CSVHeader, ",")) {
		t.Errorf("run rps: %d %q", code, out)
	}
	if code, out, _ = cc(t, append([]string{"login", "--manual"}, common...)...); code != exitOK || !strings.Contains(out, "plan          max") {
		t.Errorf("manual: %d %q", code, out)
	}
	if code, out, _ = cc(t, append([]string{"login", "--setup-token"}, common...)...); code != exitOK || !strings.Contains(out, "plan          - (scopes user:inference: no profile)") {
		t.Errorf("setup token: %d %q", code, out)
	}
	// After the setup token the account is not sent (no profile scope).
	if code, _, _ = cc(t, append([]string{"ask", "x"}, common...)...); code != exitOK ||
		!strings.Contains(f.bodies[len(f.bodies)-1], `\"account_uuid\":\"\"`) {
		t.Errorf("setup token ask: %d", code)
	}
	// Explicit credentials.
	if code, _, _ = cc(t, append([]string{"ask", "x", "--token", "sk-ant-oat01-X"}, common...)...); code != exitOK || f.auths[len(f.auths)-1] != "Bearer sk-ant-oat01-X" {
		t.Errorf("--token: %d %v", code, f.auths)
	}
	if code, _, _ = cc(t, append([]string{"ask", "x", "--auth", "api-key", "--key", "wwz-key-01"}, common...)...); code != exitOK || f.auths[len(f.auths)-1] != "wwz-key-01" {
		t.Errorf("--key: %d %v", code, f.auths)
	}
	f.noRequestID = true
	if code, out, _ = cc(t, append([]string{"login"}, common...)...); code != exitOK || !strings.Contains(out, "unconfirmed: the profile has no request-id") {
		t.Errorf("unstamped: %d %q", code, out)
	}
	// The default state path is used without --state.
	if code, _, _ = cc(t, "login", "--target", u); code != exitOK {
		t.Errorf("default state: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, ".local", "state", "wwz-load", "claude-code.json")); err != nil {
		t.Error(err)
	}
}

func TestClaudeCodeExpiredToken(t *testing.T) {
	f, u := newCCFake(t)
	state := filepath.Join(t.TempDir(), "s.json")
	common := []string{"--target", u, "--state", state}
	if code, _, _ := cc(t, append([]string{"login"}, common...)...); code != exitOK {
		t.Fatal("login")
	}
	ccNow = func() time.Time { return time.Now().Add(9 * time.Hour) }
	defer func() { ccNow = time.Now }()
	if code, _, errOut := cc(t, append([]string{"ask", "x"}, common...)...); code != exitOK || f.tokens != 2 || f.auths[0] != "Bearer sk-ant-oat01-T2" {
		t.Fatalf("auto refresh: %d %d %v %s", code, f.tokens, f.auths, errOut)
	}
	// Still expired by the clock: renewing fails, or cannot build its client.
	f.tokenStatus = 400
	if code, _, errOut := cc(t, append([]string{"ask", "x"}, common...)...); code != exitError || !strings.Contains(errOut, "Refresh token expired") {
		t.Errorf("refresh refused: %d %s", code, errOut)
	}
	if code, _, _ := cc(t, append([]string{"ask", "x", "--arch", "ppc"}, common...)...); code != exitUsage {
		t.Errorf("refresh client: %d", code)
	}
	if code, _, _ := cc(t, append([]string{"refresh"}, common...)...); code != exitError {
		t.Errorf("refresh: %d", code)
	}
	if code, _, _ := cc(t, append([]string{"login"}, common...)...); code != exitError {
		t.Errorf("login refused: %d", code)
	}
	// A state that cannot be saved after the renewal.
	f.tokenStatus = 0
	dir := filepath.Dir(state)
	_ = os.Chmod(dir, 0o500)
	defer func() { _ = os.Chmod(dir, 0o700) }()
	if code, _, _ := cc(t, append([]string{"ask", "x"}, common...)...); code != exitError {
		t.Errorf("save after renewal: %d", code)
	}
}

// TestClaudeCodeSubscriberRetries: the plan and the scopes of the state
// reach the client, so a subscription 429 is retried for enterprise (like
// Claude Code) and not for a max subscriber (spec 015, "Transporte").
func TestClaudeCodeSubscriberRetries(t *testing.T) {
	for _, tc := range []struct {
		org  string
		want int
	}{{"claude_enterprise", 3}, {"claude_max", 1}} {
		f, u := newCCFake(t)
		f.orgType = tc.org
		common := []string{"--target", u, "--state", filepath.Join(t.TempDir(), "s.json")}
		if code, _, errOut := cc(t, append([]string{"login"}, common...)...); code != exitOK {
			t.Fatalf("%s login: %s", tc.org, errOut)
		}
		f.quota = true
		if code, _, _ := cc(t, append([]string{"ask", "x", "--retries", "2"}, common...)...); code != exitError || len(f.bodies) != tc.want {
			t.Errorf("%s: exit %d, %d requests, want %d", tc.org, code, len(f.bodies), tc.want)
		}
	}
}

func TestClaudeCodeFailures(t *testing.T) {
	f, u := newCCFake(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	common := []string{"--target", u, "--state", state}
	if code, out, _ := cc(t, append([]string{"refresh"}, common...)...); code != exitError || out != "" {
		t.Errorf("refresh without login: %d", code)
	}
	if code, _, _ := cc(t, append([]string{"ask", "x", "--resume", "--token", "t"}, common...)...); code != exitUsage {
		t.Errorf("resume without conversation: %d", code)
	}
	if code, _, _ := cc(t, append([]string{"ask", "x", "--token", "t", "--arch", "ppc"}, common...)...); code != exitUsage {
		t.Errorf("ask client: %d", code)
	}
	if code, _, _ := cc(t, append([]string{"ask", "x", "--token", "t", "--dump", filepath.Join(dir, "no", "d")}, common...)...); code != exitError {
		t.Errorf("dump: %d", code)
	}
	f.status = 403
	code, _, errOut := cc(t, append([]string{"ask", "x", "--token", "t"}, common...)...)
	if code != exitError || !strings.Contains(errOut, "HTTP 403 (http_403/forged_request_error)") || !strings.Contains(errOut, "Client verification failed") {
		t.Errorf("403: %d %s", code, errOut)
	}
	f.status = 0
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := "http://" + ln.Addr().String()
	_ = ln.Close()
	if code, _, errOut := cc(t, "ask", "x", "--token", "t", "--target", closed, "--state", state, "--retries", "0"); code != exitError ||
		!strings.Contains(errOut, "connect") {
		t.Errorf("closed port: %d %s", code, errOut)
	}
	// State that cannot be read or saved.
	for _, args := range [][]string{{"login"}, {"refresh"}, {"ask", "x", "--token", "t"}, {"run", "--token", "t", "--requests", "1"}} {
		if code, _, _ := cc(t, append(args, "--target", u, "--state", dir)...); code != exitError {
			t.Errorf("%v with a directory as state: %d", args, code)
		}
	}
	ro := filepath.Join(dir, "ro")
	_ = os.MkdirAll(ro, 0o700)
	roState := filepath.Join(ro, "s.json")
	if code, _, _ := cc(t, "login", "--target", u, "--state", roState); code != exitOK {
		t.Fatal("login")
	}
	_ = os.Chmod(ro, 0o500)
	defer func() { _ = os.Chmod(ro, 0o700) }()
	for _, args := range [][]string{{"login"}, {"refresh"}, {"ask", "x"}} {
		if code, _, _ := cc(t, append(args, "--target", u, "--state", roState)...); code != exitError {
			t.Errorf("%v with a read-only state: %d", args, code)
		}
	}
	// Report outputs that cannot be written.
	if code, _, _ := cc(t, append([]string{"run", "--token", "t", "--requests", "1", "--out", filepath.Join(dir, "no", "r")}, common...)...); code != exitError {
		t.Errorf("out: %d", code)
	}
	var errb bytes.Buffer
	if code := run(context.Background(), append([]string{"claude-code", "run", "--token", "t", "--requests", "1"}, common...), failWriter{}, &errb); code != exitError {
		t.Errorf("write: %d", code)
	}
}

func TestClaudeCodeReplay(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(ccFixtures, "*.raw"))
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, file := range files {
		code, out, errOut := cc(t, "replay", file)
		if code != exitOK || !strings.HasPrefix(out, "identical sha256=") || len(strings.TrimSpace(out)) != len("identical sha256=")+64 {
			t.Errorf("%s: %d %q %q", file, code, out, errOut)
		}
	}
	dir := t.TempDir()
	turn, _ := os.ReadFile(filepath.Join(ccFixtures, "002-sdk-cli-oauth-turn.raw"))
	mutated := filepath.Join(dir, "m.raw")
	_ = os.WriteFile(mutated, bytes.Replace(turn, []byte("cc_version=2.1.308.9fe"), []byte("cc_version=2.1.308.000"), 1), 0o600)
	if code, out, _ := cc(t, "replay", mutated); code != exitError || !strings.Contains(out, "differs at byte") ||
		!strings.Contains(out, "body $.system[0].text") || !strings.Contains(out, "sha256 want=") {
		t.Errorf("mutated: %d %q", code, out)
	}
	bad := filepath.Join(dir, "bad.raw")
	_ = os.WriteFile(bad, []byte("nope"), 0o600)
	for _, f := range []string{filepath.Join(dir, "missing.raw"), bad} {
		if code, _, _ := cc(t, "replay", f); code != exitUsage {
			t.Errorf("%s: %d", f, code)
		}
	}

	f, u := newCCFake(t)
	state := filepath.Join(dir, "s.json")
	turnFile := filepath.Join(ccFixtures, "002-sdk-cli-oauth-turn.raw")
	if code, out, _ := cc(t, "replay", turnFile, "--send", "--target", u, "--state", state); code != exitOK || !strings.HasSuffix(out, "sent: HTTP 200\n") {
		t.Errorf("send: %d %q", code, out)
	}
	if code, _, _ := cc(t, "replay", turnFile, "--send", "--target", u, "--state", state, "--token", "sk-ant-oat01-Y"); code != exitOK ||
		f.auths[len(f.auths)-1] != "Bearer sk-ant-oat01-Y" {
		t.Errorf("send --token: %v", f.auths)
	}
	if code, _, _ := cc(t, "replay", turnFile, "--send", "--target", u, "--state", state, "--auth", "api-key", "--key", "K"); code != exitOK ||
		f.auths[len(f.auths)-1] != "K" {
		t.Errorf("send --key: %v", f.auths)
	}
	f.status = 403
	if code, out, errOut := cc(t, "replay", turnFile, "--send", "--target", u, "--state", state); code != exitOK ||
		!strings.Contains(out, "sent: HTTP 403") || !strings.Contains(errOut, "forged_request_error") {
		t.Errorf("send 403: %d %q %q", code, out, errOut)
	}
	if code, _, _ := cc(t, "replay", turnFile, "--send", "--target", u, "--state", state, "--arch", "ppc"); code != exitUsage {
		t.Errorf("send client: %d", code)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := "http://" + ln.Addr().String()
	_ = ln.Close()
	if code, _, errOut := cc(t, "replay", turnFile, "--send", "--target", closed, "--state", state, "--retries", "0"); code != exitUsage ||
		!strings.Contains(errOut, "send: connect") {
		t.Errorf("send to a closed port: %d %s", code, errOut)
	}
}

func TestExchangeErr(t *testing.T) {
	if err := exchangeErr(&client.Exchange{}, errors.New("build")); err == nil || err.Error() != "build" {
		t.Errorf("%v", err)
	}
	ok := &client.Exchange{Results: []loadgen.Result{{Status: 200}}}
	if err := exchangeErr(ok, nil); err != nil {
		t.Errorf("%v", err)
	}
	reset := &client.Exchange{Results: []loadgen.Result{{Err: loadgen.ErrReset, Cause: io.ErrUnexpectedEOF}}}
	if err := exchangeErr(reset, nil); err == nil || err.Error() != "reset: unexpected EOF" {
		t.Errorf("%v", err)
	}
}
