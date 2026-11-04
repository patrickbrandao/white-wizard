package agents

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/tools"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fixedResult is a run of two agents with every kind of measure.
func fixedResult() *Result {
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	a0 := NewStats(0, Coder)
	a1 := NewStats(1, Pesquisa)
	ok := func(st *Stats, ttft, e2e time.Duration) {
		st.Requests.Add(&loadgen.Result{
			TTFT: ttft, E2E: e2e, ITL: []time.Duration{time.Millisecond, 3 * time.Millisecond},
			InputTokens: 100, OutputTokens: 20, ReasoningTokens: 4,
		})
	}
	a0.SessionsStarted = 1
	a0.end(statusCompleted, "", 900*time.Millisecond)
	for range 4 {
		ok(a0, 10*time.Millisecond, 40*time.Millisecond)
	}
	a0.retry("http_429/rate_limit_error", time.Second)
	a0.Turns = 2
	a0.TurnE2E.Record(300 * time.Millisecond)
	a0.TurnE2E.Record(600 * time.Millisecond)
	a0.tool(tools.ToolReadFile, ToolResult{Outcome: tools.OutcomeSuccess})
	a0.tool(tools.ToolReadFile, ToolResult{Outcome: tools.OutcomeNatural, IsError: true})
	a0.tool(tools.ToolRunCommand, ToolResult{Outcome: tools.OutcomeError, IsError: true})

	a1.SessionsStarted = 2
	a1.end(statusFailed, "http_429/rate_limit_error", 0)
	a1.end(statusInterrupted, "", 0)
	ok(a1, 20*time.Millisecond, 80*time.Millisecond)
	a1.Requests.Add(&loadgen.Result{Err: "http_429/rate_limit_error"})
	a1.Requests.Add(&loadgen.Result{Err: "timeout"})
	a1.retry("http_429/rate_limit_error", 2*time.Second)
	a1.retry("timeout", 500*time.Millisecond)
	a1.Turns = 1
	a1.TurnE2E.Record(200 * time.Millisecond)
	a1.tool(tools.ToolWebSearch, ToolResult{Outcome: tools.OutcomeSuccess})

	res := &Result{Started: t0, Finished: t0.Add(2 * time.Second), RunID: "0000abcd", Agents: []*Stats{a0, a1}, Total: NewStats(-1, Mixed)}
	for _, a := range res.Agents {
		res.Total.Merge(a)
	}
	return res
}

func fixedConfig() ReportConfig {
	return ReportConfig{
		Target: "http://127.0.0.1:8000", Protocol: "openai", Path: "/v1/chat/completions", Model: "wwz-fast", Stream: true,
		Agents: 2, Profile: "mixed", Turns: 2, MaxToolRounds: 8, Sessions: 1, Retries: 2, Seed: 42, Tools: ToolsInProcess,
		Headers: []string{"x-test-id"},
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // G306: test golden
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: test golden
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs:\n%s", name, got)
	}
}

// TestReportGolden: JSON and CSV follow the schemas of spec 015.
func TestReportGolden(t *testing.T) {
	rep := NewReport(fixedConfig(), fixedResult(), "v0.1.0")
	rep.AdminStats = json.RawMessage(`{"requests":7}`)
	for _, f := range []struct{ format, file string }{{"json", "agents.json"}, {"csv", "agents.csv"}, {"text", "agents.txt"}} {
		var b bytes.Buffer
		if err := rep.Write(&b, f.format); err != nil {
			t.Fatal(err)
		}
		golden(t, f.file, b.Bytes())
	}
	var m map[string]any
	var b bytes.Buffer
	_ = rep.WriteJSON(&b)
	if err := json.Unmarshal(b.Bytes(), &m); err != nil || m["kind"] != "agents" || m["version"] != float64(1) {
		t.Fatalf("%v %v", err, m)
	}
	if err := rep.Write(&b, "xml"); err == nil {
		t.Fatal("xml accepted")
	}
}

func TestReportEmptyAndRates(t *testing.T) {
	t0 := time.Now()
	res := &Result{Started: t0, Finished: t0, Agents: []*Stats{}, Total: NewStats(-1, Coder), Interrupted: true}
	res.Total.Requests.TokensEstimated = true
	rep := NewReport(ReportConfig{}, res, "dev")
	if rep.Config.Headers == nil || rep.Total.RPS != 0 || rep.FailedPct() != 0 || rep.Exceeds(1) {
		t.Fatalf("%+v", rep)
	}
	var b bytes.Buffer
	if err := rep.WriteText(&b); err != nil || !strings.Contains(b.String(), "(interrupted)") || !strings.Contains(b.String(), "(estimated)") {
		t.Fatal(err, b.String())
	}
	rep = NewReport(fixedConfig(), fixedResult(), "dev")
	if rep.FailedPct() != 100.0/3 || !rep.Exceeds(33) || rep.Exceeds(34) || rep.Exceeds(0) || rep.Total.RPS != 5 {
		t.Fatalf("pct %v rps %v", rep.FailedPct(), rep.Total.RPS)
	}
}

// failWriter fails after n bytes.
type failWriter struct{ n int }

func (w *failWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		return 0, errors.New("disk full")
	}
	w.n -= len(p)
	return len(p), nil
}

func TestReportWriteErrors(t *testing.T) {
	rep := NewReport(fixedConfig(), fixedResult(), "dev")
	for f, sizes := range map[string][]int{"text": {0, 300, 800, 1200, 1500, 1620}, "csv": {0, 200}, "json": {0, 1000, 3000}} {
		for _, n := range sizes {
			if err := rep.Write(&failWriter{n: n}, f); err == nil {
				t.Errorf("%s at %d: no error", f, n)
			}
		}
	}
}
