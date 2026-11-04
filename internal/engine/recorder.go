package engine

import (
	"hash/fnv"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// Record is one served request kept for /admin/requests.
type Record struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id"`
	Driver           string    `json:"driver"`
	Model            string    `json:"model"`
	Endpoint         string    `json:"endpoint"`
	Status           int       `json:"status"`
	DurationMS       int64     `json:"duration_ms"`
	TTFTMS           int64     `json:"ttft_ms,omitempty"`
	ITLMS            float64   `json:"itl_ms,omitempty"`
	IP               string    `json:"ip,omitempty"`
	Key              string    `json:"key,omitempty"`
	Stream           bool      `json:"stream"`
	TestID           string    `json:"test_id,omitempty"`
	Scenario         string    `json:"scenario,omitempty"`
	Step             string    `json:"step,omitempty"`
	Attempt          int       `json:"attempt,omitempty"`
	Error            string    `json:"error,omitempty"`
	MidStream        string    `json:"mid_stream,omitempty"`
	ToolBreak        string    `json:"tool_break,omitempty"`
	PromptTokens     int       `json:"prompt_tokens,omitempty"`
	CompletionTokens int       `json:"completion_tokens,omitempty"`
	ReasoningTokens  int       `json:"reasoning_tokens,omitempty"`
	ToolCalls        []string  `json:"tool_calls,omitempty"`
	FinishReason     string    `json:"finish_reason,omitempty"`
	Seed             int64     `json:"seed,omitempty"`
}

// Recorder is a fixed-size ring of recent records.
type Recorder struct {
	mu   sync.Mutex
	buf  []Record
	next int
	full bool
}

// NewRecorder creates a recorder keeping the last n requests.
func NewRecorder(n int) *Recorder {
	if n <= 0 {
		n = 1000
	}
	return &Recorder{buf: make([]Record, n)}
}

// Add stores a record.
func (r *Recorder) Add(rec Record) {
	r.mu.Lock()
	r.buf[r.next] = rec
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Last returns up to n most recent records, newest first, optionally
// filtered by test id, key or scenario.
func (r *Recorder) Last(n int, filter func(*Record) bool) []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	size := r.next
	if r.full {
		size = len(r.buf)
	}
	out := make([]Record, 0, n)
	for i := 1; i <= size && len(out) < n; i++ {
		idx := (r.next - i + len(r.buf)) % len(r.buf)
		rec := &r.buf[idx]
		if filter == nil || filter(rec) {
			out = append(out, *rec)
		}
	}
	return out
}

// Clear drops all records.
func (r *Recorder) Clear() {
	r.mu.Lock()
	r.next, r.full = 0, false
	r.mu.Unlock()
}

// Resilience scores client retry behaviour per test id: whether 429/503
// were retried, whether Retry-After was honoured, and the backoff shape.
type Resilience struct {
	mu    sync.Mutex
	tests map[string]*TestReport
}

// TestReport is the per-test resilience summary.
type TestReport struct {
	TestID          string      `json:"test_id"`
	Requests        int         `json:"requests"`
	Errors          int         `json:"errors"`
	Retried         int         `json:"retried"`
	RetryAfterSent  int         `json:"retry_after_sent"`
	RetryAfterHonor int         `json:"retry_after_honoured"`
	RetryAfterEarly int         `json:"retry_after_early"`
	Intervals       []float64   `json:"retry_intervals_ms"`
	BackoffGrowing  bool        `json:"backoff_growing"`
	Statuses        map[int]int `json:"statuses"`
	// StreamsConsumed counts streams the client read (complete or cut by an
	// injected mid-stream fault); ResentAfterStream counts requests that
	// repeated the previous stream's body right after it (spec 017 I15).
	StreamsConsumed   int       `json:"streams_consumed"`
	ResentAfterStream int       `json:"resent_after_stream"`
	First             time.Time `json:"first"`
	Last              time.Time `json:"last"`
	lastErrAt         time.Time
	lastRetryAfter    time.Duration
	pendingRetry      bool
	lastStreamFP      uint64
}

// NewResilience creates the tracker.
func NewResilience() *Resilience { return &Resilience{tests: map[string]*TestReport{}} }

// Observe records one response for a test id.
func (rs *Resilience) Observe(testID string, req *core.Request, status int, er *core.Error) {
	if testID == "" || rs == nil {
		return
	}
	now := time.Now()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	t := rs.tests[testID]
	if t == nil {
		if len(rs.tests) >= 10000 {
			return
		}
		t = &TestReport{TestID: testID, Statuses: map[int]int{}, First: now}
		rs.tests[testID] = t
	}
	t.Requests++
	t.Last = now
	t.Statuses[status]++
	if req != nil {
		fp := requestFingerprint(req)
		if t.lastStreamFP != 0 && fp == t.lastStreamFP {
			t.ResentAfterStream++
		}
		t.lastStreamFP = 0
		if req.Stream && status == 200 {
			t.StreamsConsumed++
			t.lastStreamFP = fp
		}
	}
	if t.pendingRetry {
		gap := now.Sub(t.lastErrAt)
		t.Retried++
		t.Intervals = append(t.Intervals, float64(gap.Microseconds())/1000)
		if len(t.Intervals) > 200 {
			t.Intervals = t.Intervals[len(t.Intervals)-200:]
		}
		if t.lastRetryAfter > 0 {
			if gap >= t.lastRetryAfter-50*time.Millisecond {
				t.RetryAfterHonor++
			} else {
				t.RetryAfterEarly++
			}
		}
		t.pendingRetry = false
	}
	if status == 429 || status == 503 || status == 529 || status == 500 || status == 502 || status == 504 {
		t.Errors++
		t.pendingRetry = true
		t.lastErrAt = now
		t.lastRetryAfter = 0
		if er != nil && er.RetryAfter > 0 {
			t.RetryAfterSent++
			t.lastRetryAfter = er.RetryAfter
		}
	}
	if n := len(t.Intervals); n >= 3 {
		t.BackoffGrowing = t.Intervals[n-1] > t.Intervals[n-2] && t.Intervals[n-2] > t.Intervals[n-3]
	}
}

// requestFingerprint identifies a request body so a re-send of the same
// request can be told apart from the next turn of the conversation.
func requestFingerprint(req *core.Request) uint64 {
	h := fnv.New64a()
	if len(req.Raw) > 0 {
		_, _ = h.Write(req.Raw)
	} else {
		_, _ = h.Write([]byte(req.Model))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strconv.Itoa(len(req.Messages))))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(req.SystemPrompt()))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(req.LastUserMessage()))
	}
	return h.Sum64()
}

// Report returns the test report (nil when unknown).
func (rs *Resilience) Report(testID string) *TestReport {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	t := rs.tests[testID]
	if t == nil {
		return nil
	}
	cp := *t
	cp.Intervals = append([]float64(nil), t.Intervals...)
	cp.Statuses = map[int]int{}
	for k, v := range t.Statuses {
		cp.Statuses[k] = v
	}
	return &cp
}

// List returns all reports, most recent first.
func (rs *Resilience) List(limit int) []*TestReport {
	rs.mu.Lock()
	out := make([]*TestReport, 0, len(rs.tests))
	for id := range rs.tests {
		out = append(out, rs.tests[id])
	}
	rs.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	res := make([]*TestReport, 0, len(out))
	for _, t := range out {
		res = append(res, rs.Report(t.TestID))
	}
	return res
}

// Reset drops all reports.
func (rs *Resilience) Reset() {
	rs.mu.Lock()
	rs.tests = map[string]*TestReport{}
	rs.mu.Unlock()
}
