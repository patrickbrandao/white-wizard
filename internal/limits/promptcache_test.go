package limits_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

var pcAcct = limits.Entity{Kind: limits.KindAccount, ID: "acct-pc"}

// manyEntries returns n entries: the first with a short TTL, the others
// with an hour, so the cap drops the first.
func manyEntries(n int) []limits.PromptCacheEntry {
	out := make([]limits.PromptCacheEntry, n)
	for i := range out {
		out[i] = limits.PromptCacheEntry{Hash: fmt.Sprintf("%032x", i), TTL: time.Hour}
	}
	out[0].TTL = time.Minute
	return out
}

// testPCMaxEntries: above 65 536 entries per account the ones closest to
// expiring go first (cache/valkey/README.md 2.4).
func testPCMaxEntries(t *testing.T, pc limits.PromptCacher) {
	ctx := context.Background()
	now := time.Now()
	es := manyEntries(limits.PromptCacheMaxEntries + 1)
	n, err := pc.PromptCacheCommit(ctx, pcAcct, es, now)
	if err != nil || n != len(es) {
		t.Fatalf("commit = %d, %v", n, err)
	}
	if i, err := pc.PromptCacheLookup(ctx, pcAcct, []string{es[0].Hash}, now); err != nil || i != -1 {
		t.Fatalf("the shortest entry survived the cap: %d, %v", i, err)
	}
	if i, err := pc.PromptCacheLookup(ctx, pcAcct, []string{es[1].Hash, es[len(es)-1].Hash}, now); err != nil || i != 1 {
		t.Fatalf("lookup after the cap = %d, %v", i, err)
	}
	// Expired entries are pruned by the next commit; a commit of nothing
	// does nothing.
	if n, err := pc.PromptCacheCommit(ctx, pcAcct, nil, now); err != nil || n != 0 {
		t.Fatalf("empty commit = %d, %v", n, err)
	}
	late := now.Add(2 * time.Hour)
	if n, err := pc.PromptCacheCommit(ctx, pcAcct, []limits.PromptCacheEntry{{Hash: es[0].Hash, TTL: time.Minute}}, late); err != nil || n != 1 {
		t.Fatalf("commit after expiry = %d, %v", n, err)
	}
	if i, err := pc.PromptCacheLookup(ctx, pcAcct, []string{es[1].Hash, es[0].Hash}, late); err != nil || i != 1 {
		t.Fatalf("lookup after expiry = %d, %v", i, err)
	}
}

func TestMemoryPromptCacheMaxEntries(t *testing.T) {
	testPCMaxEntries(t, limits.NewMemory())
}

func TestValkeyPromptCacheMaxEntries(t *testing.T) {
	testPCMaxEntries(t, newValkeyStore(t))
}

// TestValkeyPromptCacheAcrossInstances (spec 007): a prefix committed by
// one instance is a hit on another sharing the Valkey; another account
// does not hit it.
func TestValkeyPromptCacheAcrossInstances(t *testing.T) {
	a := newValkeyStore(t)
	b := openValkey(t, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.Instance = "test-2" })
	ctx := context.Background()
	now := time.Now()
	es := []limits.PromptCacheEntry{
		{Hash: "00000000000000000000000000000001", TTL: time.Hour},
		{Hash: "00000000000000000000000000000002", TTL: time.Hour},
	}
	hashes := []string{es[0].Hash, es[1].Hash}
	if i, err := b.PromptCacheLookup(ctx, pcAcct, hashes, now); err != nil || i != -1 {
		t.Fatalf("lookup before commit = %d, %v", i, err)
	}
	if n, err := a.PromptCacheCommit(ctx, pcAcct, es, now); err != nil || n != 2 {
		t.Fatalf("commit on A = %d, %v", n, err)
	}
	if i, err := b.PromptCacheLookup(ctx, pcAcct, hashes, now); err != nil || i != 1 {
		t.Fatalf("lookup on B = %d, %v; want 1 (longest prefix)", i, err)
	}
	other := limits.Entity{Kind: limits.KindAccount, ID: "acct-other"}
	if i, err := b.PromptCacheLookup(ctx, other, hashes, now); err != nil || i != -1 {
		t.Fatalf("another account hit the prefix: %d, %v", i, err)
	}
}

// TestFallbackPromptCacheDown: with the circuit open, local uses the
// internal memory store; allow and deny miss and write nothing.
func TestFallbackPromptCacheDown(t *testing.T) {
	ctx := context.Background()
	e := []limits.PromptCacheEntry{{Hash: "00000000000000000000000000000001", TTL: time.Minute}}
	for _, mode := range []string{"allow", "deny", "local"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := downStore(t, mode)
			now := time.Now()
			n, err := f.PromptCacheCommit(ctx, pcAcct, e, now)
			want := map[string]int{"allow": 0, "deny": 0, "local": 1}[mode]
			if err != nil || n != want {
				t.Fatalf("commit = %d, %v; want %d", n, err, want)
			}
			i, err := f.PromptCacheLookup(ctx, pcAcct, []string{e[0].Hash}, now)
			if err != nil || i != want-1 {
				t.Fatalf("lookup = %d, %v; want %d", i, err, want-1)
			}
		})
	}
}

// TestFallbackPromptCacheOutage: with Valkey up the registry is shared; a
// lookup error is a miss that counts for the circuit, a commit error does
// not, and a caller that went away never opens it.
func TestFallbackPromptCacheOutage(t *testing.T) {
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
	h := []string{"00000000000000000000000000000001"}
	if n, err := f.PromptCacheCommit(ctx, pcAcct, []limits.PromptCacheEntry{{Hash: h[0], TTL: time.Minute}}, now); err != nil || n != 1 {
		t.Fatalf("commit = %d, %v", n, err)
	}
	if i, err := f.PromptCacheLookup(ctx, pcAcct, h, now); err != nil || i != 0 {
		t.Fatalf("lookup = %d, %v", i, err)
	}
	if i, err := f.Valkey().PromptCacheLookup(ctx, pcAcct, nil, now); err != nil || i != -1 {
		t.Fatalf("empty lookup = %d, %v", i, err)
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if i, err := f.PromptCacheLookup(gone, pcAcct, h, now); err == nil || i != -1 || f.CircuitOpen() {
		t.Fatalf("caller gone: %d, %v, open %v", i, err, f.CircuitOpen())
	}
	p.stop()
	if _, err := f.PromptCacheCommit(ctx, pcAcct, []limits.PromptCacheEntry{{Hash: h[0], TTL: time.Minute}}, now); err == nil {
		t.Fatal("commit with Valkey down returned no error")
	}
	for range 3 {
		if i, err := f.PromptCacheLookup(ctx, pcAcct, h, now); err == nil || i != -1 {
			t.Fatalf("lookup with Valkey down = %d, %v", i, err)
		}
	}
	if !f.CircuitOpen() {
		t.Fatal("lookup failures did not open the circuit")
	}
	p.start(p.addr)
}
