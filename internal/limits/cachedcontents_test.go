package limits_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

func ccEntry(owner, name string, create time.Time, ttl time.Duration) limits.CachedContent {
	return limits.CachedContent{
		Owner: owner, Name: name, Model: "models/gemini-3.5-flash-lite", Display: "probe", Tokens: 3281,
		Create: create, Update: create, Expire: create.Add(ttl), Body: []byte(`{"s":"sys"}`),
	}
}

func mustCCGet(t *testing.T, s limits.CachedContentStore, owner, name string, now time.Time) *limits.CachedContent {
	t.Helper()
	c, err := s.CachedContentGet(context.Background(), owner, name, now)
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return c
}

func mustCCPut(t *testing.T, s limits.CachedContentStore, c limits.CachedContent) {
	t.Helper()
	if err := s.CachedContentPut(context.Background(), c); err != nil {
		t.Fatalf("put %s: %v", c.Name, err)
	}
}

func ccNames(t *testing.T, s limits.CachedContentStore, owner string, now time.Time) string {
	t.Helper()
	l, err := s.CachedContentList(context.Background(), owner, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var out []string
	for _, c := range l {
		out = append(out, c.Name)
	}
	return fmt.Sprint(out)
}

// ccSuite is the behavior every store shares: owners apart, oldest first,
// replace, expiry at Expire, delete.
func ccSuite(t *testing.T, s limits.CachedContentStore) {
	t.Helper()
	ctx := context.Background()
	t0 := time.UnixMicro(1_800_000_000_000_001)
	mustCCPut(t, s, ccEntry("p:a/p", "cachedContents/b", t0.Add(time.Second), time.Hour))
	mustCCPut(t, s, ccEntry("p:a/p", "cachedContents/a", t0, 2*time.Hour))
	mustCCPut(t, s, ccEntry("p:a/p", "cachedContents/c", t0, time.Minute))
	mustCCPut(t, s, ccEntry("a:other", "cachedContents/a", t0, time.Hour))
	if got := ccNames(t, s, "p:a/p", t0); got != "[cachedContents/a cachedContents/c cachedContents/b]" {
		t.Fatalf("list = %s", got)
	}
	c := mustCCGet(t, s, "p:a/p", "cachedContents/a", t0)
	if c == nil || c.Tokens != 3281 || c.Model != "models/gemini-3.5-flash-lite" || c.Display != "probe" ||
		!c.Create.Equal(t0) || !c.Expire.Equal(t0.Add(2*time.Hour)) || string(c.Body) != `{"s":"sys"}` {
		t.Fatalf("get = %+v", c)
	}
	if mustCCGet(t, s, "a:nobody", "cachedContents/a", t0) != nil {
		t.Fatal("another owner found the entry")
	}
	// Replace (patch of the expiry).
	upd := ccEntry("p:a/p", "cachedContents/c", t0, 3*time.Hour)
	upd.Update = t0.Add(time.Minute)
	mustCCPut(t, s, upd)
	if c := mustCCGet(t, s, "p:a/p", "cachedContents/c", t0.Add(2*time.Minute)); c == nil || !c.Update.Equal(upd.Update) {
		t.Fatalf("patched = %+v", c)
	}
	// Expiry: at Expire the entry is gone.
	if mustCCGet(t, s, "p:a/p", "cachedContents/b", t0.Add(time.Second+time.Hour)) != nil {
		t.Fatal("found at its expiry")
	}
	if got := ccNames(t, s, "p:a/p", t0.Add(time.Second+time.Hour)); got != "[cachedContents/a cachedContents/c]" {
		t.Fatalf("list after an expiry = %s", got)
	}
	if err := s.CachedContentDelete(ctx, "p:a/p", "cachedContents/a"); err != nil {
		t.Fatal(err)
	}
	if err := s.CachedContentDelete(ctx, "p:a/p", "cachedContents/none"); err != nil {
		t.Fatal(err)
	}
	if mustCCGet(t, s, "p:a/p", "cachedContents/a", t0) != nil {
		t.Fatal("found after delete")
	}
	if got := ccNames(t, s, "nobody", t0); got != "[]" {
		t.Fatalf("empty owner = %s", got)
	}
}

func TestMemoryCachedContents(t *testing.T) {
	m := limits.NewMemory()
	ccSuite(t, m)
	t0 := time.Now()
	// The listing drops an expired entry it meets.
	mustCCPut(t, m, ccEntry("o", "cachedContents/old", t0, time.Second))
	if got := ccNames(t, m, "o", t0.Add(time.Second)); got != "[]" {
		t.Fatalf("expired listed: %s", got)
	}
	mustCCPut(t, m, ccEntry("o", "cachedContents/x", t0, time.Hour))
	if err := m.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mustCCGet(t, m, "o", "cachedContents/x", t0) != nil {
		t.Fatal("found after Reset")
	}
}

// TestSharedCachedContents: without a shared store the process-local one
// answers; a store that keeps cachedContents becomes the shared one, a
// store that does not leaves it.
func TestSharedCachedContents(t *testing.T) {
	m := limits.NewMemory()
	limits.ShareCachedContents(m)
	if limits.SharedCachedContents() != limits.CachedContentStore(m) {
		t.Fatal("memory store not shared")
	}
	limits.ShareCachedContents(noCC{m})
	if limits.SharedCachedContents() != limits.CachedContentStore(m) {
		t.Fatal("a store without cachedContents replaced the shared one")
	}
}

// noCC is a Store that does not keep cachedContents.
type noCC struct{ limits.Store }

func TestValkeyCachedContents(t *testing.T) {
	v := newValkeyStore(t)
	ccSuite(t, v)
}

// TestValkeyCachedContentsSchema: the keys and fields of
// tmp/spec-batch/265.md (cache/valkey/README.md 2.6), the TTLs, another
// instance reading the same entry, the listing dropping members whose hash
// is gone, malformed hashes and Reset.
func TestValkeyCachedContentsSchema(t *testing.T) {
	v := newValkeyStore(t)
	ctx := context.Background()
	now := time.Now()
	create := time.UnixMicro(now.UnixMicro())
	e := ccEntry("p:acme/p 1", "cachedContents/abc", create, 10*time.Minute)
	e.Expire = e.Expire.Add(123) // ns precision survives
	mustCCPut(t, v, e)
	idx, key := "wwz:gcc:{gcc:p%3Aacme/p%201}", "wwz:gcc:{gcc:p%3Aacme/p%201}:cachedContents/abc"
	if ks := keysMatching(t, v, "wwz:gcc:*"); fmt.Sprint(ks) != fmt.Sprint([]string{idx, key}) && fmt.Sprint(ks) != fmt.Sprint([]string{key, idx}) {
		t.Fatalf("keys = %v", ks)
	}
	c := v.Client()
	h, err := c.Do(ctx, c.B().Hgetall().Key(key).Build()).AsStrMap()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"model": "models/gemini-3.5-flash-lite", "disp": "probe", "toks": "3281",
		"ct": fmt.Sprint(create.UnixMicro()), "ut": fmt.Sprint(create.UnixMicro()), "exp": fmt.Sprint(e.Expire.UnixNano()), "body": `{"s":"sys"}`,
	}
	if fmt.Sprint(h) != fmt.Sprint(want) {
		t.Fatalf("hash = %v, want %v", h, want)
	}
	for _, k := range []string{key, idx} {
		n, err := c.Do(ctx, c.B().Pttl().Key(k).Build()).AsInt64()
		if err != nil || n <= 9*60_000 || n > 10*60_000+1 {
			t.Fatalf("PTTL %s = %d, %v", k, n, err)
		}
	}
	// A longer entry extends the index; a shorter one does not shorten it.
	mustCCPut(t, v, ccEntry("p:acme/p 1", "cachedContents/long", create, time.Hour))
	mustCCPut(t, v, ccEntry("p:acme/p 1", "cachedContents/short", create, time.Minute))
	if n, _ := c.Do(ctx, c.B().Pttl().Key(idx).Build()).AsInt64(); n <= 59*60_000 {
		t.Fatalf("index PTTL = %d", n)
	}
	got := mustCCGet(t, v, "p:acme/p 1", "cachedContents/abc", now)
	if got == nil || !got.Expire.Equal(e.Expire) || !got.Create.Equal(create) {
		t.Fatalf("get = %+v", got)
	}
	other := openValkey(t, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.Instance = "test-2" })
	if mustCCGet(t, other, "p:acme/p 1", "cachedContents/abc", now) == nil {
		t.Fatal("another instance does not see the entry")
	}
	// A member whose hash is gone leaves the index when listed.
	if err := c.Do(ctx, c.B().Del().Key(idx+":cachedContents/short").Build()).Error(); err != nil {
		t.Fatal(err)
	}
	if got := ccNames(t, v, "p:acme/p 1", now); got != "[cachedContents/abc cachedContents/long]" {
		t.Fatalf("list = %s", got)
	}
	if n, _ := c.Do(ctx, c.B().Zcard().Key(idx).Build()).AsInt64(); n != 2 {
		t.Fatalf("index size = %d", n)
	}
	// Malformed numbers: not found.
	for _, f := range []string{"toks", "ct", "ut", "exp"} {
		mustCCPut(t, v, ccEntry("o", "cachedContents/bad", create, time.Hour))
		if err := c.Do(ctx, c.B().Hset().Key("wwz:gcc:{gcc:o}:cachedContents/bad").FieldValue().FieldValue(f, "x").Build()).Error(); err != nil {
			t.Fatal(err)
		}
		if mustCCGet(t, v, "o", "cachedContents/bad", now) != nil {
			t.Fatalf("malformed %s found", f)
		}
	}
	// A member whose key is not a hash: the listing fails.
	if err := c.Do(ctx, c.B().Set().Key("wwz:gcc:{gcc:o}:cachedContents/bad").Value("x").Build()).Error(); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CachedContentList(ctx, "o", now); err == nil {
		t.Fatal("list over a wrong type: no error")
	}
	if err := v.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if ks := keysMatching(t, v, "wwz:gcc:*"); len(ks) != 0 {
		t.Fatalf("keys after Reset = %v", ks)
	}
}

// TestFallbackCachedContentsDown: with the circuit open, local uses the
// internal memory store; allow and deny write nothing and find nothing.
func TestFallbackCachedContentsDown(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"allow", "deny", "local"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := downStore(t, mode)
			now := time.Now()
			mustCCPut(t, f, ccEntry("o", "cachedContents/a", now, time.Hour))
			c := mustCCGet(t, f, "o", "cachedContents/a", now)
			l, err := f.CachedContentList(ctx, "o", now)
			if err != nil || (c != nil) != (mode == "local") || (len(l) == 1) != (mode == "local") {
				t.Fatalf("get = %+v, list = %v, %v", c, l, err)
			}
			if err := f.CachedContentDelete(ctx, "o", "cachedContents/a"); err != nil {
				t.Fatal(err)
			}
			if mustCCGet(t, f, "o", "cachedContents/a", now) != nil {
				t.Fatal("found after delete")
			}
		})
	}
}

// TestFallbackCachedContentsOutage: with Valkey up the entries are shared;
// a read error counts for the circuit, a write error does not, and a
// caller that went away never opens it.
func TestFallbackCachedContentsOutage(t *testing.T) {
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
	ccSuite(t, f)
	mustCCPut(t, f, ccEntry("o", "cachedContents/a", now, time.Hour))
	if mustCCGet(t, f.Valkey(), "o", "cachedContents/a", now) == nil {
		t.Fatal("not in Valkey")
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if c, err := f.CachedContentGet(gone, "o", "cachedContents/a", now); err == nil || c != nil || f.CircuitOpen() {
		t.Fatalf("caller gone: %+v, %v, open %v", c, err, f.CircuitOpen())
	}
	p.stop()
	for range 3 {
		if f.CachedContentPut(ctx, ccEntry("o", "cachedContents/b", now, time.Hour)) == nil || f.CachedContentDelete(ctx, "o", "cachedContents/a") == nil {
			t.Fatal("write with Valkey down returned no error")
		}
	}
	if f.CircuitOpen() {
		t.Fatal("write failures opened the circuit")
	}
	if _, err := f.CachedContentList(ctx, "o", now); err == nil {
		t.Fatal("list with Valkey down returned no error")
	}
	for i := 0; i < 5 && !f.CircuitOpen(); i++ {
		if c, err := f.CachedContentGet(ctx, "o", "cachedContents/a", now); err == nil || c != nil {
			t.Fatalf("get with Valkey down = %+v, %v", c, err)
		}
	}
	if !f.CircuitOpen() {
		t.Fatal("read failures did not open the circuit")
	}
	mustCCPut(t, f, ccEntry("o", "cachedContents/c", now, time.Hour))
	if mustCCGet(t, f, "o", "cachedContents/c", now) == nil {
		t.Fatal("local memory store did not answer")
	}
}
