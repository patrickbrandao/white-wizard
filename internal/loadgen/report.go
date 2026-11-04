package loadgen

import (
	"cmp"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen/hist"
)

// ReportVersion is the version of the JSON report (spec 015, "Saída").
const ReportVersion = 1

// Output formats of --output.
const (
	FormatText = "text"
	FormatJSON = "json"
	FormatCSV  = "csv"
)

// ReportConfig is the "config" object of the JSON report. Times are in ms.
type ReportConfig struct {
	Target         string    `json:"target"`
	Protocol       string    `json:"protocol"`
	Path           string    `json:"path"`
	Model          string    `json:"model"`
	Stream         bool      `json:"stream"`
	MaxTokens      int       `json:"max_tokens"`
	Mode           Mode      `json:"mode"`
	RPS            float64   `json:"rps"`
	Concurrency    int       `json:"concurrency"`
	Ramp           string    `json:"ramp"`
	SweepRates     []float64 `json:"sweep_rates"`
	StepDurationMs float64   `json:"step_duration_ms"`
	DurationMs     float64   `json:"duration_ms"`
	Requests       int64     `json:"requests"`
	WarmupMs       float64   `json:"warmup_ms"`
	TimeoutMs      float64   `json:"timeout_ms"`
	Seed           int64     `json:"seed"`
	Scenario       string    `json:"scenario"`
	Instances      int       `json:"instances"`
	// Headers: names only (values may be secrets).
	Headers []string `json:"headers"`
}

// StepReport is a step (Step = 1, 2, …) or the total (Step = "total") with
// its summary.
type StepReport struct {
	Step             any              `json:"step"`
	TargetRPS        float64          `json:"target_rps"`
	Concurrency      int              `json:"concurrency"`
	StartMs          float64          `json:"start_ms"`
	DurationMs       float64          `json:"duration_ms"`
	Requests         int64            `json:"requests"`
	OK               int64            `json:"ok"`
	Errors           int64            `json:"errors"`
	Dropped          int64            `json:"dropped"`
	RPS              float64          `json:"rps"`
	OKRPS            float64          `json:"ok_rps"`
	InputTokens      int64            `json:"input_tokens"`
	OutputTokens     int64            `json:"output_tokens"`
	ReasoningTokens  int64            `json:"reasoning_tokens"`
	TokensEstimated  bool             `json:"tokens_estimated"`
	InputTokensPerS  float64          `json:"input_tokens_per_s"`
	OutputTokensPerS float64          `json:"output_tokens_per_s"`
	TTFT             hist.Summary     `json:"ttft_ms"`
	ITL              hist.Summary     `json:"itl_ms"`
	E2E              hist.Summary     `json:"e2e_ms"`
	ScheduleLag      hist.Summary     `json:"schedule_lag_ms"`
	ErrorsByType     map[string]int64 `json:"errors_by_type"`
}

// ErrorPct is errors / requests in percent (0 without requests).
func (s *StepReport) ErrorPct() float64 {
	if s.Requests == 0 {
		return 0
	}
	return float64(s.Errors) * 100 / float64(s.Requests)
}

// Report is the wwz-load report (JSON "version": 1).
type Report struct {
	Version     int          `json:"version"`
	ToolVersion string       `json:"tool_version"`
	Started     string       `json:"started"`
	Finished    string       `json:"finished"`
	Interrupted bool         `json:"interrupted"`
	Config      ReportConfig `json:"config"`
	Steps       []StepReport `json:"steps"`
	Total       StepReport   `json:"total"`
	// Conversations (task 044) and Instances (task 045): null when absent.
	Conversations any `json:"conversations"`
	Instances     any `json:"instances"`
	// AdminStats is the raw body of /admin/stats (or {"error": "..."}).
	AdminStats json.RawMessage `json:"admin_stats"`
}

func perSecond(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return round3(float64(n) / d.Seconds())
}

// Summarize turns a StepResult into its report form; label is the step
// number or "total".
func Summarize(sr StepResult, label any) StepReport {
	s := sr.Stats
	errs := make(map[string]int64, len(s.ErrorsByType))
	for k, v := range s.ErrorsByType {
		errs[k] = v
	}
	return StepReport{
		Step: label, TargetRPS: round3(sr.TargetRPS), Concurrency: sr.Concurrency,
		StartMs: hist.Ms(sr.Start), DurationMs: hist.Ms(sr.Duration),
		Requests: s.Requests, OK: s.OK, Errors: s.Errors, Dropped: s.Dropped,
		RPS: perSecond(s.Requests, sr.Duration), OKRPS: perSecond(s.OK, sr.Duration),
		InputTokens: s.InputTokens, OutputTokens: s.OutputTokens, ReasoningTokens: s.ReasoningTokens,
		TokensEstimated: s.TokensEstimated,
		InputTokensPerS: perSecond(s.InputTokens, sr.Duration), OutputTokensPerS: perSecond(s.OutputTokens, sr.Duration),
		TTFT: s.TTFT.Summary(), ITL: s.ITL.Summary(), E2E: s.E2E.Summary(), ScheduleLag: s.ScheduleLag.Summary(),
		ErrorsByType: errs,
	}
}

// NewReport builds the report of a run.
func NewReport(cfg ReportConfig, res *RunResult, toolVersion string) *Report {
	if cfg.SweepRates == nil {
		cfg.SweepRates = []float64{}
	}
	if cfg.Headers == nil {
		cfg.Headers = []string{}
	}
	r := &Report{
		Version: ReportVersion, ToolVersion: toolVersion,
		Started:     res.Started.UTC().Format(time.RFC3339),
		Finished:    res.Finished.UTC().Format(time.RFC3339),
		Interrupted: res.Interrupted, Config: cfg, Steps: []StepReport{},
		Total: Summarize(res.Total, "total"),
	}
	for _, st := range res.Steps {
		r.Steps = append(r.Steps, Summarize(st, st.Index))
	}
	return r
}

// Write renders the report in format (text, json or csv).
func (r *Report) Write(w io.Writer, format string) error {
	switch format {
	case FormatJSON:
		return r.WriteJSON(w)
	case FormatCSV:
		return r.WriteCSV(w)
	case FormatText:
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
	"step", "target_rps", "concurrency", "start_ms", "duration_ms", "requests", "ok", "errors", "dropped", "rps",
	"output_tokens_per_s",
	"ttft_p50_ms", "ttft_p90_ms", "ttft_p99_ms", "ttft_max_ms", "ttft_mean_ms",
	"itl_p50_ms", "itl_p90_ms", "itl_p99_ms", "itl_max_ms", "itl_mean_ms",
	"e2e_p50_ms", "e2e_p90_ms", "e2e_p99_ms", "e2e_max_ms", "e2e_mean_ms",
	"schedule_lag_p99_ms", "error_types",
}

func fnum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func inum(v int64) string { return strconv.FormatInt(v, 10) }

func dist(s hist.Summary) []string {
	return []string{fnum(s.P50), fnum(s.P90), fnum(s.P99), fnum(s.Max), fnum(s.Mean)}
}

// sortedErrors returns the error classes ordered by type.
func sortedErrors(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (s *StepReport) csvRow() []string {
	types := make([]string, 0, len(s.ErrorsByType))
	for _, k := range sortedErrors(s.ErrorsByType) {
		types = append(types, k+"="+inum(s.ErrorsByType[k]))
	}
	row := []string{
		fmt.Sprint(s.Step), fnum(s.TargetRPS), strconv.Itoa(s.Concurrency), fnum(s.StartMs), fnum(s.DurationMs),
		inum(s.Requests), inum(s.OK), inum(s.Errors), inum(s.Dropped), fnum(s.RPS), fnum(s.OutputTokensPerS),
	}
	row = append(row, dist(s.TTFT)...)
	row = append(row, dist(s.ITL)...)
	row = append(row, dist(s.E2E)...)
	return append(row, fnum(s.ScheduleLag.P99), strings.Join(types, ";"))
}

// WriteCSV writes the header, one row per step and the total row (RFC 4180).
func (r *Report) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	_ = cw.Write(CSVHeader)
	for i := range r.Steps {
		_ = cw.Write(r.Steps[i].csvRow())
	}
	_ = cw.Write(r.Total.csvRow())
	cw.Flush()
	return cw.Error()
}

// WriteText writes the human report (not a contract): header, a table per
// step, and the total with every distribution, tokens/s and errors.
func (r *Report) WriteText(w io.Writer) error {
	ew := &errWriter{w: w}
	c := &r.Config
	ew.printf("wwz-load %s  target %s%s  protocol %s  model %s  stream %v\n", r.ToolVersion, c.Target, c.Path, c.Protocol, c.Model, c.Stream)
	ew.printf("mode %s  seed %d  started %s  finished %s", modeText(c), c.Seed, r.Started, r.Finished)
	if r.Interrupted {
		ew.printf("  (interrupted)")
	}
	ew.printf("\n\n")
	if len(r.Steps) > 0 {
		tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', tabwriter.AlignRight)
		_, _ = fmt.Fprintln(tw, "step\ttarget\treq/s\tok\terrors\tdropped\tttft p50\tttft p99\titl p50\titl p99\te2e p50\te2e p99\t")
		for i := range r.Steps {
			s := &r.Steps[i]
			_, _ = fmt.Fprintf(tw, "%v\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t\n", s.Step, fnum(s.TargetRPS), fnum(s.RPS), s.OK, s.Errors, s.Dropped,
				fnum(s.TTFT.P50), fnum(s.TTFT.P99), fnum(s.ITL.P50), fnum(s.ITL.P99), fnum(s.E2E.P50), fnum(s.E2E.P99))
		}
		_ = tw.Flush()
		ew.printf("\n")
	}
	t := &r.Total
	ew.printf("total: %d requests in %s ms, %s req/s (%s ok/s)\n", t.Requests, fnum(t.DurationMs), fnum(t.RPS), fnum(t.OKRPS))
	ew.printf("  ok %d  errors %d (%s%%)  dropped %d\n", t.OK, t.Errors, fnum(round3(t.ErrorPct())), t.Dropped)
	est := ""
	if t.TokensEstimated {
		est = " (estimated)"
	}
	ew.printf("  tokens/s: output %s  input %s%s; reasoning tokens %d\n\n", fnum(t.OutputTokensPerS), fnum(t.InputTokensPerS), est, t.ReasoningTokens)
	tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(tw, "ms\tcount\tmean\tp50\tp90\tp99\tmax\t")
	for _, d := range []struct {
		name string
		s    hist.Summary
	}{{"ttft", t.TTFT}, {"itl", t.ITL}, {"e2e", t.E2E}, {"schedule lag", t.ScheduleLag}} {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t\n", d.name, d.s.Count, fnum(d.s.Mean), fnum(d.s.P50), fnum(d.s.P90), fnum(d.s.P99), fnum(d.s.Max))
	}
	_ = tw.Flush()
	if len(t.ErrorsByType) > 0 {
		ew.printf("\nerrors by type:\n")
		keys := sortedErrors(t.ErrorsByType)
		slices.SortStableFunc(keys, func(a, b string) int { return cmp.Compare(t.ErrorsByType[b], t.ErrorsByType[a]) })
		for _, k := range keys {
			ew.printf("  %8d  %s\n", t.ErrorsByType[k], k)
		}
	}
	return ew.err
}

func modeText(c *ReportConfig) string {
	switch c.Mode {
	case ModeRPS:
		return "rps " + fnum(c.RPS)
	case ModeConcurrency:
		return "concurrency " + strconv.Itoa(c.Concurrency)
	case ModeRamp:
		return "ramp " + c.Ramp
	}
	rates := make([]string, len(c.SweepRates))
	for i, v := range c.SweepRates {
		rates[i] = fnum(v)
	}
	return string(c.Mode) + " " + strings.Join(rates, ",")
}

// errWriter keeps the first write error.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

func (e *errWriter) printf(format string, a ...any) { _, _ = fmt.Fprintf(e, format, a...) }

// maxAdminBody caps the /admin/stats body kept in the report.
const maxAdminBody = 32 << 20

// FetchAdminStats returns the raw body of GET base/admin/stats with the
// bearer token (methodology item 8), or {"error": "..."} on failure.
func FetchAdminStats(ctx context.Context, hc *http.Client, base, token string) json.RawMessage {
	body, err := fetchAdminStats(ctx, hc, base, token)
	if err != nil {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return b
	}
	return body
}

func fetchAdminStats(ctx context.Context, hc *http.Client, base, token string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/admin/stats", http.NoBody)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin stats: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("admin stats: invalid JSON")
	}
	return body, nil
}

// Exceeds reports whether the error share of the total is above pct
// (--fail-on-error-pct; pct ≤ 0 is off).
func (r *Report) Exceeds(pct float64) bool {
	return pct > 0 && !math.IsNaN(pct) && r.Total.ErrorPct() > pct
}
