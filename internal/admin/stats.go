package admin

import (
	"bufio"
	"bytes"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// sample is one series of the Prometheus exposition.
type sample struct {
	name   string
	labels map[string]string
	id     string // name{labels} as written, used for baselines
	value  float64
	gauge  bool
}

// scrape renders the registry and parses it back. Parsing the text keeps
// /admin/stats equal to /metrics without widening internal/metrics.
func (s *Server) scrape() []sample {
	if s.eng.M == nil || s.eng.M.Registry == nil {
		return nil
	}
	var buf bytes.Buffer
	s.eng.M.Registry.Write(&buf)
	return parseExposition(buf.Bytes())
}

func parseExposition(b []byte) []sample {
	gauges := map[string]bool{}
	var out []sample
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			f := strings.Fields(line)
			if len(f) == 4 && f[3] == "gauge" {
				gauges[f[2]] = true
			}
			continue
		}
		if line[0] == '#' {
			continue
		}
		sp, ok := parseSample(line)
		if !ok {
			continue
		}
		sp.gauge = gauges[sp.name]
		out = append(out, sp)
	}
	return out
}

func parseSample(line string) (sample, bool) {
	sp := sample{labels: map[string]string{}}
	var rest string
	if i := strings.IndexByte(line, '{'); i >= 0 {
		sp.name = line[:i]
		j := i + 1
		for j < len(line) && line[j] != '}' {
			eq := strings.IndexByte(line[j:], '=')
			if eq < 0 || j+eq+1 >= len(line) || line[j+eq+1] != '"' {
				return sp, false
			}
			key := line[j : j+eq]
			k := j + eq + 2
			var val strings.Builder
			for ; k < len(line) && line[k] != '"'; k++ {
				if line[k] == '\\' && k+1 < len(line) {
					k++
					switch line[k] {
					case 'n':
						val.WriteByte('\n')
					default:
						val.WriteByte(line[k])
					}
					continue
				}
				val.WriteByte(line[k])
			}
			if k >= len(line) {
				return sp, false
			}
			sp.labels[key] = val.String()
			j = k + 1
			if j < len(line) && line[j] == ',' {
				j++
			}
		}
		if j >= len(line) {
			return sp, false
		}
		sp.id = line[:j+1]
		rest = line[j+1:]
	} else {
		sp2 := strings.IndexByte(line, ' ')
		if sp2 < 0 {
			return sp, false
		}
		sp.name, sp.id, rest = line[:sp2], line[:sp2], line[sp2:]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	if err != nil {
		return sp, false
	}
	sp.value = v
	return sp, true
}

// sinceReset subtracts the baseline taken at the last metrics reset from
// counters and histograms (gauges are instantaneous).
func (s *Server) sinceReset(all []sample) []sample {
	s.statsMu.Lock()
	base := s.baseline
	s.statsMu.Unlock()
	if base == nil {
		return all
	}
	for i := range all {
		if all[i].gauge {
			continue
		}
		all[i].value = math.Max(0, all[i].value-base[all[i].id])
	}
	return all
}

func (s *Server) resetMetrics(now time.Time) {
	base := map[string]float64{}
	for _, sp := range s.scrape() {
		if !sp.gauge {
			base[sp.id] = sp.value
		}
	}
	s.statsMu.Lock()
	s.baseline = base
	s.resetAt = now
	s.statsMu.Unlock()
}

// Stats is the body of GET /admin/stats.
type Stats struct {
	Started time.Time    `json:"started"`
	UptimeS float64      `json:"uptime_s"`
	ResetAt *time.Time   `json:"reset_at,omitempty"`
	Pod     string       `json:"pod,omitempty"`
	Store   string       `json:"store"`
	Totals  StatsTotals  `json:"totals"`
	ByModel []ModelStats `json:"by_model"`
	ByKey   []KeyStats   `json:"by_key,omitempty"`
}

// StatsTotals aggregates every series.
type StatsTotals struct {
	Requests           int64            `json:"requests"`
	InFlight           int64            `json:"in_flight"`
	StreamsOpen        int64            `json:"streams_open"`
	ErrorsInjected     int64            `json:"errors_injected"`
	RateLimited        int64            `json:"rate_limited"`
	Blocked            int64            `json:"blocked"`
	ByStatus           map[string]int64 `json:"by_status"`
	ErrorsByKind       map[string]int64 `json:"errors_by_kind"`
	ErrorsByPhase      map[string]int64 `json:"errors_by_phase"`
	RateLimitedByLimit map[string]int64 `json:"rate_limited_by_limit"`
}

// ModelStats is one driver/model row.
type ModelStats struct {
	Driver    string           `json:"driver"`
	Model     string           `json:"model"`
	Requests  int64            `json:"requests"`
	Errors    int64            `json:"errors"`
	TTFTP50MS float64          `json:"ttft_p50_ms"`
	TTFTP99MS float64          `json:"ttft_p99_ms"`
	ITLP50MS  float64          `json:"itl_p50_ms"`
	ITLP99MS  float64          `json:"itl_p99_ms"`
	E2EP50MS  float64          `json:"e2e_p50_ms"`
	E2EP99MS  float64          `json:"e2e_p99_ms"`
	Tokens    map[string]int64 `json:"tokens,omitempty"`
}

// KeyStats is the usage of one key from the store.
type KeyStats struct {
	Key      string        `json:"key"`
	Status   limits.Status `json:"status,omitempty"`
	Requests int64         `json:"requests"`
	SpendUSD float64       `json:"spend_usd"`
}

type bucketPoint struct{ le, count float64 }

type modelKey struct{ driver, model string }

// buildStats aggregates the scraped samples.
func buildStats(samples []sample) (StatsTotals, []ModelStats) {
	t := StatsTotals{ByStatus: map[string]int64{}, ErrorsByKind: map[string]int64{}, ErrorsByPhase: map[string]int64{}, RateLimitedByLimit: map[string]int64{}}
	models := map[modelKey]*ModelStats{}
	hists := map[string]map[modelKey][]bucketPoint{}
	sums := map[string]map[modelKey]float64{}
	row := func(l map[string]string) *ModelStats {
		k := modelKey{l["driver"], l["model"]}
		m := models[k]
		if m == nil {
			m = &ModelStats{Driver: k.driver, Model: k.model}
			models[k] = m
		}
		return m
	}
	for _, sp := range samples {
		v := int64(sp.value)
		switch sp.name {
		case "wwz_requests_total":
			t.Requests += v
			t.ByStatus[sp.labels["status"]] += v
			m := row(sp.labels)
			m.Requests += v
			if st, _ := strconv.Atoi(sp.labels["status"]); st >= 400 {
				m.Errors += v
			}
		case "wwz_requests_in_flight":
			t.InFlight += v
		case "wwz_streams_open":
			t.StreamsOpen += v
		case "wwz_errors_injected_total":
			t.ErrorsInjected += v
			t.ErrorsByKind[sp.labels["kind"]] += v
			t.ErrorsByPhase[sp.labels["phase"]] += v
		case "wwz_rate_limited_total":
			t.RateLimited += v
			t.RateLimitedByLimit[sp.labels["limit"]] += v
		case "wwz_blocked_total":
			t.Blocked += v
		case "wwz_tokens_total":
			m := row(sp.labels)
			if m.Tokens == nil {
				m.Tokens = map[string]int64{}
			}
			m.Tokens[sp.labels["kind"]] += v
		case "wwz_ttft_seconds_bucket", "wwz_itl_seconds_bucket", "wwz_e2e_seconds_bucket":
			le := math.Inf(1)
			if sp.labels["le"] != "+Inf" {
				f, err := strconv.ParseFloat(sp.labels["le"], 64)
				if err != nil {
					continue
				}
				le = f
			}
			h := hists[sp.name]
			if h == nil {
				h = map[modelKey][]bucketPoint{}
				hists[sp.name] = h
			}
			k := modelKey{sp.labels["driver"], sp.labels["model"]}
			row(sp.labels)
			h[k] = append(h[k], bucketPoint{le, sp.value})
		case "wwz_ttft_seconds_sum", "wwz_itl_seconds_sum", "wwz_e2e_seconds_sum":
			name := strings.TrimSuffix(sp.name, "_sum") + "_bucket"
			if sums[name] == nil {
				sums[name] = map[modelKey]float64{}
			}
			k := modelKey{sp.labels["driver"], sp.labels["model"]}
			sums[name][k] += sp.value
		}
	}
	quant := func(name string, k modelKey) (p50, p99 float64) {
		pts := hists[name][k]
		sum, ok := sums[name][k]
		if !ok {
			sum = math.NaN()
		}
		return quantileMS(pts, sum, 0.5), quantileMS(pts, sum, 0.99)
	}
	for k, m := range models {
		m.TTFTP50MS, m.TTFTP99MS = quant("wwz_ttft_seconds_bucket", k)
		m.ITLP50MS, m.ITLP99MS = quant("wwz_itl_seconds_bucket", k)
		m.E2EP50MS, m.E2EP99MS = quant("wwz_e2e_seconds_bucket", k)
	}
	out := make([]ModelStats, 0, len(models))
	for _, m := range models {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Driver != out[j].Driver {
			return out[i].Driver < out[j].Driver
		}
		return out[i].Model < out[j].Model
	})
	return t, out
}

// quantileMS estimates a quantile (ms) from cumulative buckets with linear
// interpolation inside the bucket that holds the rank, like Prometheus'
// histogram_quantile: the rank in the first bucket interpolates from 0, the
// rank in the +Inf bucket returns the largest finite bound. The histogram
// carries no observed maximum, so when sum (the _sum series, in seconds; NaN
// when absent) is known the estimate is clamped to maxBound, an upper bound
// of the largest observation derived from it.
func quantileMS(points []bucketPoint, sum, q float64) float64 {
	if len(points) == 0 {
		return 0
	}
	sort.Slice(points, func(i, j int) bool { return points[i].le < points[j].le })
	total := points[len(points)-1].count
	if total <= 0 {
		return 0
	}
	target := q * total
	est := 0.0
	prevLE, prevCount := 0.0, 0.0
	for _, p := range points {
		if p.count >= target {
			in := p.count - prevCount
			switch {
			case math.IsInf(p.le, 1):
				est = prevLE
			case in <= 0:
				est = p.le
			default:
				est = prevLE + (p.le-prevLE)*(target-prevCount)/in
			}
			break
		}
		prevLE, prevCount = p.le, p.count
		est = prevLE
	}
	if mx, ok := maxBound(points, sum); ok && est > mx {
		est = mx
	}
	return est * 1000
}

// maxBound bounds the largest observation using _sum: every observation in a
// bucket is at least the bucket's lower bound, so the largest one is at most
// sum minus the lower bounds of all the others. points must be sorted.
func maxBound(points []bucketPoint, sum float64) (float64, bool) {
	if math.IsNaN(sum) || sum < 0 {
		return 0, false
	}
	lowerSum, topLower := 0.0, 0.0
	prevLE, prevCount := 0.0, 0.0
	for _, p := range points {
		if n := p.count - prevCount; n > 0 {
			lowerSum += n * prevLE
			topLower = prevLE
		}
		prevLE, prevCount = p.le, p.count
	}
	mx := sum - lowerSum + topLower
	if mx < topLower {
		// Inconsistent _sum (e.g. scraped between bucket and sum updates):
		// do not trust it below the bucket the maximum must lie in.
		return 0, false
	}
	return mx, true
}

func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	totals, byModel := buildStats(s.sinceReset(s.scrape()))
	st := Stats{Started: s.eng.Started(), UptimeS: now.Sub(s.eng.Started()).Seconds(), Pod: s.pod, Totals: totals, ByModel: byModel, Store: "memory"}
	s.statsMu.Lock()
	if !s.resetAt.IsZero() {
		ra := s.resetAt
		st.ResetAt = &ra
	}
	s.statsMu.Unlock()
	if s.eng.Store != nil {
		st.Store = s.eng.Store.Kind()
		ctx, cancel := storeCtx(r)
		ents, err := s.eng.Store.List(ctx, "", 10000)
		cancel()
		if err == nil {
			for _, e := range ents {
				if e.Entity.Kind == limits.KindKey {
					st.ByKey = append(st.ByKey, KeyStats{Key: e.Entity.ID, Status: e.Status, Requests: e.Requests, SpendUSD: e.SpendUSD})
				}
			}
			sort.Slice(st.ByKey, func(i, j int) bool { return st.ByKey[i].Key < st.ByKey[j].Key })
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// ResetRequest is the body of POST /admin/reset.
type ResetRequest struct {
	Scope []string `json:"scope"`
}

// ResetResult is the response of POST /admin/reset.
type ResetResult struct {
	Reset   []string  `json:"reset"`
	ResetAt time.Time `json:"reset_at"`
}

var resetScopes = map[string]bool{"limits": true, "metrics": true, "requests": true, "resilience": true, "conversations": true, "chaos": true}

func (s *Server) postReset(w http.ResponseWriter, r *http.Request) {
	var req ResetRequest
	if _, err := decodeJSON(r, &req); err != nil {
		invalid(w, "invalid body", err.Error())
		return
	}
	scope := req.Scope
	if len(scope) == 0 {
		scope = []string{"limits", "metrics"}
	}
	seen := map[string]bool{}
	for _, sc := range scope {
		if !resetScopes[sc] {
			invalid(w, "unknown scope "+strconv.Quote(sc))
			return
		}
		if seen[sc] {
			invalid(w, "duplicate scope "+strconv.Quote(sc))
			return
		}
		seen[sc] = true
	}
	now := s.now()
	ctx, cancel := storeCtx(r)
	defer cancel()
	for _, sc := range scope {
		var err error
		switch sc {
		case "limits":
			if s.eng.Store != nil {
				if cr, ok := s.eng.Store.(limits.CounterResetter); ok {
					err = cr.ResetCounters(ctx)
				} else {
					err = s.eng.Store.Reset(ctx)
				}
			}
		case "metrics":
			s.resetMetrics(now)
		case "requests":
			if s.eng.Recorder != nil {
				s.eng.Recorder.Clear()
			}
		case "resilience":
			if s.eng.Resilience != nil {
				s.eng.Resilience.Reset()
			}
		case "conversations":
			if cr, ok := s.eng.Store.(limits.ConversationResetter); ok {
				_, err = cr.ResetConversations(ctx, "")
			}
		case "chaos":
			if s.eng.Injector != nil {
				s.eng.Injector.Reset()
			}
		}
		if err != nil {
			storeProblem(w, "reset "+sc+": ", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, ResetResult{Reset: scope, ResetAt: now})
}
