package top

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// Frame is one screen of `wwz-load top`; its JSON is the `kind: "top"`
// document of spec 015. Rates are nil (JSON null, text "—") until an
// instance has two samples.
type Frame struct {
	Version    int        `json:"version"`
	Kind       string     `json:"kind"`
	SampledAt  time.Time  `json:"sampled_at"`
	IntervalMS int64      `json:"interval_ms"`
	Instances  []Instance `json:"instances"`
	Totals     Totals     `json:"totals"`
	ByModel    []ModelRow `json:"by_model"`
	ByKey      []KeyRow   `json:"by_key"`

	Store   string  `json:"-"` // store kind(s) of the instances that answered
	UptimeS float64 `json:"-"` // largest uptime among them
	Sort    string  `json:"-"` // order of ByModel
	Rows    int     `json:"-"` // rows per table on output (≤ 0 = all)
}

// Instance is one --admin in the frame.
type Instance struct {
	Admin string `json:"admin"`
	Pod   string `json:"pod"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Totals are the counters summed over the instances.
type Totals struct {
	Requests        int64                `json:"requests"`
	RPS             *float64             `json:"rps"`
	InFlight        int64                `json:"in_flight"`
	StreamsOpen     int64                `json:"streams_open"`
	ErrorsPerS      *float64             `json:"errors_per_s"`
	RateLimitedPerS *float64             `json:"rate_limited_per_s"`
	BlockedPerS     *float64             `json:"blocked_per_s"`
	ErrorsByKind    map[string]KindCount `json:"errors_by_kind"`
	ByStatus        map[string]int64     `json:"by_status"`
}

// KindCount is one injected error kind.
type KindCount struct {
	Count int64    `json:"count"`
	PerS  *float64 `json:"per_s"`
}

// ModelRow is one (driver, model) merged over the instances; percentiles are
// the maximum among them.
type ModelRow struct {
	Driver    string   `json:"driver"`
	Model     string   `json:"model"`
	Requests  int64    `json:"requests"`
	RPS       *float64 `json:"rps"`
	ErrorPct  *float64 `json:"error_pct"`
	TTFTP50MS float64  `json:"ttft_p50_ms"`
	TTFTP99MS float64  `json:"ttft_p99_ms"`
	E2EP50MS  float64  `json:"e2e_p50_ms"`
	E2EP99MS  float64  `json:"e2e_p99_ms"`

	errors          int64
	rps, dReq, dErr float64
}

// KeyRow is one key merged over the instances.
type KeyRow struct {
	Key      string   `json:"key"`
	Status   string   `json:"status"`
	Requests int64    `json:"requests"`
	RPS      *float64 `json:"rps"`
	SpendUSD float64  `json:"spend_usd"`

	rps float64
}

type modelID struct{ driver, model string }

// delta is cur-old per second; a counter that went down (reset or restart)
// gives 0.
func delta(cur, old int64, dt float64) float64 {
	if cur < old {
		return 0
	}
	return float64(cur-old) / dt
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

func ptr(x float64) *float64 {
	v := round3(x)
	return &v
}

// build merges one round of results against the last good samples.
func build(last map[string]point, res []result, at time.Time, cfg Config) *Frame {
	f := &Frame{
		Version: 1, Kind: "top", SampledAt: at.UTC(), IntervalMS: cfg.Interval.Milliseconds(),
		Sort: cfg.Sort, Rows: cfg.Rows,
		Totals: Totals{ErrorsByKind: map[string]KindCount{}, ByStatus: map[string]int64{}},
	}
	var rps, errs, limited, blocked float64
	kindRate := map[string]float64{}
	known := false
	models := map[modelID]*ModelRow{}
	keys := map[string]*KeyRow{}
	stores := map[string]bool{}
	for _, r := range res {
		in := Instance{Admin: r.admin, OK: r.err == nil}
		if r.err != nil {
			in.Error = r.err.Error()
			f.Instances = append(f.Instances, in)
			continue
		}
		st := r.st
		in.Pod = st.Pod
		f.Instances = append(f.Instances, in)
		stores[st.Store] = true
		f.UptimeS = math.Max(f.UptimeS, st.UptimeS)

		prev, has := last[r.admin]
		dt := at.Sub(prev.at).Seconds()
		has = has && dt > 0
		old := &Stats{}
		if has {
			old = prev.st
			known = true
		} else {
			dt = 1
		}
		t, o := st.Totals, old.Totals
		f.Totals.Requests += t.Requests
		f.Totals.InFlight += t.InFlight
		f.Totals.StreamsOpen += t.StreamsOpen
		for s, n := range t.ByStatus {
			f.Totals.ByStatus[s] += n
		}
		for k, n := range t.ErrorsByKind {
			kc := f.Totals.ErrorsByKind[k]
			kc.Count += n
			f.Totals.ErrorsByKind[k] = kc
			if has {
				kindRate[k] += delta(n, o.ErrorsByKind[k], dt)
			}
		}
		if has {
			rps += delta(t.Requests, o.Requests, dt)
			errs += delta(t.ErrorsInjected, o.ErrorsInjected, dt)
			limited += delta(t.RateLimited, o.RateLimited, dt)
			blocked += delta(t.Blocked, o.Blocked, dt)
		}
		mergeModels(models, st, old, has, dt)
		mergeKeys(keys, st, old, has, dt)
	}
	if known {
		f.Totals.RPS, f.Totals.ErrorsPerS = ptr(rps), ptr(errs)
		f.Totals.RateLimitedPerS, f.Totals.BlockedPerS = ptr(limited), ptr(blocked)
		for k, kc := range f.Totals.ErrorsByKind {
			kc.PerS = ptr(kindRate[k])
			f.Totals.ErrorsByKind[k] = kc
		}
	}
	f.ByModel = make([]ModelRow, 0, len(models))
	for _, m := range models {
		if known {
			m.RPS = ptr(m.rps)
			pct := 0.0
			if m.dReq > 0 {
				pct = m.dErr / m.dReq * 100
			}
			m.ErrorPct = ptr(pct)
		}
		f.ByModel = append(f.ByModel, *m)
	}
	sortModels(f.ByModel, f.Sort)
	f.ByKey = make([]KeyRow, 0, len(keys))
	for _, k := range keys {
		if known {
			k.RPS = ptr(k.rps)
		}
		f.ByKey = append(f.ByKey, *k)
	}
	slices.SortFunc(f.ByKey, func(a, b KeyRow) int {
		if c := cmp.Compare(b.rps, a.rps); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Requests, a.Requests); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	names := make([]string, 0, len(stores))
	for s := range stores {
		names = append(names, s)
	}
	slices.Sort(names)
	f.Store = strings.Join(names, ",")
	return f
}

func mergeModels(rows map[modelID]*ModelRow, st, old *Stats, has bool, dt float64) {
	prev := map[modelID]ModelStats{}
	for _, m := range old.ByModel {
		prev[modelID{m.Driver, m.Model}] = m
	}
	for _, m := range st.ByModel {
		id := modelID{m.Driver, m.Model}
		row := rows[id]
		if row == nil {
			row = &ModelRow{Driver: m.Driver, Model: m.Model}
			rows[id] = row
		}
		row.Requests += m.Requests
		row.errors += m.Errors
		row.TTFTP50MS = math.Max(row.TTFTP50MS, m.TTFTP50MS)
		row.TTFTP99MS = math.Max(row.TTFTP99MS, m.TTFTP99MS)
		row.E2EP50MS = math.Max(row.E2EP50MS, m.E2EP50MS)
		row.E2EP99MS = math.Max(row.E2EP99MS, m.E2EP99MS)
		if has {
			p := prev[id]
			dReq, dErr := delta(m.Requests, p.Requests, 1), delta(m.Errors, p.Errors, 1)
			row.rps += dReq / dt
			row.dReq += dReq
			row.dErr += dErr
		}
	}
}

// mergeKeys sums the keys of instances with their own store (memory); with a
// shared store (valkey) every instance reports the same cluster-wide
// numbers, so the maximum is taken instead of the sum.
func mergeKeys(rows map[string]*KeyRow, st, old *Stats, has bool, dt float64) {
	prev := map[string]KeyStats{}
	for _, k := range old.ByKey {
		prev[k.Key] = k
	}
	shared := st.Store == "valkey"
	for _, k := range st.ByKey {
		row := rows[k.Key]
		if row == nil {
			row = &KeyRow{Key: k.Key, Status: k.Status}
			rows[k.Key] = row
		}
		if row.Status == "" {
			row.Status = k.Status
		}
		r := 0.0
		if has {
			r = delta(k.Requests, prev[k.Key].Requests, dt)
		}
		if shared {
			row.Requests = max(row.Requests, k.Requests)
			row.SpendUSD = math.Max(row.SpendUSD, k.SpendUSD)
			row.rps = math.Max(row.rps, r)
			continue
		}
		row.Requests += k.Requests
		row.SpendUSD += k.SpendUSD
		row.rps += r
	}
}

// sortModels orders the model table by sort, descending, then by
// driver/model.
func sortModels(rows []ModelRow, sort string) {
	val := func(m ModelRow) float64 {
		switch sort {
		case "errors":
			if m.ErrorPct != nil {
				return *m.ErrorPct
			}
			if m.Requests == 0 {
				return 0
			}
			return float64(m.errors) / float64(m.Requests) * 100
		case "ttft":
			return m.TTFTP99MS
		case "e2e":
			return m.E2EP99MS
		default:
			if m.RPS != nil {
				return *m.RPS
			}
			return float64(m.Requests)
		}
	}
	slices.SortStableFunc(rows, func(a, b ModelRow) int {
		if c := cmp.Compare(val(b), val(a)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Driver, b.Driver); c != 0 {
			return c
		}
		return strings.Compare(a.Model, b.Model)
	})
}

func limit[T any](rows []T, n int) []T {
	if n > 0 && len(rows) > n {
		return rows[:n]
	}
	return rows
}

// WriteJSON writes the frame as the `kind: "top"` JSON of spec 015, with
// --rows applied to by_model and by_key.
func WriteJSON(w io.Writer, f *Frame) error {
	out := *f
	out.ByModel = limit(f.ByModel, f.Rows)
	out.ByKey = limit(f.ByKey, f.Rows)
	b, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// dash is shown for a rate not known yet.
const dash = "—"

func fmtRate(p *float64) string {
	if p == nil {
		return dash
	}
	return fmt.Sprintf("%.1f", *p)
}

func fmtMS(x float64) string { return fmt.Sprintf("%.1f", x) }

// Render is the text frame without ANSI: lines end in "\n" and are cut to
// width runes (≤ 0 = no cut). footer, when set, is the last line.
func Render(f *Frame, width int, footer string) string {
	var b strings.Builder
	ok := 0
	for _, in := range f.Instances {
		if in.OK {
			ok++
		}
	}
	head := fmt.Sprintf("wwz-load top  %s  instances %d/%d  store %s  uptime %s",
		f.SampledAt.Format("15:04:05"), ok, len(f.Instances), f.Store,
		time.Duration(f.UptimeS*float64(time.Second)).Round(time.Second))
	if len(f.Instances) == 1 && f.Instances[0].Pod != "" {
		head += "  pod " + f.Instances[0].Pod
	}
	b.WriteString(head + "\n")
	t := f.Totals
	fmt.Fprintf(&b, "req/s %s  in_flight %d  streams_open %d  errors/s %s  rate_limited/s %s  blocked/s %s  requests %d\n",
		fmtRate(t.RPS), t.InFlight, t.StreamsOpen, fmtRate(t.ErrorsPerS), fmtRate(t.RateLimitedPerS), fmtRate(t.BlockedPerS), t.Requests)

	codes := make([]string, 0, len(t.ByStatus))
	for c := range t.ByStatus {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	status := "status"
	for _, c := range codes {
		status += fmt.Sprintf(" %s=%d", c, t.ByStatus[c])
	}
	b.WriteString(status + "\n\n")

	kinds := make([]string, 0, len(t.ErrorsByKind))
	for k := range t.ErrorsByKind {
		kinds = append(kinds, k)
	}
	slices.SortFunc(kinds, func(a, c string) int {
		if d := cmp.Compare(t.ErrorsByKind[c].Count, t.ErrorsByKind[a].Count); d != 0 {
			return d
		}
		return strings.Compare(a, c)
	})
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ERROR KIND\tCOUNT\t/S")
	for _, k := range limit(kinds, f.Rows) {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\n", k, t.ErrorsByKind[k].Count, fmtRate(t.ErrorsByKind[k].PerS))
	}
	_ = tw.Flush()
	b.WriteString("\n")
	_, _ = fmt.Fprintf(tw, "DRIVER\tMODEL\tREQ/S\tERR%%\tTTFT P50\tTTFT P99\tE2E P50\tE2E P99\t(sort: %s)\n", f.Sort)
	for _, m := range limit(f.ByModel, f.Rows) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", m.Driver, m.Model, fmtRate(m.RPS), fmtRate(m.ErrorPct),
			fmtMS(m.TTFTP50MS), fmtMS(m.TTFTP99MS), fmtMS(m.E2EP50MS), fmtMS(m.E2EP99MS))
	}
	_ = tw.Flush()
	b.WriteString("\n")
	_, _ = fmt.Fprintln(tw, "KEY\tSTATUS\tREQ/S\tREQUESTS\tSPEND USD")
	for _, k := range limit(f.ByKey, f.Rows) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%.4f\n", k.Key, k.Status, fmtRate(k.RPS), k.Requests, k.SpendUSD)
	}
	_ = tw.Flush()
	for _, in := range f.Instances {
		if !in.OK {
			fmt.Fprintf(&b, "unavailable: %s: %s\n", in.Admin, in.Error)
		}
	}
	if footer != "" {
		b.WriteString("\n" + footer + "\n")
	}
	return cut(b.String(), width)
}

// cut trims every line to width runes and the trailing blanks left by the
// tabwriter.
func cut(s string, width int) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, " ")
		if width > 0 && utf8.RuneCountInString(l) > width {
			l = string([]rune(l)[:width])
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n") + "\n"
}
