package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

// ccE2ECfg is a wwz with the anthropic driver at /cc in the strict mode of
// spec 020 (client_verification: claude-code) and the subscription of the
// key "assinante" (oauth_key, oauth_plan max_20x; spec 006 16.2.1), the
// API key "wwz-key-e2e" of --auth api-key, the fake API key of the
// captures (CCKEY, for replay --send of 004 and 014) and the models the
// profile defaults to, answering "ok".
const ccE2ECfg = `
server: {auth: keys, seed: 7}
store: {kind: memory}
logging: {level: error}
drivers:
  - {name: anthropic, prefix: /cc, options: {oauth_key: assinante, oauth_plan: max_20x, client_verification: claude-code}}
keys:
  - {key: wwz-oauth-secret-e2e, label: assinante}
  - {key: wwz-key-e2e, label: api}
  - {key: CCKEY, label: cc}
profiles:
  latency: {zero: {}}
  generation:
    ok: {mode: template, template: "ok", output_tokens: {dist: fixed, value: 1}}
models:
  - {id: claude-sonnet-5-5, latency: zero, generation: ok}
  - {id: claude-opus-5-5, latency: zero, generation: ok}
`

// startCCE2E serves ccE2ECfg in process and returns the engine and the
// --target of the strict mount.
func startCCE2E(t *testing.T) (*engine.Engine, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(ccFixtures, "004-sdk-cli-apikey-turn.raw"))
	if err != nil {
		t.Fatal(err)
	}
	_, after, _ := strings.Cut(string(raw), "\r\nx-api-key: ")
	key, _, _ := strings.Cut(after, "\r\n")
	c, err := config.Parse([]byte(strings.Replace(ccE2ECfg, "CCKEY", key, 1)))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := engine.New(c, limits.NewMemory(), scenario.NewRegistry(), engine.NewMetrics(metrics.New(1000), nil), log)
	s, err := server.New(eng, server.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return eng, "http://" + ln.Addr().String() + "/cc"
}

// ccState is the state file of wwz-load claude-code, decoded.
type ccState struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	Scopes       []string `json:"scopes"`
}

func readCCState(t *testing.T, path string) ccState {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s ccState
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestClaudeCodeAgainstStrictWWZ is the acceptance of spec 015 ("Contrato
// do wwz-load claude-code", "Critérios") and of spec 020 against a real
// wwz in the strict mode: login (callback and --manual), refresh and
// --setup-token complete; ask answers 200 in sdk-cli and cli, with OAuth
// and with the API key, streamed and with the non-stream fallback, and
// --resume continues the conversation; run --requests 50 has no error;
// replay --send of every fixture gets 200. No request is refused by the
// verification.
func TestClaudeCodeAgainstStrictWWZ(t *testing.T) {
	eng, target := startCCE2E(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "claude-code.json")
	common := []string{"--target", target, "--state", state}
	args := func(extra ...string) []string { return append(append([]string{}, extra...), common...) }
	askOK := func(t *testing.T, extra ...string) {
		t.Helper()
		if code, out, errOut := cc(t, args(append([]string{"ask"}, extra...)...)...); code != exitOK || out != "ok\n" {
			t.Errorf("ask %v: %d %q %q", extra, code, out, errOut)
		}
	}

	// login with the local callback.
	code, out, errOut := cc(t, args("login")...)
	if code != exitOK || !strings.Contains(out, "plan          max (default_claude_max_20x)") ||
		!strings.Contains(out, "account       assinante@wwz.invalid") {
		t.Fatalf("login: %d %q %q", code, out, errOut)
	}
	st := readCCState(t, state)
	if !strings.HasPrefix(st.AccessToken, "sk-ant-oat01-") || !strings.HasPrefix(st.RefreshToken, "sk-ant-ort01-") {
		t.Fatalf("state after login %+v", st)
	}
	login := st.AccessToken

	// ask, every entrypoint, with --resume (sdk-cli: 002/006/008; cli:
	// quota probe then turn_safeguards, 010/012).
	askOK(t, "Hello there, reply with ok")
	askOK(t, "Second turn", "--resume")
	askOK(t, "Third turn", "--resume")
	askOK(t, "Interactive first turn", "--entrypoint", "cli")
	askOK(t, "Interactive second turn", "--entrypoint", "cli", "--resume")
	askOK(t, "Non-stream fallback", "--stream=false")
	askOK(t, "Interactive non-stream", "--entrypoint", "cli", "--stream=false")

	// refresh renews both tokens, and the new access token works.
	if code, out, errOut := cc(t, args("refresh")...); code != exitOK || !strings.Contains(out, "expires") {
		t.Fatalf("refresh: %d %q %q", code, out, errOut)
	}
	if st2 := readCCState(t, state); st2.AccessToken == st.AccessToken || st2.RefreshToken == st.RefreshToken ||
		!strings.HasPrefix(st2.AccessToken, "sk-ant-oat01-") {
		t.Errorf("refresh kept the tokens: %+v", st2)
	}
	askOK(t, "After refresh")

	// login --manual (code#state read from the authorize page).
	if code, out, errOut := cc(t, args("login", "--manual")...); code != exitOK || !strings.Contains(out, "plan          max (default_claude_max_20x)") {
		t.Errorf("login --manual: %d %q %q", code, out, errOut)
	}
	askOK(t, "After the manual login", "--entrypoint", "cli")

	// login --setup-token: only user:inference, a one-year token, no
	// profile; the token still serves inference.
	code, out, errOut = cc(t, args("login", "--setup-token")...)
	if code != exitOK || !strings.Contains(out, "plan          - (scopes user:inference: no profile)") {
		t.Errorf("login --setup-token: %d %q %q", code, out, errOut)
	}
	if s := readCCState(t, state); len(s.Scopes) != 1 || s.Scopes[0] != "user:inference" {
		t.Errorf("setup token scopes %v", s.Scopes)
	}
	exp := strings.TrimSpace(out[strings.Index(out, "expires")+len("expires"):])
	exp, _, _ = strings.Cut(exp, "\n")
	if at, err := time.Parse(time.RFC3339, exp); err != nil || time.Until(at) < 364*24*time.Hour {
		t.Errorf("setup token expires %q %v", exp, err)
	}
	askOK(t, "With the setup token")
	askOK(t, "With the setup token, interactive", "--entrypoint", "cli")

	// The API key, both entrypoints (the API key beta lists of the profile,
	// partly inferred: spec 020, "Betas por tipo").
	askOK(t, "API key turn", "--auth", "api-key", "--key", "wwz-key-e2e")
	askOK(t, "API key second turn", "--auth", "api-key", "--key", "wwz-key-e2e", "--resume")
	askOK(t, "API key interactive", "--auth", "api-key", "--key", "wwz-key-e2e", "--entrypoint", "cli")
	askOK(t, "API key interactive again", "--auth", "api-key", "--key", "wwz-key-e2e", "--entrypoint", "cli", "--resume")
	askOK(t, "API key non-stream", "--auth", "api-key", "--key", "wwz-key-e2e", "--stream=false")
	// An explicit token (no state) works too.
	askOK(t, "Explicit token", "--token", login)

	// run --requests 50 without errors, in both entrypoints and credentials.
	for _, extra := range [][]string{
		{"--concurrency", "5"},
		{"--concurrency", "5", "--entrypoint", "cli", "--turns", "2"},
		{"--concurrency", "5", "--auth", "api-key", "--key", "wwz-key-e2e"},
		{"--rps", "200", "--auth", "api-key", "--key", "wwz-key-e2e", "--entrypoint", "cli"},
	} {
		out := filepath.Join(dir, "run.json")
		a := args(append([]string{"run", "--requests", "50", "--duration", "0", "--output", "json", "--out", out}, extra...)...)
		if code, _, errOut := cc(t, a...); code != exitOK {
			t.Fatalf("run %v: %d %s", extra, code, errOut)
		}
		var rep loadgenReport
		data, _ := os.ReadFile(out)
		if err := json.Unmarshal(data, &rep); err != nil || rep.Config.Protocol != "claude-code" || rep.Total.Errors != 0 ||
			rep.Total.OK != rep.Total.Requests || rep.Total.OK < 50 {
			t.Errorf("run %v: %v %s", extra, err, data)
		}
	}

	// replay --send of every fixture: identical and 200 (the OAuth captures
	// with the token wwz issued; HEAD /api/hello on the public route).
	files, _ := filepath.Glob(filepath.Join(ccFixtures, "*.raw"))
	if len(files) != 9 {
		t.Fatalf("%d fixtures", len(files))
	}
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		a := args("replay", f, "--send")
		if strings.Contains(string(raw), "\r\nAuthorization: Bearer sk-ant-oat01-") {
			a = append(a, "--token", login)
		}
		if code, out, errOut := cc(t, a...); code != exitOK || !strings.HasPrefix(out, "identical sha256=") ||
			!strings.HasSuffix(out, "sent: HTTP 200\n") {
			t.Errorf("replay --send %s: %d %q %q", filepath.Base(f), code, out, errOut)
		}
	}

	if n := eng.M.ClientVerificationFailures.Total(); n != 0 {
		t.Errorf("the strict mount refused %d requests of the client", n)
	}
	// The mount is strict: a request that is not Claude Code's is refused.
	r, err := http.Post(target+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != 403 || r.Header.Get("x-wwz-client-verification") != "failed; reason=query" ||
		eng.M.ClientVerificationFailures.Value("anthropic", "query") != 1 {
		t.Errorf("plain request on the strict mount: %d %v", r.StatusCode, r.Header)
	}
}

// loadgenReport is the part of the run report this test reads.
type loadgenReport struct {
	Config struct {
		Protocol string `json:"protocol"`
	} `json:"config"`
	Total struct {
		Requests int64 `json:"requests"`
		OK       int64 `json:"ok"`
		Errors   int64 `json:"errors"`
	} `json:"total"`
}
