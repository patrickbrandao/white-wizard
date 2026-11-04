package limits_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

func respRecord(acct string, at time.Time) limits.ResponseRecord {
	return limits.ResponseRecord{Account: acct, Driver: "openai", Model: "gpt-4o-mini", Messages: 3, Tokens: 27, Prefix: "0123456789abcdef0123456789abcdef", At: at}
}

func mustRespGet(t *testing.T, r limits.ResponseRegistry, id, acct string, now time.Time) *limits.ResponseRecord {
	t.Helper()
	rec, err := r.ResponseGet(context.Background(), id, acct, now)
	if err != nil {
		t.Fatalf("ResponseGet(%q): %v", id, err)
	}
	return rec
}

func mustRespPut(t *testing.T, r limits.ResponseRegistry, id string, rec limits.ResponseRecord) {
	t.Helper()
	if err := r.ResponsePut(context.Background(), id, rec); err != nil {
		t.Fatalf("ResponsePut(%q): %v", id, err)
	}
}

// TestMemoryResponseExpiry: a record lives exactly 30 days from its
// creation; reads do not renew it and an expired read drops it.
func TestMemoryResponseExpiry(t *testing.T) {
	m := limits.NewMemory()
	t0 := time.UnixMilli(1_700_000_000_000)
	mustRespPut(t, m, "resp_1", respRecord("a", t0))
	if mustRespGet(t, m, "resp_1", "a", t0.Add(limits.ResponseTTL-time.Millisecond)) == nil {
		t.Fatal("not found just before the TTL")
	}
	if mustRespGet(t, m, "resp_1", "a", t0.Add(limits.ResponseTTL)) != nil {
		t.Fatal("found at the TTL")
	}
	if n := m.ResponseCount(); n != 0 {
		t.Fatalf("count after expiry = %d", n)
	}
}

// TestMemoryResponseZeroTimes: a zero At is the write time and a zero now
// is the wall clock.
func TestMemoryResponseZeroTimes(t *testing.T) {
	m := limits.NewMemory()
	before := time.Now().Truncate(time.Millisecond)
	mustRespPut(t, m, "resp_1", respRecord("a", time.Time{}))
	rec := mustRespGet(t, m, "resp_1", "a", time.Time{})
	if rec == nil || rec.At.Before(before) || rec.At.After(time.Now()) {
		t.Fatalf("record = %+v", rec)
	}
}

// TestMemoryResponseLRU: above the capacity the least recently written or
// read record is dropped.
func TestMemoryResponseLRU(t *testing.T) {
	m := limits.NewMemory()
	m.SetResponseCapacity(2)
	now := time.Now()
	mustRespPut(t, m, "a", respRecord("x", now))
	mustRespPut(t, m, "b", respRecord("x", now))
	if mustRespGet(t, m, "a", "x", now) == nil { // a is now the most recent
		t.Fatal("a missing")
	}
	mustRespPut(t, m, "c", respRecord("x", now))
	if mustRespGet(t, m, "b", "x", now) != nil {
		t.Fatal("b survived the eviction")
	}
	mustRespPut(t, m, "a", respRecord("y", now)) // overwrite refreshes a
	mustRespPut(t, m, "d", respRecord("x", now))
	if mustRespGet(t, m, "c", "x", now) != nil || mustRespGet(t, m, "a", "y", now) == nil || mustRespGet(t, m, "d", "x", now) == nil {
		t.Fatal("wrong eviction after overwrite")
	}
	if n := m.ResponseCount(); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	// Order is now d, a. A read from another account does not refresh a.
	if mustRespGet(t, m, "a", "x", now) != nil {
		t.Fatal("other account found a")
	}
	mustRespPut(t, m, "e", respRecord("x", now))
	if mustRespGet(t, m, "a", "y", now) != nil || mustRespGet(t, m, "d", "x", now) == nil {
		t.Fatal("a probe by another account refreshed the record")
	}
}

// TestMemoryResponseDefaultCapacity: n <= 0 keeps the default of 1 M.
func TestMemoryResponseDefaultCapacity(t *testing.T) {
	if limits.ResponseMaxRecords != 1_000_000 {
		t.Fatalf("ResponseMaxRecords = %d", limits.ResponseMaxRecords)
	}
	m := limits.NewMemory()
	m.SetResponseCapacity(0)
	now := time.Now()
	for i := range 1000 {
		mustRespPut(t, m, fmt.Sprint(i), respRecord("x", now))
	}
	if n := m.ResponseCount(); n != 1000 {
		t.Fatalf("count = %d", n)
	}
}

// TestMemoryResponseParallel exercises the lock under -race.
func TestMemoryResponseParallel(t *testing.T) {
	m := limits.NewMemory()
	m.SetResponseCapacity(64)
	now := time.Now()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 200 {
				id := fmt.Sprint(g, "-", i%100)
				_ = m.ResponsePut(context.Background(), id, respRecord("x", now))
				_, _ = m.ResponseGet(context.Background(), id, "x", now)
			}
		})
	}
	wg.Wait()
	if n := m.ResponseCount(); n != 64 {
		t.Fatalf("count = %d", n)
	}
}

// TestValkeyResponseSchema: key, fields and TTL follow README 2.5; a read
// does not renew the TTL; a malformed record is not found.
func TestValkeyResponseSchema(t *testing.T) {
	v := newValkeyStore(t)
	ctx := context.Background()
	at := time.UnixMilli(1_700_000_000_123)
	mustRespPut(t, v, "resp_a:b", respRecord("acct-a", at))
	key := "wwz:resp:{resp:resp_a%3Ab}"
	if ks := keysMatching(t, v, "wwz:resp:*"); len(ks) != 1 || ks[0] != key {
		t.Fatalf("keys = %v, want [%s]", ks, key)
	}
	c := v.Client()
	h, err := c.Do(ctx, c.B().Hgetall().Key(key).Build()).AsStrMap()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"acct": "acct-a", "drv": "openai", "model": "gpt-4o-mini", "msgs": "3", "toks": "27", "pfx": "0123456789abcdef0123456789abcdef", "at": "1700000000123"}
	if fmt.Sprint(h) != fmt.Sprint(want) {
		t.Fatalf("hash = %v, want %v", h, want)
	}
	pttl := func() int64 {
		n, err := c.Do(ctx, c.B().Pttl().Key(key).Build()).AsInt64()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := pttl(); n <= limits.ResponseTTL.Milliseconds()-60_000 || n > limits.ResponseTTL.Milliseconds() {
		t.Fatalf("PTTL = %d", n)
	}
	if err := c.Do(ctx, c.B().Pexpire().Key(key).Milliseconds(60_000).Build()).Error(); err != nil {
		t.Fatal(err)
	}
	if mustRespGet(t, v, "resp_a:b", "acct-a", time.Time{}) == nil {
		t.Fatal("not found")
	}
	if n := pttl(); n > 60_000 {
		t.Fatalf("read renewed the TTL: %d", n)
	}
	// Malformed numbers: not found, no error.
	for _, f := range []string{"msgs", "toks", "at"} {
		mustRespPut(t, v, "resp_bad", respRecord("acct-a", at))
		if err := c.Do(ctx, c.B().Hset().Key("wwz:resp:{resp:resp_bad}").FieldValue().FieldValue(f, "x").Build()).Error(); err != nil {
			t.Fatal(err)
		}
		if rec := mustRespGet(t, v, "resp_bad", "acct-a", time.Time{}); rec != nil {
			t.Fatalf("malformed %s found: %+v", f, rec)
		}
	}
	// Reset deletes the records.
	if err := v.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if ks := keysMatching(t, v, "wwz:resp:*"); len(ks) != 0 {
		t.Fatalf("keys after Reset = %v", ks)
	}
}

// TestValkeyResponseAcrossInstances: a record written by one instance is
// read by another (spec 012).
func TestValkeyResponseAcrossInstances(t *testing.T) {
	a := newValkeyStore(t)
	b := openValkey(t, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.Instance = "test-2" })
	rec := respRecord("acct-a", time.Now())
	mustRespPut(t, a, "resp_shared", rec)
	got := mustRespGet(t, b, "resp_shared", "acct-a", time.Now())
	if got == nil || got.Tokens != rec.Tokens || got.Model != rec.Model || got.At.UnixMilli() != rec.At.UnixMilli() {
		t.Fatalf("other instance read %+v", got)
	}
	if mustRespGet(t, b, "resp_shared", "acct-b", time.Now()) != nil {
		t.Fatal("other account found the record")
	}
}

// TestFallbackResponsesDown: with the circuit open, local uses the internal
// memory store; allow and deny write nothing and find nothing.
func TestFallbackResponsesDown(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"allow", "deny", "local"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := downStore(t, mode)
			if err := f.ResponsePut(ctx, "resp_1", respRecord("a", time.Now())); err != nil {
				t.Fatalf("put: %v", err)
			}
			rec, err := f.ResponseGet(ctx, "resp_1", "a", time.Now())
			if err != nil || (rec != nil) != (mode == "local") {
				t.Fatalf("get = %+v, %v", rec, err)
			}
		})
	}
}

// TestFallbackResponsesOutage: with Valkey up the registry is shared; a
// read error is "not found" and counts for the circuit, a write error does
// not, and a caller that went away never opens it.
func TestFallbackResponsesOutage(t *testing.T) {
	addr, db := valkeyEnv(t)
	p := newProxy(t, addr)
	cfg := testStoreConfig(p.addr)
	cfg.ReadTimeout = 200 * time.Millisecond
	cfg.OnDown = "local"
	f := limits.OpenValkey(context.Background(), cfg, limits.ValkeyOptions{DB: db, Heartbeat: -1}, limits.FallbackOptions{ProbeInterval: 20 * time.Millisecond})
	t.Cleanup(func() { _ = f.Close() })
	flush(t, f.Valkey())
	ctx := context.Background()
	now := time.Now()
	mustRespPut(t, f, "resp_1", respRecord("a", now))
	if mustRespGet(t, f, "resp_1", "a", now) == nil {
		t.Fatal("not found through the fallback")
	}
	if mustRespGet(t, f.Valkey(), "resp_1", "a", now) == nil {
		t.Fatal("not in Valkey")
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if rec, err := f.ResponseGet(gone, "resp_1", "a", now); err == nil || rec != nil || f.CircuitOpen() {
		t.Fatalf("caller gone: %+v, %v, open %v", rec, err, f.CircuitOpen())
	}
	p.stop()
	for range 3 {
		if err := f.ResponsePut(ctx, "resp_2", respRecord("a", now)); err == nil {
			t.Fatal("put with Valkey down returned no error")
		}
	}
	if f.CircuitOpen() {
		t.Fatal("put failures opened the circuit")
	}
	for range 3 {
		if rec, err := f.ResponseGet(ctx, "resp_1", "a", now); err == nil || rec != nil {
			t.Fatalf("get with Valkey down = %+v, %v", rec, err)
		}
	}
	if !f.CircuitOpen() {
		t.Fatal("get failures did not open the circuit")
	}
	// Open circuit, local: the internal memory store answers.
	mustRespPut(t, f, "resp_3", respRecord("a", now))
	if mustRespGet(t, f, "resp_3", "a", now) == nil {
		t.Fatal("local registry empty while down")
	}
	p.start(p.addr)
}
