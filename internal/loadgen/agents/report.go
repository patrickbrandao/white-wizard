package agents

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/loadgen/hist"
	"github.com/patrickbrandao/white-wizard/internal/tools"
)

// Kind is the "kind" of the agents report.
const Kind = "agents"

// ToolsInProcess is config.tools when the tools run in this process.
const ToolsInProcess = "in-process"

// ReportConfig is the "config" object of the report (spec 015).
type ReportConfig struct {
	Target        string  `json:"target"`
	Protocol      string  `json:"protocol"`
	Path          string  `json:"path"`
	Model         string  `json:"model"`
	Stream        bool    `json:"stream"`
	Agents        int     `json:"agents"`
	Profile       string  `json:"profile"`
	Turns         int     `json:"turns"`
	MaxToolRounds int     `json:"max_tool_rounds"`
	Sessions      int     `json:"sessions"`
	DurationMs    float64 `json:"duration_ms"`
	Retries       int     `json:"retries"`
	Seed          int64   `json:"seed"`
	// Tools is "in-process" or the --tools-url.
	Tools string `json:"tools"`
	// Headers: names only (values may be secrets).
	Headers []string `json:"headers"`
}

// AgentReport is one entry of "agents".
type AgentReport struct {
	Agent             int              `json:"agent"`
	Profile           string           `json:"profile"`
	SessionsStarted   int64            `json:"sessions_started"`
	SessionsCompleted int64            `json:"sessions_completed"`
	SessionsFailed    int64            `json:"sessions_failed"`
	Turns             int64            `json:"turns"`
	Requests          int64            `json:"requests"`
	Retries           int64            `json:"retries"`
	ToolCalls         int64            `json:"tool_calls"`
	ToolErrors        int64            `json:"tool_errors"`
	Errors            int64            `json:"errors"`
	ErrorsByType      map[string]int64 `json:"errors_by_type"`
	TTFT              hist.Summary     `json:"ttft_ms"`
	E2E               hist.Summary     `json:"e2e_ms"`
}

// SessionsReport is total.sessions.
type SessionsReport struct {
	Started        int64            `json:"started"`
	Completed      int64            `json:"completed"`
	Failed         int64            `json:"failed"`
	Interrupted    int64            `json:"interrupted"`
	FailedByReason map[string]int64 `json:"failed_by_reason"`
}

// ToolsReport is total.tools.
type ToolsReport struct {
	CallsByName map[string]int64        `json:"calls_by_name"`
	Outcomes    map[tools.Outcome]int64 `json:"outcomes"`
}

// TotalReport is "total".
type TotalReport struct {
	Sessions        SessionsReport   `json:"sessions"`
	Turns           int64            `json:"turns"`
	Requests        int64            `json:"requests"`
	OK              int64            `json:"ok"`
	Errors          int64            `json:"errors"`
	Retries         int64            `json:"retries"`
	RetriesByType   map[string]int64 `json:"retries_by_type"`
	ErrorsByType    map[string]int64 `json:"errors_by_type"`
	RetryWait       hist.Summary     `json:"retry_wait_ms"`
	RPS             float64          `json:"rps"`
	InputTokens     int64            `json:"input_tokens"`
	OutputTokens    int64            `json:"output_tokens"`
	ReasoningTokens int64            `json:"reasoning_tokens"`
	TokensEstimated bool             `json:"tokens_estimated"`
	TTFT            hist.Summary     `json:"ttft_ms"`
	ITL             hist.Summary     `json:"itl_ms"`
	E2E             hist.Summary     `json:"e2e_ms"`
	TurnE2E         hist.Summary     `json:"turn_e2e_ms"`
	SessionE2E      hist.Summary     `json:"session_e2e_ms"`
	ToolCalls       int64            `json:"tool_calls"`
	Tools           ToolsReport      `json:"tools"`
}

// Report is the agents report (JSON "version": 1, "kind": "agents").
type Report struct {
	Version     int           `json:"version"`
	Kind        string        `json:"kind"`
	ToolVersion string        `json:"tool_version"`
	Started     string        `json:"started"`
	Finished    string        `json:"finished"`
	Interrupted bool          `json:"interrupted"`
	Config      ReportConfig  `json:"config"`
	Agents      []AgentReport `json:"agents"`
	Total       TotalReport   `json:"total"`
	// AdminStats is the raw body of /admin/stats (or {"error": "..."}).
	AdminStats json.RawMessage `json:"admin_stats"`
}

func clone[K comparable](m map[K]int64) map[K]int64 {
	out := make(map[K]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func agentReport(s *Stats) AgentReport {
	q := s.Requests
	return AgentReport{
		Agent: s.Agent, Profile: string(s.Profile), SessionsStarted: s.SessionsStarted,
		SessionsCompleted: s.SessionsCompleted, SessionsFailed: s.SessionsFailed, Turns: s.Turns,
		Requests: q.Requests, Retries: s.Retries, ToolCalls: s.ToolCalls, ToolErrors: s.ToolErrors,
		Errors: q.Errors, ErrorsByType: clone(q.ErrorsByType), TTFT: q.TTFT.Summary(), E2E: q.E2E.Summary(),
	}
}

func perSecond(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return math.Round(float64(n)/d.Seconds()*1000) / 1000
}

func totalReport(s *Stats, elapsed time.Duration) TotalReport {
	q := s.Requests
	return TotalReport{
		Sessions: SessionsReport{
			Started: s.SessionsStarted, Completed: s.SessionsCompleted, Failed: s.SessionsFailed,
			Interrupted: s.SessionsInterrupted, FailedByReason: clone(s.FailedByReason),
		},
		Turns: s.Turns, Requests: q.Requests, OK: q.OK, Errors: q.Errors, Retries: s.Retries,
		RetriesByType: clone(s.RetriesByType), ErrorsByType: clone(q.ErrorsByType), RetryWait: s.RetryWait.Summary(),
		RPS: perSecond(q.Requests, elapsed), InputTokens: q.InputTokens, OutputTokens: q.OutputTokens,
		ReasoningTokens: q.ReasoningTokens, TokensEstimated: q.TokensEstimated,
		TTFT: q.TTFT.Summary(), ITL: q.ITL.Summary(), E2E: q.E2E.Summary(),
		TurnE2E: s.TurnE2E.Summary(), SessionE2E: s.SessionE2E.Summary(), ToolCalls: s.ToolCalls,
		Tools: ToolsReport{CallsByName: clone(s.CallsByName), Outcomes: clone(s.Outcomes)},
	}
}

// NewReport builds the report of a run.
func NewReport(cfg ReportConfig, res *Result, toolVersion string) *Report {
	if cfg.Headers == nil {
		cfg.Headers = []string{}
	}
	r := &Report{
		Version: loadgen.ReportVersion, Kind: Kind, ToolVersion: toolVersion,
		Started:     res.Started.UTC().Format(time.RFC3339),
		Finished:    res.Finished.UTC().Format(time.RFC3339),
		Interrupted: res.Interrupted, Config: cfg, Agents: make([]AgentReport, 0, len(res.Agents)),
		Total: totalReport(res.Total, res.Elapsed()),
	}
	for _, a := range res.Agents {
		r.Agents = append(r.Agents, agentReport(a))
	}
	return r
}

// FailedPct is sessions.failed / sessions.started in percent.
func (r *Report) FailedPct() float64 {
	s := r.Total.Sessions
	if s.Started == 0 {
		return 0
	}
	return float64(s.Failed) * 100 / float64(s.Started)
}

// Exceeds reports whether the failed share of the sessions is above pct
// (--fail-on-error-pct; pct ≤ 0 is off).
func (r *Report) Exceeds(pct float64) bool {
	return pct > 0 && r.FailedPct() > pct
}

// Write renders the report in format (text, json or csv).
func (r *Report) Write(w io.Writer, format string) error {
	switch format {
	case loadgen.FormatJSON:
		return r.WriteJSON(w)
	case loadgen.FormatCSV:
		return r.WriteCSV(w)
	case loadgen.FormatText:
		return r.WriteText(w)
	}
	return fmt.Errorf("output %q: want text, json or csv", format)
}

// WriteJSON writes the indented JSON report.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// CSVHeader is the fixed header of the CSV report.
var CSVHeader = []string{
	"agent", "profile", "sessions_started", "sessions_completed", "sessions_failed", "turns", "requests", "retries",
	"tool_calls", "tool_errors", "errors", "ttft_p50_ms", "ttft_p99_ms", "e2e_p50_ms", "e2e_p99_ms", "error_types",
}

func fnum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func inum(v int64) string { return strconv.FormatInt(v, 10) }

func sortedKeys[K ~string](m map[K]int64) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func errorTypes(m map[string]int64) string {
	parts := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		parts = append(parts, k+"="+inum(m[k]))
	}
	return strings.Join(parts, ";")
}

func (a *AgentReport) csvRow(agent string) []string {
	return []string{
		agent, a.Profile, inum(a.SessionsStarted), inum(a.SessionsCompleted), inum(a.SessionsFailed), inum(a.Turns),
		inum(a.Requests), inum(a.Retries), inum(a.ToolCalls), inum(a.ToolErrors), inum(a.Errors),
		fnum(a.TTFT.P50), fnum(a.TTFT.P99), fnum(a.E2E.P50), fnum(a.E2E.P99), errorTypes(a.ErrorsByType),
	}
}

// totalRow is the total in the shape of an agent row.
func (r *Report) totalRow() AgentReport {
	t := &r.Total
	var toolErrors int64
	for _, a := range r.Agents {
		toolErrors += a.ToolErrors
	}
	return AgentReport{
		Profile: r.Config.Profile, SessionsStarted: t.Sessions.Started, SessionsCompleted: t.Sessions.Completed,
		SessionsFailed: t.Sessions.Failed, Turns: t.Turns, Requests: t.Requests, Retries: t.Retries,
		ToolCalls: t.ToolCalls, ToolErrors: toolErrors, Errors: t.Errors, ErrorsByType: t.ErrorsByType,
		TTFT: t.TTFT, E2E: t.E2E,
	}
}

// WriteCSV writes the header, one row per agent and the total row
// (agent=total), RFC 4180.
func (r *Report) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	_ = cw.Write(CSVHeader)
	for i := range r.Agents {
		_ = cw.Write(r.Agents[i].csvRow(strconv.Itoa(r.Agents[i].Agent)))
	}
	total := r.totalRow()
	_ = cw.Write(total.csvRow("total"))
	cw.Flush()
	return cw.Error()
}

// errWriter keeps the first write error.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, a ...any) {
	if e.err == nil {
		_, e.err = fmt.Fprintf(e.w, format, a...)
	}
}

// WriteText writes the human report (not a contract).
func (r *Report) WriteText(w io.Writer) error {
	ew := &errWriter{w: w}
	c := &r.Config
	ew.printf("wwz-load agents %s  target %s%s  protocol %s  model %s  stream %v\n", r.ToolVersion, c.Target, c.Path, c.Protocol, c.Model, c.Stream)
	ew.printf("agents %d  profile %s  turns %d  max tool rounds %d  retries %d  seed %d  tools %s\n",
		c.Agents, c.Profile, c.Turns, c.MaxToolRounds, c.Retries, c.Seed, c.Tools)
	ew.printf("started %s  finished %s", r.Started, r.Finished)
	if r.Interrupted {
		ew.printf("  (interrupted)")
	}
	ew.printf("\n\n")
	var tb strings.Builder
	tw := tabwriter.NewWriter(&tb, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(tw, "agent\tprofile\tsessions\tok\tfailed\tturns\trequests\tretries\ttools\ttool err\terrors\tttft p50\te2e p50\t")
	rows := append(slices.Clone(r.Agents), r.totalRow())
	for i := range rows {
		a := &rows[i]
		name := strconv.Itoa(a.Agent)
		if i == len(rows)-1 {
			name = "total"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t\n", name, a.Profile, a.SessionsStarted,
			a.SessionsCompleted, a.SessionsFailed, a.Turns, a.Requests, a.Retries, a.ToolCalls, a.ToolErrors, a.Errors,
			fnum(a.TTFT.P50), fnum(a.E2E.P50))
	}
	_ = tw.Flush()
	ew.printf("%s\n", tb.String())
	t := &r.Total
	ew.printf("sessions: %d started  %d completed  %d failed  %d interrupted\n",
		t.Sessions.Started, t.Sessions.Completed, t.Sessions.Failed, t.Sessions.Interrupted)
	ew.printf("requests: %d (%d ok, %d errors, %d retries), %s req/s\n", t.Requests, t.OK, t.Errors, t.Retries, fnum(t.RPS))
	est := ""
	if t.TokensEstimated {
		est = " (estimated)"
	}
	ew.printf("tokens: input %d  output %d%s  reasoning %d\n\n", t.InputTokens, t.OutputTokens, est, t.ReasoningTokens)
	tb.Reset()
	tw = tabwriter.NewWriter(&tb, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(tw, "ms\tcount\tmean\tp50\tp90\tp99\tmax\t")
	for _, d := range []struct {
		name string
		s    hist.Summary
	}{{"ttft", t.TTFT}, {"itl", t.ITL}, {"e2e", t.E2E}, {"turn e2e", t.TurnE2E}, {"session e2e", t.SessionE2E}, {"retry wait", t.RetryWait}} {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t\n", d.name, d.s.Count, fnum(d.s.Mean), fnum(d.s.P50), fnum(d.s.P90), fnum(d.s.P99), fnum(d.s.Max))
	}
	_ = tw.Flush()
	ew.printf("%s", tb.String())
	ew.printf("\ntools: %d calls", t.ToolCalls)
	for _, k := range sortedKeys(t.Tools.Outcomes) {
		ew.printf("  %s %d", k, t.Tools.Outcomes[k])
	}
	ew.printf("\n")
	for _, k := range sortedKeys(t.Tools.CallsByName) {
		ew.printf("  %8d  %s\n", t.Tools.CallsByName[k], k)
	}
	for _, sec := range []struct {
		title string
		m     map[string]int64
	}{{"failed sessions by reason", t.Sessions.FailedByReason}, {"errors by type", t.ErrorsByType}, {"retries by type", t.RetriesByType}} {
		if len(sec.m) == 0 {
			continue
		}
		ew.printf("\n%s:\n", sec.title)
		for _, k := range sortedKeys(sec.m) {
			ew.printf("  %8d  %s\n", sec.m[k], k)
		}
	}
	return ew.err
}
