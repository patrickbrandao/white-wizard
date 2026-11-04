// Package top is the core of `wwz-load top` (spec 015, ADR 0036): it samples
// GET /admin/stats from one or more wwz control planes, turns the counters
// into rates, merges the instances and renders a frame as text or JSON. The
// terminal handling (raw mode, alternate screen) stays in cmd/wwz-load; here
// the interactive loop only sees an io.Reader of keys and an io.Writer.
package top

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Stats is the part of the Stats schema of cmd/wwz/openapi.yaml read by top.
type Stats struct {
	UptimeS float64      `json:"uptime_s"`
	Pod     string       `json:"pod"`
	Store   string       `json:"store"`
	Totals  StatsTotals  `json:"totals"`
	ByModel []ModelStats `json:"by_model"`
	ByKey   []KeyStats   `json:"by_key"`
}

// StatsTotals mirrors StatsTotals of the openapi.
type StatsTotals struct {
	Requests       int64            `json:"requests"`
	InFlight       int64            `json:"in_flight"`
	StreamsOpen    int64            `json:"streams_open"`
	ErrorsInjected int64            `json:"errors_injected"`
	RateLimited    int64            `json:"rate_limited"`
	Blocked        int64            `json:"blocked"`
	ByStatus       map[string]int64 `json:"by_status"`
	ErrorsByKind   map[string]int64 `json:"errors_by_kind"`
}

// ModelStats mirrors ModelStats of the openapi.
type ModelStats struct {
	Driver    string  `json:"driver"`
	Model     string  `json:"model"`
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	TTFTP50MS float64 `json:"ttft_p50_ms"`
	TTFTP99MS float64 `json:"ttft_p99_ms"`
	E2EP50MS  float64 `json:"e2e_p50_ms"`
	E2EP99MS  float64 `json:"e2e_p99_ms"`
}

// KeyStats mirrors KeyStats of the openapi.
type KeyStats struct {
	Key      string  `json:"key"`
	Status   string  `json:"status"`
	Requests int64   `json:"requests"`
	SpendUSD float64 `json:"spend_usd"`
}

// AuthError is a 401 from a control plane (exit 1 in the CLI).
type AuthError struct{ Admin string }

func (e *AuthError) Error() string { return "token recusado por " + e.Admin }

// ErrNoInstance means no control plane answered the first sample.
var ErrNoInstance = errors.New("nenhuma instância respondeu")

// Sort orders of the model table, in the order the `s` key cycles them.
var Sorts = []string{"requests", "errors", "ttft", "e2e"}

// ValidSort reports whether s is one of Sorts.
func ValidSort(s string) bool {
	for _, x := range Sorts {
		if x == s {
			return true
		}
	}
	return false
}

// nextSort is the sort after s in Sorts (wrapping).
func nextSort(s string) string {
	for i, x := range Sorts {
		if x == s {
			return Sorts[(i+1)%len(Sorts)]
		}
	}
	return Sorts[0]
}

// Config drives a Monitor.
type Config struct {
	Admins   []string      // control planes, without trailing slash
	Token    string        // bearer
	Interval time.Duration // between samples
	Timeout  time.Duration // ceiling of each GET (0 = none)
	Sort     string        // one of Sorts ("" = requests)
	Rows     int           // rows per table (≤ 0 = no limit)
	Client   *http.Client  // nil = http.DefaultClient
	Now      func() time.Time
	// Sleep waits d or until ctx ends (false). nil = timer.
	Sleep func(ctx context.Context, d time.Duration) bool
}

// result is one instance in one round.
type result struct {
	admin string
	st    *Stats
	err   error
}

// point is the last good sample of an instance.
type point struct {
	at time.Time
	st *Stats
}

// Monitor keeps the last good sample of each instance to compute rates.
type Monitor struct {
	cfg  Config
	last map[string]point
}

// New returns a Monitor with the defaults filled in.
func New(cfg Config) *Monitor {
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.Sort == "" {
		cfg.Sort = Sorts[0]
	}
	for i, a := range cfg.Admins {
		cfg.Admins[i] = strings.TrimRight(a, "/")
	}
	return &Monitor{cfg: cfg, last: map[string]point{}}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// fetch does one GET /admin/stats.
func (m *Monitor) fetch(ctx context.Context, admin string) (*Stats, error) {
	if m.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.cfg.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, admin+"/admin/stats", nil)
	if err != nil {
		return nil, err
	}
	if m.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.Token)
	}
	resp, err := m.cfg.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, &AuthError{Admin: admin}
	default:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var st Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &st, nil
}

// sample fetches every instance in parallel. A 401 from any instance is
// returned as *AuthError.
func (m *Monitor) sample(ctx context.Context) ([]result, error) {
	out := make([]result, len(m.cfg.Admins))
	var wg sync.WaitGroup
	for i, a := range m.cfg.Admins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := m.fetch(ctx, a)
			out[i] = result{admin: a, st: st, err: err}
		}()
	}
	wg.Wait()
	for _, r := range out {
		var ae *AuthError
		if errors.As(r.err, &ae) {
			return nil, ae
		}
	}
	return out, nil
}

// Step samples every instance and returns the frame built against the
// previous good samples; then it records the new ones.
func (m *Monitor) Step(ctx context.Context) (*Frame, error) {
	res, err := m.sample(ctx)
	if err != nil {
		return nil, err
	}
	at := m.cfg.Now()
	f := build(m.last, res, at, m.cfg)
	for _, r := range res {
		if r.err == nil {
			m.last[r.admin] = point{at: at, st: r.st}
		}
	}
	return f, nil
}

// first is Step for the first round: no instance answering is an error.
func (m *Monitor) first(ctx context.Context) (*Frame, error) {
	f, err := m.Step(ctx)
	if err != nil {
		return nil, err
	}
	var msgs []string
	for _, in := range f.Instances {
		if in.OK {
			return f, nil
		}
		msgs = append(msgs, in.Admin+": "+in.Error)
	}
	return nil, fmt.Errorf("%w: %s", ErrNoInstance, strings.Join(msgs, "; "))
}

// Once takes two samples separated by Interval and writes one frame (text or
// JSON). ctx ending during the wait prints the first frame (rates null).
func (m *Monitor) Once(ctx context.Context, w io.Writer, jsonOut bool, width int) error {
	f, err := m.first(ctx)
	if err != nil {
		return err
	}
	if m.cfg.Sleep(ctx, m.cfg.Interval) {
		if f, err = m.Step(ctx); err != nil {
			return err
		}
	}
	if jsonOut {
		return WriteJSON(w, f)
	}
	_, err = io.WriteString(w, Render(f, width, ""))
	return err
}

// Stream writes one text frame per interval, separated by a blank line,
// until ctx ends (not a TTY).
func (m *Monitor) Stream(ctx context.Context, w io.Writer, width int) error {
	f, err := m.first(ctx)
	for {
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, Render(f, width, "")); err != nil {
			return err
		}
		if !m.cfg.Sleep(ctx, m.cfg.Interval) {
			return nil
		}
		if f, err = m.Step(ctx); err == nil {
			_, err = io.WriteString(w, "\n")
		}
	}
}

// Screen is the terminal seen by Interactive.
type Screen struct {
	Keys  io.Reader  // raw keys
	Out   io.Writer  // ANSI output; lines end in \r\n (raw mode)
	Width func() int // current width
}

// Key bindings of the interactive mode.
const (
	keyQuit  = 'q'
	keyCtrlC = 3
	keySort  = 's'
	keyPause = 'p'
)

// Interactive redraws a frame every Interval until `q`, Ctrl-C or ctx ends.
// `s` cycles the sort order and `p` pauses/resumes sampling.
func (m *Monitor) Interactive(ctx context.Context, scr Screen) error {
	f, err := m.first(ctx)
	if err != nil {
		return err
	}
	keys := make(chan byte, 16)
	go readKeys(scr.Keys, keys)
	tick := time.NewTicker(m.cfg.Interval)
	defer tick.Stop()
	paused, redraw := false, true
	for {
		if redraw {
			if err := m.draw(scr, f, paused); err != nil {
				return err
			}
		}
		redraw = true
		select {
		case <-ctx.Done():
			return nil
		case k, ok := <-keys:
			switch {
			case !ok, k == keyQuit, k == keyCtrlC:
				return nil
			case k == keySort:
				m.cfg.Sort = nextSort(m.cfg.Sort)
				f.Sort = m.cfg.Sort
				sortModels(f.ByModel, f.Sort)
			case k == keyPause:
				paused = !paused
			default:
				redraw = false
			}
		case <-tick.C:
			if paused {
				redraw = false
				continue
			}
			if f, err = m.Step(ctx); err != nil {
				return err
			}
		}
	}
}

func readKeys(r io.Reader, keys chan<- byte) {
	defer close(keys)
	buf := make([]byte, 32)
	for {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			keys <- b
		}
		if err != nil {
			return
		}
	}
}

// ANSI sequences of the interactive screen.
const (
	ansiHome  = "\x1b[H"
	ansiClear = "\x1b[2J"
)

func (m *Monitor) draw(scr Screen, f *Frame, paused bool) error {
	footer := "q quit  s sort: " + f.Sort + "  p pause"
	if paused {
		footer = "q quit  s sort: " + f.Sort + "  p resume  [PAUSED]"
	}
	text := Render(f, scr.Width(), footer)
	_, err := io.WriteString(scr.Out, ansiHome+ansiClear+strings.ReplaceAll(text, "\n", "\r\n"))
	return err
}
