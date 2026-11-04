package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// Group is the consumer group of the panel on ev:stream (asyncapi).
const Group = "panel"

// LagAlert is the lag above which the consumer logs a warning (alert "fila
// ev:stream acima de 500 000", spec 016).
const LagAlert = 500_000

// lagAlert is LagAlert (a variable so tests can lower it).
var lagAlert int64 = LagAlert

// EventSink stores a batch of events idempotently (store.Events).
type EventSink interface {
	InsertBatch(ctx context.Context, evs []store.Event) (int, error)
}

// ConsumerOptions configures NewConsumer; zero values take the defaults of
// spec 016.
type ConsumerOptions struct {
	// Name is the consumer name; "" = hostname.
	Name string
	// Count per XREADGROUP; 0 = 500.
	Count int
	// Block of XREADGROUP; 0 = 1 s.
	Block time.Duration
	// ClaimIdle is the idle time after which pending entries of other
	// consumers are taken with XAUTOCLAIM; 0 = 60 s.
	ClaimIdle time.Duration
	// ClaimEvery is how often XAUTOCLAIM runs; 0 = 30 s.
	ClaimEvery time.Duration
	// MinBackoff and MaxBackoff bound the exponential reconnection backoff;
	// 0 = 250 ms and 30 s.
	MinBackoff, MaxBackoff time.Duration
	// LagEvery is how often Run refreshes Lag; 0 = 15 s.
	LagEvery time.Duration
	Logger   *slog.Logger
}

// Consumer reads P:ev:stream with the group "panel"
// (eventbus/valkey/asyncapi.yaml), stores each batch in the panel SQLite and
// only then XACKs it. After a start or any failure it first re-reads its
// own pending entries (XREADGROUP ... 0), so a crash between the insert and
// the XACK loses nothing; the insert is idempotent by stream id, so nothing
// is duplicated either.
type Consumer struct {
	v    *Valkey
	sink EventSink
	o    ConsumerOptions
	log  *slog.Logger

	mu        sync.Mutex // serializes Step
	ready     bool       // group created
	pending   bool       // read own pending entries first
	lastClaim time.Time

	ingested atomic.Int64
	acked    atomic.Int64
	lag      atomic.Int64
	up       atomic.Bool
}

// NewConsumer builds the consumer of v's stream writing to sink.
func NewConsumer(v *Valkey, sink EventSink, o ConsumerOptions) *Consumer {
	if o.Name == "" {
		o.Name = hostname()
	}
	if o.Count <= 0 {
		o.Count = 500
	}
	if o.Block <= 0 {
		o.Block = time.Second
	}
	if o.ClaimIdle <= 0 {
		o.ClaimIdle = 60 * time.Second
	}
	if o.ClaimEvery <= 0 {
		o.ClaimEvery = 30 * time.Second
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = 250 * time.Millisecond
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 30 * time.Second
	}
	if o.LagEvery <= 0 {
		o.LagEvery = 15 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	c := &Consumer{v: v, sink: sink, o: o, log: o.Logger, pending: true}
	c.lag.Store(-1)
	return c
}

// Ingested is how many new events were stored (wwz_panel_events_ingested_total).
func (c *Consumer) Ingested() int64 { return c.ingested.Load() }

// Acked is how many stream entries were acknowledged (stored, duplicated or
// discarded as invalid).
func (c *Consumer) Acked() int64 { return c.acked.Load() }

// Lag is the last lag read by Run (wwz_panel_stream_lag); -1 = unknown.
func (c *Consumer) Lag() int64 { return c.lag.Load() }

// Up reports whether the last step reached Valkey and the store.
func (c *Consumer) Up() bool { return c.up.Load() }

// Run consumes until ctx ends (then it returns ctx.Err()), retrying failures
// with exponential backoff from MinBackoff to MaxBackoff.
func (c *Consumer) Run(ctx context.Context) error {
	backoff := c.o.MinBackoff
	var lastLag time.Time
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Since(lastLag) >= c.o.LagEvery {
			lastLag = time.Now()
			c.RefreshLag(ctx)
		}
		_, err := c.Step(ctx)
		if err == nil {
			backoff = c.o.MinBackoff
			continue
		}
		if ctx.Err() == nil {
			c.log.Warn("panel: ev:stream indisponível; nova tentativa", "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, c.o.MaxBackoff)
	}
}

// RefreshLag reads XINFO GROUPS into Lag and logs when it passes LagAlert.
func (c *Consumer) RefreshLag(ctx context.Context) {
	n, err := c.v.StreamLag(ctx, Group)
	if err != nil {
		c.log.Debug("panel: XINFO GROUPS falhou", "err", err)
		return
	}
	c.lag.Store(n)
	if n > lagAlert {
		c.log.Warn("panel: atraso no ev:stream", "lag", n)
	}
}

// Step does one round: creates the group if needed, runs XAUTOCLAIM when
// due, reads one batch (own pending entries first, then new ones with
// BLOCK), stores it and acknowledges it. Returns how many new events were
// stored.
func (c *Consumer) Step(ctx context.Context) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.step(ctx)
	if err != nil {
		c.pending = true
		if strings.Contains(err.Error(), "NOGROUP") {
			c.ready = false
		}
	}
	c.up.Store(err == nil)
	return n, err
}

func (c *Consumer) step(ctx context.Context) (int, error) {
	cl, err := c.v.client()
	if err != nil {
		return 0, err
	}
	key := c.v.StreamKey()
	if !c.ready {
		cctx, cancel := c.v.ctx(ctx)
		err := cl.Do(cctx, cl.B().Arbitrary("XGROUP", "CREATE").Keys(key).Args(Group, "$", "MKSTREAM").Build()).Error()
		cancel()
		if err != nil && !valkey.IsValkeyBusyGroup(err) {
			return 0, fmt.Errorf("source: XGROUP CREATE: %w", err)
		}
		c.ready = true
	}
	total := 0
	if time.Since(c.lastClaim) >= c.o.ClaimEvery {
		n, err := c.claim(ctx, cl, key)
		total += n
		if err != nil {
			return total, err
		}
		c.lastClaim = time.Now()
	}
	count := strconv.Itoa(c.o.Count)
	var cmd valkey.Completed
	cctx, cancel := context.WithTimeout(ctx, c.v.timeout+c.o.Block)
	defer cancel()
	if c.pending {
		cmd = cl.B().Arbitrary("XREADGROUP", "GROUP", Group, c.o.Name, "COUNT", count, "STREAMS").Keys(key).Args("0").Build()
	} else {
		block := strconv.FormatInt(c.o.Block.Milliseconds(), 10)
		cmd = cl.B().Arbitrary("XREADGROUP", "GROUP", Group, c.o.Name, "COUNT", count, "BLOCK", block, "STREAMS").Keys(key).Args(">").Blocking()
	}
	streams, err := cl.Do(cctx, cmd).AsXRead()
	if err != nil && !valkey.IsValkeyNil(err) {
		return total, fmt.Errorf("source: XREADGROUP: %w", err)
	}
	entries := streams[key]
	if c.pending && len(entries) == 0 {
		c.pending = false
	}
	n, err := c.store(ctx, cl, key, entries)
	return total + n, err
}

// claim takes entries idle for more than ClaimIdle from any consumer
// (XAUTOCLAIM) and stores them; at most 10 rounds of Count.
func (c *Consumer) claim(ctx context.Context, cl valkey.Client, key string) (int, error) {
	idle := strconv.FormatInt(c.o.ClaimIdle.Milliseconds(), 10)
	start, total := "0-0", 0
	for range 10 {
		cctx, cancel := c.v.ctx(ctx)
		arr, err := cl.Do(cctx, cl.B().Arbitrary("XAUTOCLAIM").Keys(key).Args(Group, c.o.Name, idle, start, "COUNT", strconv.Itoa(c.o.Count)).Build()).ToArray()
		cancel()
		// Reply: next cursor, claimed entries, deleted ids.
		next, entries := "0-0", []valkey.XRangeEntry(nil)
		if err == nil && len(arr) >= 2 {
			next, _ = arr[0].ToString()
			entries, err = arr[1].AsXRange()
		}
		if err != nil {
			return total, fmt.Errorf("source: XAUTOCLAIM: %w", err)
		}
		n, err := c.store(ctx, cl, key, entries)
		total += n
		if err != nil {
			return total, err
		}
		if next == "0-0" || next == "" {
			break
		}
		start = next
	}
	return total, nil
}

// store decodes entries, inserts them in one batch and only then XACKs
// them. Entries without a valid d field are logged and acknowledged (they
// would block the group forever).
func (c *Consumer) store(ctx context.Context, cl valkey.Client, key string, entries []valkey.XRangeEntry) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	evs := make([]store.Event, 0, len(entries))
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
		d, ok := e.FieldValues["d"]
		if !ok {
			c.log.Warn("panel: entrada do ev:stream sem campo d; descartada", "id", e.ID)
			continue
		}
		ev, err := store.ParseEvent(e.ID, []byte(d))
		if err != nil {
			c.log.Warn("panel: evento inválido no ev:stream; descartado", "id", e.ID, "err", err)
			continue
		}
		evs = append(evs, ev)
	}
	n, err := c.sink.InsertBatch(ctx, evs)
	if err != nil {
		return 0, fmt.Errorf("source: gravar %d eventos: %w", len(evs), err)
	}
	c.ingested.Add(int64(n))
	cctx, cancel := c.v.ctx(ctx)
	defer cancel()
	acked, err := cl.Do(cctx, cl.B().Arbitrary("XACK").Keys(key).Args(append([]string{Group}, ids...)...).Build()).AsInt64()
	if err != nil {
		return n, fmt.Errorf("source: XACK: %w", err)
	}
	c.acked.Add(acked)
	return n, nil
}

// ---- mode without Valkey: GET /admin/requests ----

// PollerOptions configures NewRequestPoller.
type PollerOptions struct {
	// Every is the polling interval; 0 = 5 s.
	Every time.Duration
	// Last is the ?last= of each poll; 0 = 1000.
	Last int
	// Instance fills the instance field of the events; "" = host of the
	// admin URL.
	Instance string
	Logger   *slog.Logger
}

// RequestPoller is the event source without Valkey (store: memory): every
// Every it reads GET /admin/requests?last=Last and stores each record as a
// request (or error, status >= 400) event with stream_id "req:<request_id>".
// It needs admin.debug: true on wwz; Enabled reports it so the screen can
// warn.
type RequestPoller struct {
	a    *Admin
	sink EventSink
	o    PollerOptions
	log  *slog.Logger

	enabled  atomic.Bool
	ingested atomic.Int64
	up       atomic.Bool
}

// NewRequestPoller builds the poller of a writing to sink.
func NewRequestPoller(a *Admin, sink EventSink, o PollerOptions) *RequestPoller {
	if o.Every <= 0 {
		o.Every = 5 * time.Second
	}
	if o.Last <= 0 {
		o.Last = 1000
	}
	if o.Instance == "" {
		if u, err := url.Parse(a.BaseURL()); err == nil && u.Host != "" {
			o.Instance = u.Host
		} else {
			o.Instance = "wwz"
		}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &RequestPoller{a: a, sink: sink, o: o, log: o.Logger}
}

// Enabled is the enabled field of the last answer (admin.debug of wwz).
func (p *RequestPoller) Enabled() bool { return p.enabled.Load() }

// Ingested is how many new events were stored.
func (p *RequestPoller) Ingested() int64 { return p.ingested.Load() }

// Up reports whether the last poll succeeded.
func (p *RequestPoller) Up() bool { return p.up.Load() }

// Run polls until ctx ends (then it returns ctx.Err()); failures are
// logged and retried on the next tick.
func (p *RequestPoller) Run(ctx context.Context) error {
	t := time.NewTicker(p.o.Every)
	defer t.Stop()
	for {
		if _, err := p.Poll(ctx); err != nil && ctx.Err() == nil {
			p.log.Warn("panel: GET /admin/requests falhou", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Poll does one read and returns how many new events were stored. Records
// that cannot become an event (no request_id or time) are skipped.
func (p *RequestPoller) Poll(ctx context.Context) (int, error) {
	list, err := p.a.Requests(ctx, p.o.Last)
	p.up.Store(err == nil)
	if err != nil {
		return 0, err
	}
	p.enabled.Store(list.Enabled)
	evs := make([]store.Event, 0, len(list.Requests))
	for _, r := range list.Requests {
		ev, err := RecordEvent(r, p.o.Instance)
		if err != nil {
			p.log.Debug("panel: registro de /admin/requests ignorado", "err", err)
			continue
		}
		evs = append(evs, ev)
	}
	n, err := p.sink.InsertBatch(ctx, evs)
	if err != nil {
		p.up.Store(false)
		return 0, fmt.Errorf("source: gravar %d eventos: %w", len(evs), err)
	}
	p.ingested.Add(int64(n))
	return n, nil
}

// errNoRequestID rejects records that cannot have a stream id.
var errNoRequestID = errors.New("source: registro sem request_id")

// RecordEvent converts a /admin/requests record into the asyncapi Event
// (type request, or error when status >= 400) keyed "req:<request_id>".
// entity is the key when there is one, otherwise the IP.
func RecordEvent(r RequestRecord, instance string) (store.Event, error) {
	if r.RequestID == "" {
		return store.Event{}, errNoRequestID
	}
	typ := store.EventRequest
	if r.Status >= 400 {
		typ = store.EventError
	}
	fields := map[string]any{
		"duration_ms":   r.DurationMS,
		"prompt_tokens": r.PromptTokens,
		"output_tokens": r.CompletionTokens,
	}
	if r.ReasoningTokens > 0 {
		fields["reasoning_tokens"] = r.ReasoningTokens
	}
	if len(r.ToolCalls) > 0 {
		fields["tool_calls"] = r.ToolCalls
	}
	ev := map[string]any{
		"t": r.Time, "type": typ, "instance": instance, "request_id": r.RequestID,
		"driver": r.Driver, "model": r.Model, "status": r.Status, "fields": fields,
	}
	set := func(k, v string) {
		if v != "" {
			ev[k] = v
		}
	}
	set("test_id", r.TestID)
	set("key", r.Key)
	set("ip", r.IP)
	set("error", r.Error)
	// scenario and step feed the "Cenários" screen without Prometheus.
	set("scenario", r.Scenario)
	set("step", r.Step)
	switch {
	case r.Key != "":
		ev["entity"] = map[string]string{"kind": "key", "id": r.Key}
	case r.IP != "":
		ev["entity"] = map[string]string{"kind": "ip", "id": r.IP}
	}
	data, _ := json.Marshal(ev)
	return store.ParseEvent("req:"+r.RequestID, data)
}
