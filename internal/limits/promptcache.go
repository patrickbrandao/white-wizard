package limits

import (
	"context"
	"hash/maphash"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Prompt-cache registry (cache/valkey/README.md 2.4 and 3.14, ADR 0046).
// The engine hashes the prompt prefixes; the store only keeps, per account,
// which prefix hashes are alive and until when.

const (
	// PromptCacheMaxEntries caps the live entries of one account; above it
	// the entries closest to expiring are dropped.
	PromptCacheMaxEntries = 65536
	// PromptCacheMaxCandidates caps the hashes of one lookup or commit.
	PromptCacheMaxCandidates = 1024
	// pcPrune is how many expired entries a commit removes at most.
	pcPrune = 64
)

// PromptCacheEntry is one prefix written or renewed by PromptCacheCommit.
type PromptCacheEntry struct {
	// Hash identifies the prefix (32 lower-case hex characters).
	Hash string
	// TTL is the provider's time to live, renewed on every read.
	TTL time.Duration
	// MaxLife caps the life of the entry counted from its creation; 0 means
	// no cap.
	MaxLife time.Duration
}

// PromptCacher is implemented by stores that keep the prompt-cache registry
// (optional, like CounterResetter). Account is the entity that owns the
// cache: the key's account, or the client ip without a key.
type PromptCacher interface {
	// PromptCacheLookup returns the index of the last hash (the hashes go
	// in ascending prefix length) whose entry is alive at now, or -1. It is
	// read-only: nothing is renewed.
	PromptCacheLookup(ctx context.Context, account Entity, hashes []string, now time.Time) (int, error)
	// PromptCacheCommit creates the entries that are missing or dead and
	// renews the live ones (README 3.14). It returns how many were created.
	PromptCacheCommit(ctx context.Context, account Entity, entries []PromptCacheEntry, now time.Time) (int, error)
}

var (
	_ PromptCacher = (*Memory)(nil)
	_ PromptCacher = (*Valkey)(nil)
	_ PromptCacher = (*Fallback)(nil)
)

// pcEntry is one prefix in the memory registry.
type pcEntry struct {
	exp, ttl, deadline int64 // ms since the epoch; deadline 0 = no cap
}

// pcCommitEntries applies README 3.14 to one account map and returns the
// created count. now and the entries are in ms.
func pcCommitEntries(m map[string]pcEntry, now int64, entries []PromptCacheEntry) int {
	created := 0
	for _, en := range entries {
		ttl, life := en.TTL.Milliseconds(), en.MaxLife.Milliseconds()
		old, ok := m[en.Hash]
		var e pcEntry
		if ok && old.exp > now {
			e.ttl = max(old.ttl, ttl)
			e.deadline = old.deadline
			e.exp = max(old.exp, now+e.ttl)
		} else {
			created++
			e.ttl = ttl
			if life > 0 {
				e.deadline = now + life
			}
			e.exp = now + ttl
		}
		if e.deadline > 0 && e.exp > e.deadline {
			e.exp = e.deadline
		}
		m[en.Hash] = e
	}
	// Prune up to pcPrune expired entries, then the excess by expiry.
	pruned := 0
	for h, e := range m {
		if pruned == pcPrune {
			break
		}
		if e.exp <= now {
			delete(m, h)
			pruned++
		}
	}
	if over := len(m) - PromptCacheMaxEntries; over > 0 {
		type he struct {
			h   string
			exp int64
		}
		all := make([]he, 0, len(m))
		for h, e := range m {
			all = append(all, he{h, e.exp})
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].exp != all[j].exp {
				return all[i].exp < all[j].exp
			}
			return all[i].h < all[j].h
		})
		for _, x := range all[:over] {
			delete(m, x.h)
		}
	}
	return created
}

// pcShards is the memory registry: accounts spread over locked shards.
type pcShards struct {
	seed   maphash.Seed
	shards [64]pcShard
}

type pcShard struct {
	mu   sync.Mutex
	accs map[Entity]map[string]pcEntry
}

func newPCShards() *pcShards { return &pcShards{seed: maphash.MakeSeed()} }

func (p *pcShards) shard(e Entity) *pcShard {
	var h maphash.Hash
	h.SetSeed(p.seed)
	_, _ = h.WriteString(string(e.Kind))
	_ = h.WriteByte(0)
	_, _ = h.WriteString(e.ID)
	return &p.shards[h.Sum64()%uint64(len(p.shards))]
}

func (p *pcShards) lookup(account Entity, hashes []string, now time.Time) int {
	s := p.shard(account)
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.accs[account]
	if m == nil {
		return -1
	}
	n := now.UnixMilli()
	for i := len(hashes) - 1; i >= 0; i-- {
		if e, ok := m[hashes[i]]; ok && e.exp > n {
			return i
		}
	}
	return -1
}

func (p *pcShards) commit(account Entity, entries []PromptCacheEntry, now time.Time) int {
	if len(entries) == 0 {
		return 0
	}
	s := p.shard(account)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accs == nil {
		s.accs = map[Entity]map[string]pcEntry{}
	}
	m := s.accs[account]
	if m == nil {
		m = map[string]pcEntry{}
		s.accs[account] = m
	}
	created := pcCommitEntries(m, now.UnixMilli(), entries)
	if len(m) == 0 {
		delete(s.accs, account)
	}
	return created
}

func (p *pcShards) reset() {
	for i := range p.shards {
		s := &p.shards[i]
		s.mu.Lock()
		s.accs = nil
		s.mu.Unlock()
	}
}

// pcache returns the memory registry, created on first use.
func (m *Memory) pcache() *pcShards {
	m.pcOnce.Do(func() { m.pc = newPCShards() })
	return m.pc
}

// PromptCacheLookup implements PromptCacher.
func (m *Memory) PromptCacheLookup(_ context.Context, account Entity, hashes []string, now time.Time) (int, error) {
	return m.pcache().lookup(account, hashes, now), nil
}

// PromptCacheCommit implements PromptCacher.
func (m *Memory) PromptCacheCommit(_ context.Context, account Entity, entries []PromptCacheEntry, now time.Time) (int, error) {
	return m.pcache().commit(account, entries, now), nil
}

// ---- Valkey ----

func (v *Valkey) pcKeys(account Entity) (pc, pcx string) {
	t := v.tag(account)
	return v.p + ":pc:" + t, v.p + ":pcx:" + t
}

// PromptCacheLookup implements PromptCacher: ZMSCORE on P:pcx:A.
func (v *Valkey) PromptCacheLookup(ctx context.Context, account Entity, hashes []string, now time.Time) (int, error) {
	if len(hashes) == 0 {
		return -1, nil
	}
	_, pcx := v.pcKeys(account)
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	arr, err := v.c.Do(cctx, v.c.B().Arbitrary("ZMSCORE").Keys(pcx).Args(hashes...).Build()).ToArray()
	if err != nil {
		return -1, err
	}
	n := now.UnixMilli()
	for i := len(arr) - 1; i >= 0; i-- {
		if arr[i].IsNil() {
			continue
		}
		if f, ferr := arr[i].AsFloat64(); ferr == nil && int64(f) > n {
			return i, nil
		}
	}
	return -1, nil
}

// PromptCacheCommit implements PromptCacher (script pc_commit, README 3.14).
func (v *Valkey) PromptCacheCommit(ctx context.Context, account Entity, entries []PromptCacheEntry, now time.Time) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	pc, pcx := v.pcKeys(account)
	args := make([]string, 0, 3+3*len(entries))
	args = append(args, ms(now), strconv.Itoa(PromptCacheMaxEntries), strconv.Itoa(len(entries)))
	for _, e := range entries {
		args = append(args, e.Hash, itoa(e.TTL.Milliseconds()), itoa(e.MaxLife.Milliseconds()))
	}
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	res := v.pipe(cctx, scriptCall(luaPCCommit, []string{pc, pcx}, args))
	if err := res[0].Error(); err != nil {
		return 0, err
	}
	n, err := res[0].AsInt64()
	return int(n), err
}

// ---- Fallback ----

// PromptCacheLookup implements PromptCacher. An error counts for the
// circuit like Check and is a miss; with the circuit open, local uses the
// internal memory store and allow/deny miss.
func (f *Fallback) PromptCacheLookup(ctx context.Context, account Entity, hashes []string, now time.Time) (int, error) {
	if v := f.live(); v != nil {
		i, err := v.PromptCacheLookup(ctx, account, hashes, now)
		if err == nil {
			f.success()
			return i, nil
		}
		if callerGone(ctx) {
			return -1, err
		}
		f.failure(err)
		return -1, err
	}
	if f.mode == "local" {
		return f.memory().PromptCacheLookup(ctx, account, hashes, now)
	}
	return -1, nil
}

// PromptCacheCommit implements PromptCacher. A failure is only returned
// (the engine logs it at debug); it never counts for the circuit.
func (f *Fallback) PromptCacheCommit(ctx context.Context, account Entity, entries []PromptCacheEntry, now time.Time) (int, error) {
	if v := f.live(); v != nil {
		return v.PromptCacheCommit(ctx, account, entries, now)
	}
	if f.mode == "local" {
		return f.memory().PromptCacheCommit(ctx, account, entries, now)
	}
	return 0, nil
}
