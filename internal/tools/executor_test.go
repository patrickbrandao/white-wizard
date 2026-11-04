package tools

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type sleepRec struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleepRec) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return ctx.Err()
}

func newExec(t *testing.T, mut func(*Config)) (*Executor, *fakeClock, *sleepRec) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Seed = 42
	if mut != nil {
		mut(&cfg)
	}
	clk := &fakeClock{t: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}
	sl := &sleepRec{}
	e, err := New(cfg, WithClock(clk.now), WithSleep(sl.sleep))
	if err != nil {
		t.Fatal(err)
	}
	return e, clk, sl
}

func mustCall(t *testing.T, e *Executor, req CallRequest) Result {
	t.Helper()
	res, err := e.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("%s: %v", req.Name, err)
	}
	return res
}

func TestCallErrors(t *testing.T) {
	e, _, _ := newExec(t, nil)
	ctx := context.Background()
	if _, err := e.Call(ctx, CallRequest{Name: "rm_rf"}); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := e.Call(ctx, CallRequest{Name: ToolReadFile, ForceOutcome: "natural_error"}); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("outcome: %v", err)
	}
	if _, err := e.NewSession(SessionOptions{ID: "chat", Profile: ProfileChat}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Call(ctx, CallRequest{Session: "chat", Name: ToolReadFile}); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("profile: %v", err)
	}
	if _, err := e.NewSession(SessionOptions{ID: "p", Profile: ProfilePesquisa}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Call(ctx, CallRequest{Session: "p", Name: ToolWriteFile}); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("pesquisa write: %v", err)
	}
	mustCall(t, e, CallRequest{Session: "p", Name: ToolWebSearch, Arguments: json.RawMessage(`{"query":"go"}`)})

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.Call(cctx, CallRequest{Name: ToolListDir}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestForcedOutcomes(t *testing.T) {
	e, _, sl := newExec(t, nil)
	res := mustCall(t, e, CallRequest{Session: "s", Name: ToolListDir, ToolCallID: "call_1", ForceOutcome: OutcomeTimeout})
	if res.Outcome != OutcomeTimeout || !res.IsError || res.Content != "tool timed out after 30s" || res.Latency != 30*time.Second || res.ToolCallID != "call_1" {
		t.Fatalf("timeout: %+v", res)
	}
	if sl.waits[len(sl.waits)-1] != 30*time.Second {
		t.Fatalf("timeout must wait the configured timeout: %v", sl.waits)
	}
	res = mustCall(t, e, CallRequest{Session: "s", Name: ToolListDir, ForceOutcome: OutcomeError})
	if res.Outcome != OutcomeError || !res.IsError || !strings.Contains(strings.Join(injectedErrors, "|"), res.Content) {
		t.Fatalf("error: %+v", res)
	}
	if res.Latency < 5*time.Millisecond || res.Latency > 50*time.Millisecond {
		t.Fatalf("latency out of range: %v", res.Latency)
	}
	res = mustCall(t, e, CallRequest{Session: "s", Name: ToolListDir, ForceOutcome: OutcomeSuccess})
	if res.Outcome != OutcomeSuccess || res.IsError || !strings.Contains(res.Content, "README.md") {
		t.Fatalf("success: %+v", res)
	}
	res = mustCall(t, e, CallRequest{Session: "s", Name: ToolReadFile, ForceOutcome: OutcomeSuccess, Arguments: json.RawMessage(`{"path":"nope"}`)})
	if res.Outcome != OutcomeNatural || !res.IsError {
		t.Fatalf("forced success still gives natural errors: %+v", res)
	}
}

func TestInvalidPayloadVariants(t *testing.T) {
	e, _, _ := newExec(t, nil)
	for _, name := range []string{ToolRunCommand, ToolWebSearch, ToolListDir} {
		args := map[string]string{ToolRunCommand: `{"command":"pwd"}`, ToolWebSearch: `{"query":"x"}`, ToolListDir: `{}`}[name]
		seen := map[string]bool{}
		for range 200 {
			res := mustCall(t, e, CallRequest{Session: "ip", Name: name, ForceOutcome: OutcomeInvalidPayload, Arguments: json.RawMessage(args)})
			if res.IsError || res.Outcome != OutcomeInvalidPayload {
				t.Fatalf("%s: %+v", name, res)
			}
			switch {
			case res.Content == "":
				seen["empty"] = true
			case !utf8Valid(res.Content):
				seen["utf8"] = true
			default:
				if json.Valid([]byte(res.Content)) {
					t.Fatalf("%s: truncated JSON still valid: %q", name, res.Content)
				}
				seen["truncated"] = true
			}
		}
		want := 3
		if name == ToolListDir {
			want = 2
		}
		if len(seen) != want {
			t.Errorf("%s: variants %v", name, seen)
		}
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func TestInjectedRates(t *testing.T) {
	e, _, _ := newExec(t, func(c *Config) {
		c.Outcomes = Rates{Error: 10, Timeout: 10, InvalidPayload: 10}
	})
	counts := map[Outcome]int{}
	const n = 10000
	for i := range n {
		res := mustCall(t, e, CallRequest{Session: "rates-" + string(rune('a'+i%10)), Name: ToolListDir})
		counts[res.Outcome]++
	}
	for _, o := range []Outcome{OutcomeTimeout, OutcomeError, OutcomeInvalidPayload} {
		pct := float64(counts[o]) * 100 / n
		if math.Abs(pct-10) > 2 {
			t.Errorf("%s: %.2f%%", o, pct)
		}
	}
	if pct := float64(counts[OutcomeSuccess]) * 100 / n; math.Abs(pct-70) > 3 {
		t.Errorf("success: %.2f%%", pct)
	}
}

func TestPerToolRatesAndLatency(t *testing.T) {
	hundred := 100.0
	e, _, _ := newExec(t, func(c *Config) {
		c.Tools[ToolReadFile] = ToolConfig{Latency: &Range{7 * time.Millisecond, 7 * time.Millisecond}, Outcomes: RateOverrides{Error: &hundred}}
	})
	res := mustCall(t, e, CallRequest{Session: "s", Name: ToolReadFile})
	if res.Outcome != OutcomeError || res.Latency != 7*time.Millisecond {
		t.Fatalf("read_file: %+v", res)
	}
	res = mustCall(t, e, CallRequest{Session: "s", Name: ToolWebSearch, Arguments: json.RawMessage(`{"query":"q"}`)})
	if res.Latency < 100*time.Millisecond || res.Latency > 800*time.Millisecond {
		t.Fatalf("web_search latency %v", res.Latency)
	}
}

type step struct {
	name, args string
}

var script = []step{
	{ToolListDir, `{"recursive":true}`},
	{ToolReadFile, `{"path":"README.md","limit":3}`},
	{ToolWriteFile, `{"path":"notes/todo.md","content":"a\nb\n"}`},
	{ToolEditFile, `{"path":"notes/todo.md","old":"a","new":"z"}`},
	{ToolSearchCode, `{"pattern":"func"}`},
	{ToolRunCommand, `{"command":"go test ./..."}`},
	{ToolWebSearch, `{"query":"golang context"}`},
	{ToolFetchURL, `{"url":"https://example.com/x"}`},
}

func runScript(t *testing.T, e *Executor, session string, seed int64) []Result {
	t.Helper()
	var out []Result
	for range 5 {
		for _, s := range script {
			out = append(out, mustCall(t, e, CallRequest{Session: session, Seed: seed, Name: s.name, Arguments: json.RawMessage(s.args)}))
		}
	}
	return out
}

func TestDeterminismBySeed(t *testing.T) {
	mut := func(c *Config) { c.Outcomes = Rates{Error: 15, Timeout: 5, InvalidPayload: 10} }
	e1, _, _ := newExec(t, mut)
	e2, _, _ := newExec(t, mut)
	a := runScript(t, e1, "agent-1", 0)
	b := runScript(t, e2, "agent-1", 0)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("call %d differs:\n%+v\n%+v", i, a[i], b[i])
		}
	}
	c := runScript(t, e2, "agent-2", 0)
	same := 0
	for i := range a {
		if a[i] == c[i] {
			same++
		}
	}
	if same == len(a) {
		t.Fatal("different sessions should not replay the same sequence")
	}
	// Explicit per-call seed for an implicit session.
	d1 := runScript(t, e1, "seeded", 7)
	e3, _, _ := newExec(t, func(c *Config) { mut(c); c.Seed = 99 })
	d2 := runScript(t, e3, "seeded", 7)
	for i := range d1 {
		if d1[i] != d2[i] {
			t.Fatalf("call %d differs with x-wwz-seed", i)
		}
	}
	if info, _ := e3.Session("seeded"); info.Seed != 7 {
		t.Fatalf("seed: %+v", info)
	}
}

func TestRandomSeed(t *testing.T) {
	e, _, _ := newExec(t, func(c *Config) { c.Seed = 0 })
	a, _ := e.NewSession(SessionOptions{})
	b, _ := e.NewSession(SessionOptions{})
	if a.Seed == 0 || b.Seed == 0 || a.Seed == b.Seed || a.ID == b.ID {
		t.Fatalf("random seeds: %+v %+v", a, b)
	}
	if !strings.HasPrefix(a.ID, "ses_") || len(a.ID) != 20 {
		t.Fatalf("id: %q", a.ID)
	}
	res := mustCall(t, e, CallRequest{Name: ToolListDir})
	if res.Outcome != OutcomeSuccess || e.Sessions() != 2 {
		t.Fatalf("throwaway session kept: %+v %d", res, e.Sessions())
	}
}

func TestSessionsLifecycle(t *testing.T) {
	e, clk, _ := newExec(t, func(c *Config) { c.MaxSessions = 3; c.SessionTTL = time.Minute })
	info, err := e.NewSession(SessionOptions{ID: "a", Profile: ProfileCoder, Seed: 5})
	if err != nil || info.ID != "a" || info.Profile != ProfileCoder || info.Seed != 5 || len(info.Files) < 12 || !sort.StringsAreSorted(info.Files) || !slices.Contains(info.Files, "README.md") {
		t.Fatalf("new: %+v %v", info, err)
	}
	if _, err := e.NewSession(SessionOptions{ID: "a"}); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("exists: %v", err)
	}
	if _, err := e.NewSession(SessionOptions{Profile: "x"}); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("profile: %v", err)
	}
	if _, err := e.Session("zz"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := e.DeleteSession("zz"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("delete missing: %v", err)
	}

	files0 := infoFiles(t, e, "a")
	mustCall(t, e, CallRequest{Session: "a", Name: ToolWriteFile, ForceOutcome: OutcomeSuccess, Arguments: json.RawMessage(`{"path":"x","content":"1"}`)})
	if got := infoFiles(t, e, "a"); got != files0+1 {
		t.Fatalf("files: %d want %d", got, files0+1)
	}
	// LRU: a, b, c; touch a; d evicts b.
	for _, id := range []string{"b", "c"} {
		clk.advance(time.Second)
		if _, err := e.NewSession(SessionOptions{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	clk.advance(time.Second)
	mustCall(t, e, CallRequest{Session: "a", Name: ToolListDir})
	clk.advance(time.Second)
	if _, err := e.NewSession(SessionOptions{ID: "d"}); err != nil {
		t.Fatal(err)
	}
	if e.Sessions() != 3 {
		t.Fatalf("sessions %d", e.Sessions())
	}
	if _, err := e.Session("b"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("b should be evicted")
	}
	if _, err := e.Session("a"); err != nil {
		t.Fatal("a was touched and must survive")
	}

	// TTL: everything idle for more than a minute is gone.
	clk.advance(2 * time.Minute)
	if _, err := e.Session("a"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("a should be expired")
	}
	if err := e.DeleteSession("c"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("c should be expired")
	}
	if _, err := e.NewSession(SessionOptions{ID: "d"}); err != nil {
		t.Fatalf("expired id reusable: %v", err)
	}
	if e.Sessions() != 1 {
		t.Fatalf("expired sessions swept on create: %d", e.Sessions())
	}
	// An expired session used by Call starts over with a fresh tree.
	clk.advance(2 * time.Minute)
	res := mustCall(t, e, CallRequest{Session: "d", Name: ToolReadFile, Arguments: json.RawMessage(`{"path":"x"}`)})
	if res.Outcome != OutcomeNatural {
		t.Fatalf("fresh tree: %+v", res)
	}
	if err := e.DeleteSession("d"); err != nil || e.Sessions() != 0 {
		t.Fatalf("delete: %v %d", err, e.Sessions())
	}
}

func infoFiles(t *testing.T, e *Executor, id string) int {
	t.Helper()
	info, err := e.Session(id)
	if err != nil {
		t.Fatal(err)
	}
	return len(info.Files)
}

func TestConcurrentCalls(t *testing.T) {
	e, _, _ := newExec(t, func(c *Config) { c.Outcomes = Rates{Error: 5} })
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				s := script[(i+j)%len(script)]
				_, _ = e.Call(context.Background(), CallRequest{Session: []string{"x", "y"}[i%2], Name: s.name, Arguments: json.RawMessage(s.args)})
			}
		}()
	}
	wg.Wait()
	if e.Sessions() != 2 {
		t.Fatalf("sessions %d", e.Sessions())
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := sleepCtx(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Default options: real clock and sleep.
	cfg := DefaultConfig()
	cfg.Latency = Range{}
	cfg.Tools = nil
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Call(context.Background(), CallRequest{Session: "s", Name: ToolListDir})
	if err != nil || res.Latency != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

// TestHostDiskUntouched (spec 017): write_file, edit_file and run_command are
// simulated; the working directory stays empty, a sentinel that rm -rf would
// delete survives, and no file shows up in the temp dir or the parent.
func TestHostDiskUntouched(t *testing.T) {
	sandbox := t.TempDir()
	t.Chdir(sandbox)
	sentinel := filepath.Join(t.TempDir(), "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(os.TempDir(), "wwz-hostdisk-"+filepath.Base(sandbox)+".txt")
	escape := "../wwz-escape-" + filepath.Base(sandbox) + ".txt"
	e, _, _ := newExec(t, nil)
	calls := []struct{ name, args string }{
		{ToolWriteFile, `{"path":"x","content":"hello"}`},
		{ToolWriteFile, `{"path":"` + abs + `","content":"hello"}`},
		{ToolWriteFile, `{"path":"` + escape + `","content":"hello"}`},
		{ToolWriteFile, `{"path":"` + sentinel + `","content":"overwritten"}`},
		{ToolEditFile, `{"path":"x","old":"hello","new":"bye"}`},
		{ToolEditFile, `{"path":"` + sentinel + `","old":"keep","new":"gone"}`},
		{ToolRunCommand, `{"command":"rm -rf /"}`},
		{ToolRunCommand, `{"command":"rm -rf ` + filepath.Dir(sentinel) + `"}`},
		{ToolRunCommand, `{"command":"touch x"}`},
	}
	for _, c := range calls {
		mustCall(t, e, CallRequest{Session: "disk", Name: c.name, Arguments: json.RawMessage(c.args), ForceOutcome: OutcomeSuccess})
	}
	if ents, err := os.ReadDir(sandbox); err != nil || len(ents) != 0 {
		t.Fatalf("working dir not empty: %v %v", ents, err)
	}
	for _, p := range []string{abs, filepath.Join(sandbox, escape)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s exists (%v)", p, err)
		}
	}
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "keep" {
		t.Fatalf("sentinel changed: %q %v", b, err)
	}
}

// TestNoExecNoNetwork proves the package cannot run processes nor reach the
// network: neither os/exec nor net appear in its dependency closure.
func TestNoExecNoNetwork(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not in PATH")
	}
	out, err := exec.Command(gobin, "list", "-deps", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "os/exec" || dep == "net" || strings.HasPrefix(dep, "net/") {
			t.Errorf("forbidden dependency %s", dep)
		}
	}
}
