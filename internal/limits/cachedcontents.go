package limits

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Gemini cachedContents (spec 006 16.5, spec 007; task 265): the explicit
// context caches the gemini driver creates, kept in the store so every wwz
// instance behind a balancer sees the same entries. The keys follow the
// proposal of tmp/spec-batch/265.md (cache/valkey/README.md 2.6, pending
// the guardian):
//
//	P:gcc:{gcc:<owner>}         zset  name -> create time (µs); the listing
//	P:gcc:{gcc:<owner>}:<name>  hash  model, disp, toks, ct, ut, exp, body
//
// Both carry the owner's hash tag, so one MULTI touches one slot. The hash
// expires at exp (PEXPIREAT); the zset expires with the latest entry and
// loses members of expired entries when listed.

// CachedContent is one cachedContents entry. Body is opaque to the store:
// the driver's encoding of systemInstruction, contents and tools.
type CachedContent struct {
	// Owner is who sees the entry (the driver's scope: project, account or
	// key); Name is "cachedContents/<id>".
	Owner, Name string
	// Model is "models/<id>"; Display is displayName.
	Model, Display string
	// Tokens is usageMetadata.totalTokenCount.
	Tokens int
	// Create and Update keep µs; Expire keeps ns (the real's precision).
	Create, Update, Expire time.Time
	Body                   []byte
}

// CachedContentStore is implemented by stores that keep cachedContents
// (optional, like ResponseRegistry). A get or list at now never returns an
// entry whose Expire is not after now.
type CachedContentStore interface {
	// CachedContentPut creates or replaces the entry Owner/Name.
	CachedContentPut(ctx context.Context, c CachedContent) error
	// CachedContentGet returns the live entry or nil.
	CachedContentGet(ctx context.Context, owner, name string, now time.Time) (*CachedContent, error)
	// CachedContentList returns the live entries of owner, oldest first.
	CachedContentList(ctx context.Context, owner string, now time.Time) ([]CachedContent, error)
	// CachedContentDelete removes the entry; a missing one is not an error.
	CachedContentDelete(ctx context.Context, owner, name string) error
}

var (
	_ CachedContentStore = (*Memory)(nil)
	_ CachedContentStore = (*Valkey)(nil)
	_ CachedContentStore = (*Fallback)(nil)
)

// sharedCC is the store the gemini driver reaches: the engine shares its
// store (ShareCachedContents) and, without one, a process-local memory.
var (
	sharedCC atomic.Pointer[ccHolder]
	localCC  = NewMemory()
)

type ccHolder struct{ s CachedContentStore }

// ShareCachedContents makes s the cachedContents store of the process when
// it implements CachedContentStore (the engine calls it with its store).
func ShareCachedContents(s Store) {
	if cc, ok := s.(CachedContentStore); ok {
		sharedCC.Store(&ccHolder{s: cc})
	}
}

// SharedCachedContents is the store ShareCachedContents set last, or a
// process-local memory store.
func SharedCachedContents() CachedContentStore {
	if h := sharedCC.Load(); h != nil {
		return h.s
	}
	return localCC
}

// ---- Memory ----

type ccMem struct {
	mu sync.Mutex
	m  map[string]map[string]CachedContent // owner -> name -> entry
}

func (m *Memory) cachedContents() *ccMem {
	m.ccOnce.Do(func() { m.cc = &ccMem{m: map[string]map[string]CachedContent{}} })
	return m.cc
}

func (c *ccMem) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = map[string]map[string]CachedContent{}
}

// CachedContentPut implements CachedContentStore.
func (m *Memory) CachedContentPut(_ context.Context, c CachedContent) error {
	s := m.cachedContents()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[c.Owner] == nil {
		s.m[c.Owner] = map[string]CachedContent{}
	}
	c.Body = append([]byte(nil), c.Body...)
	s.m[c.Owner][c.Name] = c
	return nil
}

// CachedContentGet implements CachedContentStore; an expired entry is
// dropped.
func (m *Memory) CachedContentGet(_ context.Context, owner, name string, now time.Time) (*CachedContent, error) {
	s := m.cachedContents()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[owner][name]
	if !ok {
		return nil, nil
	}
	if !now.Before(c.Expire) {
		delete(s.m[owner], name)
		return nil, nil
	}
	return &c, nil
}

// CachedContentList implements CachedContentStore.
func (m *Memory) CachedContentList(_ context.Context, owner string, now time.Time) ([]CachedContent, error) {
	s := m.cachedContents()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CachedContent
	for name, c := range s.m[owner] {
		if !now.Before(c.Expire) {
			delete(s.m[owner], name)
			continue
		}
		out = append(out, c)
	}
	sortCC(out)
	return out, nil
}

// CachedContentDelete implements CachedContentStore.
func (m *Memory) CachedContentDelete(_ context.Context, owner, name string) error {
	s := m.cachedContents()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m[owner], name)
	return nil
}

// sortCC orders entries by creation, then name.
func sortCC(l []CachedContent) {
	sort.Slice(l, func(i, j int) bool {
		if !l[i].Create.Equal(l[j].Create) {
			return l[i].Create.Before(l[j].Create)
		}
		return l[i].Name < l[j].Name
	})
}

// ---- Valkey ----

func (v *Valkey) ccIndex(owner string) string { return v.p + ":gcc:{gcc:" + escapeID(owner) + "}" }

func (v *Valkey) ccKey(owner, name string) string { return v.ccIndex(owner) + ":" + escapeID(name) }

// ccExpireMs is the PEXPIREAT of an entry: its expiry rounded up to the ms.
func ccExpireMs(t time.Time) int64 {
	ms := t.UnixMilli()
	if t.UnixNano()%int64(time.Millisecond) != 0 {
		ms++
	}
	return ms
}

// CachedContentPut implements CachedContentStore: MULTI / HSET / PEXPIREAT
// / ZADD / PEXPIREAT NX / PEXPIREAT GT / EXEC.
func (v *Valkey) CachedContentPut(ctx context.Context, c CachedContent) error {
	k, idx := v.ccKey(c.Owner, c.Name), v.ccIndex(c.Owner)
	exp := ccExpireMs(c.Expire)
	b := v.c.B()
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	res := v.c.DoMulti(cctx,
		b.Multi().Build(),
		b.Hset().Key(k).FieldValue().
			FieldValue("model", c.Model).
			FieldValue("disp", c.Display).
			FieldValue("toks", strconv.Itoa(c.Tokens)).
			FieldValue("ct", itoa(c.Create.UnixMicro())).
			FieldValue("ut", itoa(c.Update.UnixMicro())).
			FieldValue("exp", itoa(c.Expire.UnixNano())).
			FieldValue("body", string(c.Body)).Build(),
		b.Pexpireat().Key(k).MillisecondsTimestamp(exp).Build(),
		b.Zadd().Key(idx).ScoreMember().ScoreMember(float64(c.Create.UnixMicro()), c.Name).Build(),
		b.Pexpireat().Key(idx).MillisecondsTimestamp(exp).Nx().Build(),
		b.Pexpireat().Key(idx).MillisecondsTimestamp(exp).Gt().Build(),
		b.Exec().Build(),
	)
	return firstErr(res)
}

// ccDecode reads a hash; a malformed or empty one is nil.
func ccDecode(owner, name string, h map[string]string) *CachedContent {
	if len(h) == 0 {
		return nil
	}
	toks, err1 := strconv.Atoi(h["toks"])
	ct, err2 := strconv.ParseInt(h["ct"], 10, 64)
	ut, err3 := strconv.ParseInt(h["ut"], 10, 64)
	exp, err4 := strconv.ParseInt(h["exp"], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return nil
	}
	return &CachedContent{
		Owner: owner, Name: name, Model: h["model"], Display: h["disp"], Tokens: toks,
		Create: time.UnixMicro(ct), Update: time.UnixMicro(ut), Expire: time.Unix(0, exp),
		Body: []byte(h["body"]),
	}
}

// CachedContentGet implements CachedContentStore: HGETALL.
func (v *Valkey) CachedContentGet(ctx context.Context, owner, name string, now time.Time) (*CachedContent, error) {
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	h, err := v.c.Do(cctx, v.c.B().Hgetall().Key(v.ccKey(owner, name)).Build()).AsStrMap()
	if err != nil {
		return nil, err
	}
	c := ccDecode(owner, name, h)
	if c == nil || !now.Before(c.Expire) {
		return nil, nil
	}
	return c, nil
}

// CachedContentList implements CachedContentStore: ZRANGE, then HGETALL of
// each member in one round trip; members whose hash is gone are removed.
func (v *Valkey) CachedContentList(ctx context.Context, owner string, now time.Time) ([]CachedContent, error) {
	idx := v.ccIndex(owner)
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	names, err := v.c.Do(cctx, v.c.B().Zrange().Key(idx).Min("0").Max("-1").Build()).AsStrSlice()
	if err != nil || len(names) == 0 {
		return nil, err
	}
	b := v.c.B()
	cmds := make([]valkey.Completed, len(names))
	for i, n := range names {
		cmds[i] = b.Hgetall().Key(v.ccKey(owner, n)).Build()
	}
	res := v.c.DoMulti(cctx, cmds...)
	var out []CachedContent
	var gone []string
	for i, r := range res {
		h, err := r.AsStrMap()
		if err != nil {
			return nil, err
		}
		c := ccDecode(owner, names[i], h)
		switch {
		case c == nil:
			gone = append(gone, names[i])
		case now.Before(c.Expire):
			out = append(out, *c)
		}
	}
	if len(gone) > 0 {
		_ = v.c.Do(cctx, b.Zrem().Key(idx).Member(gone...).Build()).Error()
	}
	sortCC(out)
	return out, nil
}

// CachedContentDelete implements CachedContentStore: MULTI / DEL / ZREM /
// EXEC.
func (v *Valkey) CachedContentDelete(ctx context.Context, owner, name string) error {
	b := v.c.B()
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	res := v.c.DoMulti(cctx,
		b.Multi().Build(),
		b.Del().Key(v.ccKey(owner, name)).Build(),
		b.Zrem().Key(v.ccIndex(owner)).Member(name).Build(),
		b.Exec().Build(),
	)
	return firstErr(res)
}

// ---- Fallback ----

// CachedContentPut implements CachedContentStore. A write failure is only
// returned (the driver answers 500); it never counts for the circuit, like
// ResponsePut. With the circuit open, local writes to the internal memory
// store and allow/deny write nothing.
func (f *Fallback) CachedContentPut(ctx context.Context, c CachedContent) error {
	if v := f.live(); v != nil {
		return v.CachedContentPut(ctx, c)
	}
	if f.mode == "local" {
		return f.memory().CachedContentPut(ctx, c)
	}
	return nil
}

// CachedContentGet implements CachedContentStore. An error counts for the
// circuit like Check (unless the caller went away); with the circuit open,
// local reads the internal memory store and allow/deny find nothing.
func (f *Fallback) CachedContentGet(ctx context.Context, owner, name string, now time.Time) (*CachedContent, error) {
	if v := f.live(); v != nil {
		c, err := v.CachedContentGet(ctx, owner, name, now)
		return c, f.ccResult(ctx, err)
	}
	if f.mode == "local" {
		return f.memory().CachedContentGet(ctx, owner, name, now)
	}
	return nil, nil
}

// CachedContentList implements CachedContentStore.
func (f *Fallback) CachedContentList(ctx context.Context, owner string, now time.Time) ([]CachedContent, error) {
	if v := f.live(); v != nil {
		l, err := v.CachedContentList(ctx, owner, now)
		return l, f.ccResult(ctx, err)
	}
	if f.mode == "local" {
		return f.memory().CachedContentList(ctx, owner, now)
	}
	return nil, nil
}

// CachedContentDelete implements CachedContentStore (a write, like Put).
func (f *Fallback) CachedContentDelete(ctx context.Context, owner, name string) error {
	if v := f.live(); v != nil {
		return v.CachedContentDelete(ctx, owner, name)
	}
	if f.mode == "local" {
		return f.memory().CachedContentDelete(ctx, owner, name)
	}
	return nil
}

// ccResult counts a Valkey read for the circuit, like Check: a success
// closes it, an error (unless the caller went away) counts as a failure.
func (f *Fallback) ccResult(ctx context.Context, err error) error {
	if err == nil {
		f.success()
	} else if !callerGone(ctx) {
		f.failure(err)
	}
	return err
}
