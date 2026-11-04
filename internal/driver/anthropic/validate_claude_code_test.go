package anthropic_test

import (
	"bufio"
	"bytes"
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
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

// ccConfig serves the anthropic driver at the root, without any option,
// with the models the Claude Code 2.1.308 captures name; ccKeys adds their
// fake credentials (an OAuth token taken as a plain key, an API key).
const ccConfig = `
server: {listen: "127.0.0.1:0", auth: keys, seed: 7}
store: {kind: memory}
logging: {level: error}
drivers: [{name: anthropic}]
keys: [{key: wwz-key-cc}]
profiles:
  latency: {zero: {}}
  generation:
    text: {mode: lorem, output_tokens: {dist: fixed, value: 8}, reasoning_tokens: {probability: 1, min: 4, max: 8}}
models:
  - {id: claude-sonnet-5-5, latency: zero, generation: text}
  - {id: claude-opus-5-5, latency: zero, generation: text}
  - {id: claude-haiku-5-5, latency: zero, generation: text}
`

// startWWZ serves cfg in this process and returns its address.
func startWWZ(t *testing.T, cfg string) string {
	t.Helper()
	c, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := engine.NewMetrics(metrics.New(c.Metrics.MaxSeries), c.Metrics.Buckets)
	srv, err := server.New(engine.New(c, limits.NewMemory(), scenario.NewRegistry(), m, log), server.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	return ln.Addr().String()
}

// rawDo writes raw (a whole HTTP/1.1 request, as a client sent it) on a new
// connection and reads the answer; method is the request's, for the body
// rules of HEAD.
func rawDo(t *testing.T, addr, method string, raw []byte) (*http.Response, string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(raw); err != nil {
		t.Fatal(err)
	}
	r, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return r, string(b)
}

// captureDirs are where the Claude Code 2.1.308 requests are: the raw
// captures of tmp/claude-code/captures/ (outside git, found from any
// worktree under the checkout) and the normalized fixtures of
// testdata/real/claude-code/2.1.308/ (task 442), when they exist.
func captureDirs(t *testing.T) []string {
	t.Helper()
	var dirs []string
	if d := filepath.Join("..", "..", "..", "testdata", "real", "claude-code", "2.1.308"); isDir(d) {
		dirs = append(dirs, d)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := wd; ; d = filepath.Dir(d) {
		if c := filepath.Join(d, "tmp", "claude-code", "captures"); isDir(c) {
			dirs = append(dirs, c)
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return dirs
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// TestClaudeCodeCaptures: every request Claude Code 2.1.308 sent (-p with
// OAuth and API key, a conversation, the quota probe, the session title and
// the interactive turn with safeguards), sent byte for byte as captured,
// gets a 200 from the anthropic driver without any option (ADR 0074; task
// 441: before it, 002 got 400 messages.1.output_config and then
// messages.1.role). The HEAD /api/hello probe is a 200 too. Skipped when no
// capture is around.
func TestClaudeCodeCaptures(t *testing.T) {
	dirs := captureDirs(t)
	if len(dirs) == 0 {
		t.Skip("no Claude Code captures (tmp/claude-code/captures, testdata/real/claude-code/2.1.308)")
	}
	var files []string
	keys := map[string]bool{}
	for _, dir := range dirs {
		fs, _ := filepath.Glob(filepath.Join(dir, "*.raw"))
		for _, f := range fs {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
			for _, line := range strings.Split(head, "\r\n") {
				if k, ok := strings.CutPrefix(line, "Authorization: Bearer "); ok {
					keys[k] = true
				} else if k, ok := strings.CutPrefix(line, "x-api-key: "); ok {
					keys[k] = true
				}
			}
		}
		files = append(files, fs...)
	}
	cfg := ccConfig
	for k := range keys {
		cfg = strings.Replace(cfg, "keys: [", "keys: [{key: "+k+"}, ", 1)
	}
	addr := startWWZ(t, cfg)
	seen := 0
	{
		for _, f := range files {
			dir := filepath.Dir(f)
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			method, _, _ := strings.Cut(string(raw), " ")
			if bytes.HasPrefix(raw, []byte("POST http")) {
				continue // a capture through a proxy (absolute URI)
			}
			seen++
			t.Run(filepath.Base(dir)+"/"+filepath.Base(f), func(t *testing.T) {
				r, body := rawDo(t, addr, method, raw)
				if r.StatusCode != 200 {
					t.Fatalf("status %d: %s", r.StatusCode, body)
				}
				if method == "POST" && !strings.Contains(body, `"type":"message_stop"`) && !strings.Contains(body, `"type":"message"`) && !strings.HasPrefix(body, `{"input_tokens":`) {
					t.Errorf("not a message: %s", body)
				}
			})
		}
	}
	if seen == 0 {
		t.Fatal("capture directories without captures")
	}
}

// TestValidateClaudeCodeForm covers the branches of the Claude Code form
// the recorded fixtures (testdata/real/anthropic/validation/cc_*.json,
// replayed by internal/server) do not reach.
func TestValidateClaudeCodeForm(t *testing.T) {
	d := newDriver(t).(driver.RequestValidator)
	const pt = "per-turn-control-2026-07-01"
	cases := []struct {
		name, beta, body, want string
	}{
		{"tool_addition in system", "", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"},{"role":"system","content":[{"type":"tool_addition","tools":[]},{"type":"tool_removal","names":[]}]}]}`, ""},
		{"after server tool result", "", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"},{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"web_search","input":{}},{"type":"web_search_tool_result","tool_use_id":"s","content":[]}]},{"role":"system","content":"s"}]}`, ""},
		{"after a tool_use turn", "", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{}}]},{"role":"system","content":"s"}]}`, "messages.2: role 'system' must follow a 'user' message or an 'assistant' message ending in a server tool result; the directive-only form (content: [] with output_config) is accepted at any position"},
		{"after empty assistant", "", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"},{"role":"assistant","content":[]},{"role":"system","content":"s"}]}`, "messages.2: role 'system' must follow a 'user' message or an 'assistant' message ending in a server tool result; the directive-only form (content: [] with output_config) is accepted at any position"},
		{"directive then system", pt, `{"model":"m","max_tokens":1,"messages":[{"role":"system","content":[],"output_config":{"effort":"low"}},{"role":"system","content":"s"},{"role":"user","content":"x"}]}`, "messages.1: use the top-level 'system' parameter for the initial system prompt; the directive-only form (content: [] with output_config) is accepted at any position"},
		{"message not an object", "", `{"model":"m","max_tokens":1,"messages":["x",{"role":"user","content":"x"}]}`, ""},
		{"output_config not an object", pt, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"},{"role":"system","content":"s","output_config":"low"}]}`, "messages.1.output_config: Input does not match the expected shape."},
		{"display not a string", "", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"adaptive","display":1}}`, "thinking.adaptive.display: Input should be 'summarized', 'omitted'"},
		{"thinking enabled untouched", "", `{"model":"m","max_tokens":2000,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"enabled","budget_tokens":1024,"display":"x"}}`, ""},
		{"thinking not an object", "", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"thinking":"adaptive"}`, "thinking: Input should be an object"},
		{"safeguards null with beta", "dangerous-tool-use-2026-09-03", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"safeguards":null}`, ""},
		{"safeguards without type", "dangerous-tool-use-2026-09-03", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"safeguards":[{}]}`, "safeguards.0: Unable to extract tag using discriminator 'type'"},
		{"safeguards on count_tokens", "dangerous-tool-use-2026-09-03", `{"model":"m","messages":[{"role":"user","content":"x"}],"safeguards":[]}`, "safeguards: Extra inputs are not permitted"},
		{"deferred and not", "", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a","input_schema":{"type":"object"},"defer_loading":false},{"name":"b","input_schema":{"type":"object"},"defer_loading":true}]}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := driver.MapHeaders{"anthropic-version": "2023-06-01"}
			if c.beta != "" {
				h["anthropic-beta"] = c.beta
			}
			kind := driver.KindMessages
			if c.name == "safeguards on count_tokens" {
				kind = driver.KindCountTokens
			}
			got := ""
			if e := d.ValidateRequest(kind, []byte(c.body), h, nil); e != nil {
				got = e.Message
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestValidateClaudeCodeFixtures runs the recorded Claude Code form
// (testdata/real/anthropic/validation/cc_*.json, 2026-11-03) through the
// validator alone: the message of each refusal, nothing on each 200 (the
// whole answer is replayed through the data plane in internal/server).
func TestValidateClaudeCodeFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "real", "anthropic", "validation", "cc_*.json"))
	if err != nil || len(files) < 70 {
		t.Fatalf("cc fixtures: %d %v", len(files), err)
	}
	d := newDriver(t).(driver.RequestValidator)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Request struct {
				Headers map[string]string `json:"headers"`
				Body    string            `json:"body"`
			} `json:"request"`
			Response struct {
				Body *string `json:"body"`
			} `json:"response"`
		}
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatal(err)
		}
		want := ""
		if fx.Response.Body != nil {
			var env struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(*fx.Response.Body), &env); err != nil {
				t.Fatal(err)
			}
			want = env.Error.Message
		}
		got := ""
		if e := d.ValidateRequest(driver.KindMessages, []byte(fx.Request.Body), driver.MapHeaders(fx.Request.Headers), nil); e != nil {
			got = e.Message
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", filepath.Base(f), got, want)
		}
	}
}
