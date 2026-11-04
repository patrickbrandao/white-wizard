package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	panelcfg "github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func countEvents(t *testing.T, st *store.Store) int64 {
	t.Helper()
	n, err := st.Events.Count(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// flakySink wraps a sink: before = fail without inserting; after = insert,
// then fail (crash between the insert and the XACK).
type flakySink struct {
	inner         EventSink
	before, after atomic.Bool
}

func (s *flakySink) InsertBatch(ctx context.Context, evs []store.Event) (int, error) {
	if s.before.Load() {
		return 0, errors.New("disco cheio")
	}
	n, err := s.inner.InsertBatch(ctx, evs)
	if s.after.Load() {
		return n, errors.New("queda depois de gravar")
	}
	return n, err
}

func eventJSON(i int) string {
	return fmt.Sprintf(`{"t":"2026-10-30T12:00:%02d.123456789Z","type":"request","instance":"wwz-1","request_id":"req_%d","driver":"openai","model":"gpt-5-sim","entity":{"kind":"key","id":"k%d"},"key":"k%d","status":200,"fields":{"duration_ms":10,"prompt_tokens":1,"output_tokens":2}}`, i%60, i, i, i)
}

func (e *env) xadd(t *testing.T, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		e.do(t, "XADD", "wwz:ev:stream", "*", "d", eventJSON(i))
	}
}

func (e *env) pending(t *testing.T) int64 {
	t.Helper()
	arr, err := e.do(t, "XPENDING", "wwz:ev:stream", Group).ToArray()
	if err != nil {
		t.Fatal(err)
	}
	n, _ := arr[0].AsInt64()
	return n
}

func fastOpts(name string) ConsumerOptions {
	return ConsumerOptions{Name: name, Block: 20 * time.Millisecond, ClaimEvery: time.Hour}
}

// TestConsumerNoLossNoDuplicateAfterRestart is the acceptance criterion:
// events reach SQLite without loss or duplication across a restart between
// the insert and the XACK.
func TestConsumerNoLossNoDuplicateAfterRestart(t *testing.T) {
	e := newEnv(t)
	st := openStore(t)
	ctx := context.Background()
	sink := &flakySink{inner: st.Events}
	c1 := NewConsumer(e.src, sink, fastOpts("panel-1"))
	if n, err := c1.Step(ctx); err != nil || n != 0 {
		t.Fatalf("first step (group creation) = %d, %v", n, err)
	}
	e.xadd(t, 0, 10)
	sink.after.Store(true)
	if _, err := c1.Step(ctx); err == nil {
		t.Fatal("crash after insert: want error")
	}
	if c1.Up() {
		t.Error("Up after failure")
	}
	if got := countEvents(t, st); got != 10 {
		t.Fatalf("stored = %d, want 10", got)
	}
	if p := e.pending(t); p != 10 {
		t.Fatalf("pending = %d, want 10 (no XACK before a successful insert)", p)
	}

	// Restart: a new consumer with the same name re-reads its pending entries.
	c2 := NewConsumer(e.src, st.Events, fastOpts("panel-1"))
	n, err := c2.Step(ctx)
	if err != nil || n != 0 {
		t.Fatalf("restart step = %d, %v (duplicates must be ignored)", n, err)
	}
	if c2.Acked() != 10 || e.pending(t) != 0 || !c2.Up() {
		t.Fatalf("acked = %d, pending = %d", c2.Acked(), e.pending(t))
	}
	if n, err := c2.Step(ctx); err != nil || n != 0 { // own pending now empty
		t.Fatalf("pending drained step = %d, %v", n, err)
	}
	if n, err := c2.Step(ctx); err != nil || n != 0 { // BLOCK timeout
		t.Fatalf("block timeout step = %d, %v", n, err)
	}
	e.xadd(t, 10, 5)
	if n, err := c2.Step(ctx); err != nil || n != 5 {
		t.Fatalf("new events step = %d, %v", n, err)
	}
	if got := countEvents(t, st); got != 15 || c2.Ingested() != 5 {
		t.Fatalf("stored = %d, ingested = %d", got, c2.Ingested())
	}

	// Failure before inserting: nothing stored, nothing acked, then retried.
	e.xadd(t, 15, 3)
	c3 := NewConsumer(e.src, sink, fastOpts("panel-1"))
	sink.after.Store(false)
	sink.before.Store(true)
	if _, err := c3.Step(ctx); err != nil { // own pending (empty) first
		t.Fatal(err)
	}
	if _, err := c3.Step(ctx); err == nil || !strings.Contains(err.Error(), "disco cheio") {
		t.Fatalf("failing sink: %v", err)
	}
	if e.pending(t) != 3 || countEvents(t, st) != 15 {
		t.Fatalf("pending = %d stored = %d", e.pending(t), countEvents(t, st))
	}
	sink.before.Store(false)
	if n, err := c3.Step(ctx); err != nil || n != 3 {
		t.Fatalf("retry = %d, %v", n, err)
	}
	evs, _ := st.Events.Query(ctx, store.EventFilter{Limit: 1})
	if len(evs) != 1 || evs[0].Driver != "openai" || evs[0].EntityKind != "key" {
		t.Fatalf("event = %+v", evs)
	}
}

// TestConsumerTrimmedPending: a pending entry trimmed from the stream
// (MAXLEN) comes back without fields and is acknowledged, not retried forever.
func TestConsumerTrimmedPending(t *testing.T) {
	e := newEnv(t)
	st := openStore(t)
	ctx := context.Background()
	sink := &flakySink{inner: st.Events}
	c := NewConsumer(e.src, sink, fastOpts("t"))
	_, _ = c.Step(ctx)
	e.xadd(t, 0, 2)
	sink.before.Store(true)
	if _, err := c.Step(ctx); err == nil {
		t.Fatal("want error")
	}
	ids, _ := e.do(t, "XRANGE", "wwz:ev:stream", "-", "+").AsXRange()
	e.do(t, "XDEL", "wwz:ev:stream", ids[0].ID)
	sink.before.Store(false)
	if n, err := c.Step(ctx); err != nil || n != 1 {
		t.Fatalf("step = %d, %v", n, err)
	}
	if e.pending(t) != 0 || countEvents(t, st) != 1 {
		t.Fatalf("pending = %d stored = %d", e.pending(t), countEvents(t, st))
	}
}

func TestConsumerClaimsIdlePending(t *testing.T) {
	e := newEnv(t)
	st := openStore(t)
	ctx := context.Background()
	sink := &flakySink{inner: st.Events}
	a := NewConsumer(e.src, sink, fastOpts("dead"))
	_, _ = a.Step(ctx)
	e.xadd(t, 0, 5)
	sink.before.Store(true)
	if _, err := a.Step(ctx); err == nil {
		t.Fatal("want error")
	}
	time.Sleep(10 * time.Millisecond)
	b := NewConsumer(e.src, st.Events, ConsumerOptions{Name: "alive", Count: 2, Block: 20 * time.Millisecond, ClaimIdle: time.Millisecond, ClaimEvery: time.Nanosecond})
	n, err := b.Step(ctx)
	if err != nil || n != 5 {
		t.Fatalf("claim = %d, %v", n, err)
	}
	if e.pending(t) != 0 || countEvents(t, st) != 5 {
		t.Fatalf("pending = %d stored = %d", e.pending(t), countEvents(t, st))
	}
	// Claimed entries that fail to store make the step fail.
	e.xadd(t, 5, 1)
	sink.before.Store(true)
	_, _ = a.Step(ctx) // own pending first (empty)
	if _, err := a.Step(ctx); err == nil {
		t.Fatal("want error")
	}
	time.Sleep(5 * time.Millisecond)
	bad := NewConsumer(e.src, sink, ConsumerOptions{Name: "alive2", ClaimIdle: time.Millisecond, ClaimEvery: time.Nanosecond})
	if _, err := bad.Step(ctx); err == nil || !strings.Contains(err.Error(), "disco cheio") {
		t.Fatalf("claim with failing sink: %v", err)
	}
}

func TestConsumerPoisonAndNoGroup(t *testing.T) {
	e := newEnv(t)
	st := openStore(t)
	ctx := context.Background()
	var logs bytes.Buffer
	c := NewConsumer(e.src, st.Events, ConsumerOptions{Name: "p", Block: 20 * time.Millisecond, ClaimEvery: time.Hour, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	_, _ = c.Step(ctx)
	e.do(t, "XADD", "wwz:ev:stream", "*", "x", "1")
	e.do(t, "XADD", "wwz:ev:stream", "*", "d", "not json")
	e.do(t, "XADD", "wwz:ev:stream", "*", "d", `{"type":"request"}`)
	e.xadd(t, 0, 1)
	if n, err := c.Step(ctx); err != nil || n != 1 {
		t.Fatalf("poison step = %d, %v", n, err)
	}
	if c.Acked() != 4 || e.pending(t) != 0 || countEvents(t, st) != 1 {
		t.Fatalf("acked %d pending %d stored %d", c.Acked(), e.pending(t), countEvents(t, st))
	}
	if !strings.Contains(logs.String(), "sem campo d") || !strings.Contains(logs.String(), "evento inválido") {
		t.Fatalf("logs = %s", logs.String())
	}
	// The stream (and its group) vanish, e.g. POST /admin/reset: NOGROUP,
	// then the group is created again.
	e.do(t, "DEL", "wwz:ev:stream")
	e.xadd(t, 1, 1)
	if _, err := c.Step(ctx); err == nil || !strings.Contains(err.Error(), "NOGROUP") {
		t.Fatalf("want NOGROUP, got %v", err)
	}
	if _, err := c.Step(ctx); err != nil {
		t.Fatalf("group recreated: %v", err)
	}
	e.xadd(t, 2, 1)
	if n, err := c.Step(ctx); err != nil || n != 1 {
		t.Fatalf("after recreation = %d, %v", n, err)
	}
	// Wrong type at the stream key: XGROUP CREATE fails.
	e.do(t, "DEL", "wwz:ev:stream")
	e.do(t, "SET", "wwz:ev:stream", "x")
	c2 := NewConsumer(e.src, st.Events, fastOpts("q"))
	if _, err := c2.Step(ctx); err == nil || !strings.Contains(err.Error(), "XGROUP") {
		t.Fatalf("XGROUP on string: %v", err)
	}
	c2.ready, c2.lastClaim = true, time.Now()
	if _, err := c2.Step(ctx); err == nil || !strings.Contains(err.Error(), "XREADGROUP") {
		t.Fatalf("XREADGROUP on string: %v", err)
	}
	c2.ready, c2.lastClaim = true, time.Time{}
	if _, err := c2.Step(ctx); err == nil || !strings.Contains(err.Error(), "XAUTOCLAIM") {
		t.Fatalf("XAUTOCLAIM on string: %v", err)
	}
}

func TestConsumerXACKFails(t *testing.T) {
	e := newEnv(t)
	st := openStore(t)
	ctx := context.Background()
	c := NewConsumer(e.src, st.Events, fastOpts("x"))
	_, _ = c.Step(ctx)
	e.xadd(t, 0, 1)
	// The group disappears between XREADGROUP and XACK: XACK answers 0, no
	// error; a non-stream key makes XACK fail.
	sink := sinkFunc(func(ctx context.Context, evs []store.Event) (int, error) {
		e.do(t, "DEL", "wwz:ev:stream")
		e.do(t, "SET", "wwz:ev:stream", "x")
		return st.Events.InsertBatch(ctx, evs)
	})
	c.sink = sink
	if _, err := c.Step(ctx); err == nil || !strings.Contains(err.Error(), "XACK") {
		t.Fatalf("XACK on string: %v", err)
	}
}

type sinkFunc func(context.Context, []store.Event) (int, error)

func (f sinkFunc) InsertBatch(ctx context.Context, evs []store.Event) (int, error) {
	return f(ctx, evs)
}

func TestConsumerRun(t *testing.T) {
	e := newEnv(t)
	st := openStore(t)
	old := lagAlert
	lagAlert = 0
	defer func() { lagAlert = old }()
	var logs syncBuffer
	c := NewConsumer(e.src, st.Events, ConsumerOptions{Name: "run", Block: 20 * time.Millisecond, LagEvery: time.Millisecond, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	_, _ = c.Step(context.Background())
	if c.Lag() != -1 {
		t.Fatalf("initial lag = %d", c.Lag())
	}
	e.xadd(t, 0, 20)
	c.RefreshLag(context.Background())
	if c.Lag() != 20 || !strings.Contains(logs.String(), "atraso") {
		t.Fatalf("lag = %d logs = %s", c.Lag(), logs.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for countEvents(t, st) < 20 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not consume")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run on done ctx = %v", err)
	}
}

func TestConsumerRunBackoff(t *testing.T) {
	v, err := NewValkey(panelcfg.Valkey{Addr: []string{"127.0.0.1:1"}}, ValkeyOptions{Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	c := NewConsumer(v, sinkFunc(nil), ConsumerOptions{MinBackoff: time.Millisecond, MaxBackoff: 3 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	c.RefreshLag(context.Background()) // fails silently
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := c.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v", err)
	}
	if !strings.Contains(logs.String(), "nova tentativa") {
		t.Fatalf("logs = %s", logs.String())
	}
	// Cancelled while a step is failing: returns at once.
	cctx, ccancel := context.WithCancel(context.Background())
	c2 := NewConsumer(v, sinkFunc(nil), ConsumerOptions{MinBackoff: time.Hour})
	go func() { time.Sleep(20 * time.Millisecond); ccancel() }()
	if err := c2.Run(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
}

// TestConsumerFromLimitsStore consumes the events the real wwz store writes.
func TestConsumerFromLimitsStore(t *testing.T) {
	e := newEnv(t)
	st := openStore(t)
	ctx := context.Background()
	c := NewConsumer(e.src, st.Events, fastOpts("lv"))
	_, _ = c.Step(ctx)
	lv := e.limitsStore(t, "inst-a", -1)
	k := limits.Entity{Kind: limits.KindKey, ID: "k1"}
	if err := lv.Block(ctx, k, "r", time.Minute, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := lv.Finish(ctx, limits.FinishRequest{Subjects: []limits.Subject{{Entity: k}}, RequestID: "req-1", Status: 429, Driver: "anthropic", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Step(ctx); err != nil || n != 2 {
		t.Fatalf("step = %d, %v", n, err)
	}
	evs, _ := st.Events.Query(ctx, store.EventFilter{Types: []string{store.EventBlock}})
	if len(evs) != 1 || evs[0].EntityID != "k1" || evs[0].Instance != "inst-a" {
		t.Fatalf("block = %+v", evs)
	}
}

// ---- poller ----

func TestRecordEvent(t *testing.T) {
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	ev, err := RecordEvent(RequestRecord{
		Time: at, RequestID: "r1", Driver: "openai", Model: "m", Status: 429, Error: "rate_limit", Key: "k", IP: "1.2.3.4",
		TestID: "t", DurationMS: 5, PromptTokens: 3, CompletionTokens: 4, ReasoningTokens: 2, ToolCalls: []string{"a"}, Scenario: "s1", Step: "a",
	}, "wwz:9000")
	if err != nil {
		t.Fatal(err)
	}
	if ev.StreamID != "req:r1" || ev.Type != store.EventError || ev.EntityKind != "key" || ev.EntityID != "k" || ev.Instance != "wwz:9000" ||
		ev.Error != "rate_limit" || ev.TestID != "t" || !ev.At.Equal(at) {
		t.Fatalf("event = %+v", ev)
	}
	for _, s := range []string{`"output_tokens":4`, `"reasoning_tokens":2`, `"tool_calls":["a"]`, `"ip":"1.2.3.4"`, `"key":"k"`, `"scenario":"s1"`, `"step":"a"`} {
		if !strings.Contains(string(ev.Data), s) {
			t.Errorf("data %s lacks %s", ev.Data, s)
		}
	}
	ev, err = RecordEvent(RequestRecord{Time: at, RequestID: "r2", Status: 200, IP: "::1"}, "i")
	if err != nil || ev.Type != store.EventRequest || ev.EntityKind != "ip" || strings.Contains(string(ev.Data), "reasoning") {
		t.Fatalf("ip event = %+v %v", ev, err)
	}
	ev, err = RecordEvent(RequestRecord{Time: at, RequestID: "r3"}, "i")
	if err != nil || ev.EntityKind != "" {
		t.Fatalf("no entity = %+v %v", ev, err)
	}
	if _, err := RecordEvent(RequestRecord{Time: at}, "i"); err == nil {
		t.Error("no request_id: want error")
	}
	if _, err := RecordEvent(RequestRecord{RequestID: "x"}, "i"); err == nil {
		t.Error("no time: want error")
	}
}

func TestRequestPoller(t *testing.T) {
	st := openStore(t)
	var enabled atomic.Bool
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/requests" || r.URL.Query().Get("last") != "1000" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "bad", 400)
			return
		}
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		if !enabled.Load() {
			_, _ = w.Write([]byte(`{"enabled":false,"requests":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"enabled":true,"requests":[
			{"time":"2026-10-30T12:00:00Z","request_id":"a","driver":"openai","model":"m","endpoint":"/v1/chat/completions","status":200,"duration_ms":3,"stream":false},
			{"time":"2026-10-30T12:00:01Z","request_id":"b","driver":"openai","model":"m","endpoint":"/v1/chat/completions","status":500,"duration_ms":3,"stream":true},
			{"time":"2026-10-30T12:00:02Z","request_id":"","driver":"openai","model":"m","endpoint":"/x","status":200,"duration_ms":3,"stream":false}]}`))
	}))
	defer srv.Close()
	a := NewAdmin(srv.URL, "tok", nil)
	p := NewRequestPoller(a, st.Events, PollerOptions{})
	if p.o.Instance != strings.TrimPrefix(srv.URL, "http://") || p.o.Every != 5*time.Second {
		t.Fatalf("defaults = %+v", p.o)
	}
	ctx := context.Background()
	if n, err := p.Poll(ctx); err != nil || n != 0 || p.Enabled() || !p.Up() {
		t.Fatalf("disabled poll = %d %v", n, err)
	}
	enabled.Store(true)
	if n, err := p.Poll(ctx); err != nil || n != 2 || !p.Enabled() {
		t.Fatalf("poll = %d %v", n, err)
	}
	if n, err := p.Poll(ctx); err != nil || n != 0 || p.Ingested() != 2 {
		t.Fatalf("second poll = %d %v (idempotent)", n, err)
	}
	if n, _ := st.Events.Count(ctx, store.EventFilter{Types: []string{store.EventError}}); n != 1 {
		t.Fatalf("errors = %d", n)
	}
	fail.Store(true)
	if _, err := p.Poll(ctx); err == nil || p.Up() {
		t.Fatal("503: want error")
	}
	fail.Store(false)
	bad := NewRequestPoller(a, sinkFunc(func(context.Context, []store.Event) (int, error) { return 0, errors.New("x") }), PollerOptions{Instance: "i"})
	if _, err := bad.Poll(ctx); err == nil || bad.Up() {
		t.Fatal("sink failure: want error")
	}
	// Run: polls at once and on every tick, logs failures.
	var logs syncBuffer
	st2 := openStore(t)
	r := NewRequestPoller(NewAdmin(srv.URL, "wrong", nil), st2.Events, PollerOptions{Every: 5 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	rctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := r.Run(rctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v", err)
	}
	if !strings.Contains(logs.String(), "/admin/requests falhou") {
		t.Fatalf("logs = %s", logs.String())
	}
	if p := NewRequestPoller(NewAdmin("::bad", "", nil), st2.Events, PollerOptions{}); p.o.Instance != "wwz" {
		t.Fatalf("instance fallback = %q", p.o.Instance)
	}
}
