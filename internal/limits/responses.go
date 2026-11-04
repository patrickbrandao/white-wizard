package limits

import (
	"container/list"
	"context"
	"strconv"
	"sync"
	"time"
)

// Responses API registry (cache/valkey/README.md 2.5, ADR 0055, spec 012):
// one compact record per response stored for previous_response_id. It never
// holds text, tools or instructions.

const (
	// ResponseTTL is how long a record lives from its creation (the real
	// provider's retention). Reads never renew it.
	ResponseTTL = 30 * 24 * time.Hour
	// ResponseMaxRecords is the default LRU capacity of the memory store.
	ResponseMaxRecords = 1_000_000
)

// ResponseRecord is the state kept for one response (README 2.5 fields).
type ResponseRecord struct {
	// Account is the raw id of the owning account (keys[].account); a
	// lookup from another account does not find the record.
	Account string
	// Driver is the driver that wrote the record (openai, azure, ...).
	Driver string
	// Model is the canonical models[].id.
	Model string
	// Messages counts the messages of the accumulated conversation.
	Messages int
	// Tokens of the accumulated conversation, with the per-message overhead
	// and without the 3 priming tokens of the request.
	Tokens int
	// Prefix is the prompt-cache prefix hash at the end of the accumulated
	// conversation (32 hex), empty when the model has no cache.
	Prefix string
	// At is the creation time (ms precision); zero means now on ResponsePut.
	At time.Time
}

// ResponseRegistry is implemented by stores that keep the Responses API
// registry (optional, like PromptCacher).
type ResponseRegistry interface {
	// ResponsePut writes (or overwrites) the record of response id, with a
	// TTL of ResponseTTL from now. Called on the first byte of the response.
	ResponsePut(ctx context.Context, id string, rec ResponseRecord) error
	// ResponseGet returns the record of response id when it exists, is
	// alive at now and belongs to account; otherwise nil. An error is also
	// "not found" for the caller (previous_response_not_found).
	ResponseGet(ctx context.Context, id, account string, now time.Time) (*ResponseRecord, error)
}

var (
	_ ResponseRegistry = (*Memory)(nil)
	_ ResponseRegistry = (*Valkey)(nil)
	_ ResponseRegistry = (*Fallback)(nil)
)

// respLRU is the memory registry: a map with LRU eviction by write or read.
type respLRU struct {
	mu  sync.Mutex
	max int
	ll  *list.List // front = most recent; values are *respItem
	m   map[string]*list.Element
}

type respItem struct {
	id  string
	rec ResponseRecord
}

func newRespLRU(n int) *respLRU {
	if n <= 0 {
		n = ResponseMaxRecords
	}
	return &respLRU{max: n, ll: list.New(), m: map[string]*list.Element{}}
}

func (r *respLRU) put(id string, rec ResponseRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.m[id]; ok {
		el.Value.(*respItem).rec = rec
		r.ll.MoveToFront(el)
		return
	}
	r.m[id] = r.ll.PushFront(&respItem{id: id, rec: rec})
	for r.ll.Len() > r.max {
		old := r.ll.Back()
		r.ll.Remove(old)
		delete(r.m, old.Value.(*respItem).id)
	}
}

func (r *respLRU) get(id, account string, now time.Time) *ResponseRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.m[id]
	if !ok {
		return nil
	}
	it := el.Value.(*respItem)
	if !now.Before(it.rec.At.Add(ResponseTTL)) {
		r.ll.Remove(el)
		delete(r.m, id)
		return nil
	}
	if it.rec.Account != account {
		return nil
	}
	r.ll.MoveToFront(el)
	rec := it.rec
	return &rec
}

func (r *respLRU) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ll.Len()
}

func (r *respLRU) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ll.Init()
	r.m = map[string]*list.Element{}
}

// responses returns the memory registry, created on first use.
func (m *Memory) responses() *respLRU {
	m.respOnce.Do(func() { m.resp = newRespLRU(m.respMax) })
	return m.resp
}

// SetResponseCapacity sets the LRU capacity of the Responses API registry
// (default ResponseMaxRecords; n <= 0 keeps the default). It must be called
// before the first ResponsePut or ResponseGet.
func (m *Memory) SetResponseCapacity(n int) { m.respMax = n }

// ResponseCount is the number of records held (alive or not yet pruned).
func (m *Memory) ResponseCount() int { return m.responses().len() }

// ResponsePut implements ResponseRegistry.
func (m *Memory) ResponsePut(_ context.Context, id string, rec ResponseRecord) error {
	rec.At = respAt(rec.At)
	m.responses().put(id, rec)
	return nil
}

// ResponseGet implements ResponseRegistry. Expiry is checked against now
// (time.Now when zero).
func (m *Memory) ResponseGet(_ context.Context, id, account string, now time.Time) (*ResponseRecord, error) {
	if now.IsZero() {
		now = time.Now()
	}
	return m.responses().get(id, account, now), nil
}

// respAt is the creation time with ms precision (now when zero).
func respAt(at time.Time) time.Time {
	if at.IsZero() {
		at = time.Now()
	}
	return time.UnixMilli(at.UnixMilli())
}

// ---- Valkey ----

// respKey is P:resp:{resp:<escaped id>}.
func (v *Valkey) respKey(id string) string { return v.p + ":resp:{resp:" + escapeID(id) + "}" }

// ResponsePut implements ResponseRegistry: MULTI / HSET / PEXPIRE / EXEC.
func (v *Valkey) ResponsePut(ctx context.Context, id string, rec ResponseRecord) error {
	k := v.respKey(id)
	b := v.c.B()
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	res := v.c.DoMulti(cctx,
		b.Multi().Build(),
		b.Hset().Key(k).FieldValue().
			FieldValue("acct", rec.Account).
			FieldValue("drv", rec.Driver).
			FieldValue("model", rec.Model).
			FieldValue("msgs", strconv.Itoa(rec.Messages)).
			FieldValue("toks", strconv.Itoa(rec.Tokens)).
			FieldValue("pfx", rec.Prefix).
			FieldValue("at", ms(respAt(rec.At))).Build(),
		b.Pexpire().Key(k).Milliseconds(ResponseTTL.Milliseconds()).Build(),
		b.Exec().Build(),
	)
	return firstErr(res)
}

// ResponseGet implements ResponseRegistry: HGETALL, account checked in Go.
// The expiry is the key TTL; now is not used. A malformed record is not
// found.
func (v *Valkey) ResponseGet(ctx context.Context, id, account string, _ time.Time) (*ResponseRecord, error) {
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	h, err := v.c.Do(cctx, v.c.B().Hgetall().Key(v.respKey(id)).Build()).AsStrMap()
	if err != nil {
		return nil, err
	}
	if len(h) == 0 || h["acct"] != account {
		return nil, nil
	}
	msgs, err1 := strconv.Atoi(h["msgs"])
	toks, err2 := strconv.Atoi(h["toks"])
	at, err3 := strconv.ParseInt(h["at"], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, nil
	}
	return &ResponseRecord{
		Account: h["acct"], Driver: h["drv"], Model: h["model"],
		Messages: msgs, Tokens: toks, Prefix: h["pfx"], At: time.UnixMilli(at),
	}, nil
}

// ---- Fallback ----

// ResponsePut implements ResponseRegistry. A failure is only returned (the
// driver logs it at debug); it never counts for the circuit. With the
// circuit open, local writes to the internal memory store and allow/deny
// write nothing.
func (f *Fallback) ResponsePut(ctx context.Context, id string, rec ResponseRecord) error {
	if v := f.live(); v != nil {
		return v.ResponsePut(ctx, id, rec)
	}
	if f.mode == "local" {
		return f.memory().ResponsePut(ctx, id, rec)
	}
	return nil
}

// ResponseGet implements ResponseRegistry. An error is "not found" and
// counts for the circuit like Check (unless the caller went away); with the
// circuit open, local reads the internal memory store and allow/deny find
// nothing.
func (f *Fallback) ResponseGet(ctx context.Context, id, account string, now time.Time) (*ResponseRecord, error) {
	if v := f.live(); v != nil {
		rec, err := v.ResponseGet(ctx, id, account, now)
		if err == nil {
			f.success()
			return rec, nil
		}
		if !callerGone(ctx) {
			f.failure(err)
		}
		return nil, err
	}
	if f.mode == "local" {
		return f.memory().ResponseGet(ctx, id, account, now)
	}
	return nil, nil
}
