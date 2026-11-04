package limits

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// Valkey is the cluster-wide Store (spec 012). Key schema, scripts and
// replies follow cache/valkey/README.md; what the README does not fix
// follows memory.go.
type Valkey struct {
	c        valkey.Client
	cfg      config.StoreConfig
	p        string // key prefix
	timeout  time.Duration
	instance string
	log      *slog.Logger
	cache    banCache
	lf       *localFirst
	// perEntity selects check_entity + release (store.cluster) instead of
	// the single check script.
	perEntity bool

	ringMu sync.Mutex
	ring   []Event // events when store.event_stream is false

	pw polWatch // cfg:changed subscription (policies.go)

	started time.Time
	opts    ValkeyOptions
	stop    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
}

// ValkeyOptions are the process-level settings of NewValkey that are not
// part of the YAML configuration.
type ValkeyOptions struct {
	// DB selects a logical database (standalone only). Tests use 15.
	DB int
	// Instance identifies this process in events and heartbeats; empty =
	// POD_NAME or the hostname.
	Instance string
	// Version and Addr are published in the heartbeat (inst:{id}).
	Version string
	Addr    string
	// Heartbeat interval; 0 = 5 s, negative disables it.
	Heartbeat time.Duration
	// SyncInterval of local_first; 0 = 100 ms.
	SyncInterval time.Duration
	// PerEntity forces the cluster evaluation (check_entity + release) on a
	// standalone server, to exercise it without a cluster.
	PerEntity bool
	Logger    *slog.Logger
}

// ErrStoreDown is returned by admin operations while Valkey is unreachable.
var ErrStoreDown = errors.New("limits: valkey store unavailable")

//go:embed lua/*.lua
var luaFS embed.FS

// script is a Lua script (lib.lua + body) run with EVALSHA, falling back to
// EVAL on NOSCRIPT.
type script struct {
	name string
	src  string
	sha  string
}

// mustReadLua reads an embedded script; a missing one is a build bug.
func mustReadLua(name string) []byte {
	b, err := luaFS.ReadFile("lua/" + name + ".lua")
	if err != nil {
		panic(err)
	}
	return b
}

func newScript(name string) *script {
	src := string(mustReadLua("lib")) + "\n" + string(mustReadLua(name))
	sum := sha1Hex(src)
	return &script{name: name, src: src, sha: sum}
}

var (
	luaCheck       = newScript("check")
	luaCheckEntity = newScript("check_entity")
	luaRelease     = newScript("release")
	luaFinish      = newScript("finish")
	luaSetStatus   = newScript("set_status")
	luaStrike      = newScript("strike")
	luaResetCount  = newScript("reset_counters")
	luaSyncLocal   = newScript("sync_local")
	luaPeek        = newScript("peek")
	luaResetEntity = newScript("reset_entity")
	luaSetPolicy   = newScript("set_policy")
	luaSetTier     = newScript("set_tier")
	luaPCCommit    = newScript("pc_commit")
	luaEscalate    = newScript("escalate")
	allScripts     = []*script{luaCheck, luaCheckEntity, luaRelease, luaFinish, luaSetStatus, luaStrike, luaResetCount, luaSyncLocal, luaPeek, luaResetEntity, luaSetPolicy, luaSetTier, luaPCCommit, luaEscalate}
)

// NewValkey connects to store.addr, loads the scripts on every node and
// starts the heartbeat (and the local_first sync when enabled).
func NewValkey(ctx context.Context, cfg config.StoreConfig, o ValkeyOptions) (*Valkey, error) {
	if len(cfg.Addr) == 0 {
		return nil, errors.New("limits: store.addr is empty")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Instance == "" {
		o.Instance = defaultInstance()
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "wwz"
	}
	if cfg.EventMaxLen <= 0 {
		cfg.EventMaxLen = 1_000_000
	}
	if cfg.EventSample <= 0 || cfg.EventSample > 1 {
		cfg.EventSample = 1
	}
	opt := valkey.ClientOption{
		InitAddress:  cfg.Addr,
		Password:     cfg.Password,
		DisableCache: true,
		ClientName:   "wwz:" + o.Instance,
	}
	opt.Dialer.Timeout = 2 * time.Second
	if !cfg.Cluster {
		opt.ForceSingleClient = true
		opt.SelectDB = o.DB
	}
	c, err := valkey.NewClient(opt)
	if err != nil {
		return nil, fmt.Errorf("limits: valkey %s: %w", strings.Join(cfg.Addr, ","), err)
	}
	v := &Valkey{
		c:         c,
		cfg:       cfg,
		p:         cfg.Prefix,
		timeout:   cfg.ReadTimeout,
		instance:  o.Instance,
		log:       o.Logger,
		cache:     banCache{m: map[Entity]*cacheEnt{}, ttl: time.Second},
		started:   time.Now(),
		opts:      o,
		stop:      make(chan struct{}),
		perEntity: cfg.Cluster || o.PerEntity,
	}
	if err := v.loadScripts(ctx); err != nil {
		c.Close()
		return nil, err
	}
	if cfg.LocalFirst {
		v.lf = newLocalFirst(v, o.SyncInterval)
	}
	hb := o.Heartbeat
	if hb == 0 {
		hb = 5 * time.Second
	}
	if hb > 0 {
		v.beat(ctx)
		v.wg.Go(func() { v.loop(hb, v.beat) })
	}
	if v.lf != nil {
		v.wg.Go(func() { v.loop(v.lf.interval, v.lf.sync) })
	}
	return v, nil
}

func defaultInstance() string {
	host, err := os.Hostname()
	return instanceName(os.Getenv("POD_NAME"), host, err)
}

// instanceName picks the pod name, else the hostname, else "wwz".
func instanceName(pod, host string, hostErr error) string {
	if pod != "" {
		return pod
	}
	if hostErr == nil && host != "" {
		return host
	}
	return "wwz"
}

func (v *Valkey) loop(every time.Duration, fn func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-v.stop:
			return
		case <-t.C:
			fn(context.Background())
		}
	}
}

// loadScripts runs SCRIPT LOAD of every script on every node to fail early.
func (v *Valkey) loadScripts(ctx context.Context) error {
	for addr, n := range v.c.Nodes() {
		for _, s := range allScripts {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			sha, err := n.Do(cctx, n.B().Arbitrary("SCRIPT", "LOAD").Args(s.src).Build()).ToString()
			cancel()
			if err != nil {
				return fmt.Errorf("limits: SCRIPT LOAD %s on %s: %w", s.name, addr, err)
			}
			if sha != s.sha {
				return fmt.Errorf("limits: SCRIPT LOAD %s: sha %s, want %s", s.name, sha, s.sha)
			}
		}
	}
	return nil
}

// Client exposes the underlying client (tests, admin tooling).
func (v *Valkey) Client() valkey.Client { return v.c }

// Kind implements Store.
func (v *Valkey) Kind() string { return "valkey" }

// Close stops the background loops and closes the client.
func (v *Valkey) Close() error {
	v.once.Do(func() {
		close(v.stop)
		v.wg.Wait()
		if v.lf != nil {
			v.lf.sync(context.Background())
		}
		v.c.Close()
	})
	return nil
}

func (v *Valkey) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	if v.timeout > 0 {
		return context.WithTimeout(ctx, v.timeout)
	}
	return context.WithCancel(ctx)
}

// ---- keys ----

const maxEscapedID = 200

// escapeID applies the README escape: bytes outside [A-Za-z0-9._~@+=/-]
// become %XX; ids longer than 200 bytes once escaped become ~ + 32 hex of
// sha256.
func escapeID(id string) string {
	clean := true
	for i := 0; i < len(id); i++ {
		if !idSafe(id[i]) {
			clean = false
			break
		}
	}
	out := id
	if !clean {
		var b strings.Builder
		b.Grow(len(id) + 8)
		for i := 0; i < len(id); i++ {
			c := id[i]
			if idSafe(c) {
				b.WriteByte(c)
				continue
			}
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&15])
		}
		out = b.String()
	}
	if len(out) > maxEscapedID {
		sum := sha256.Sum256([]byte(id))
		return "~" + hex.EncodeToString(sum[:])[:32]
	}
	return out
}

func idSafe(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("._~@+=/-", c) >= 0
}

func (v *Valkey) tag(e Entity) string { return "{" + string(e.Kind) + ":" + escapeID(e.ID) + "}" }

func (v *Valkey) key(sub string, e Entity) string { return v.p + ":" + sub + ":" + v.tag(e) }

// ekeys are the 7 entity keys in README order: ent, tb, win, q5h, qwk, ramp, conc.
func (v *Valkey) ekeys(e Entity) []string {
	t := v.tag(e)
	p := v.p + ":"
	return []string{p + "ent:" + t, p + "tb:" + t, p + "win:" + t, p + "q5h:" + t, p + "qwk:" + t, p + "ramp:" + t, p + "conc:" + t}
}

func member(e Entity) string { return string(e.Kind) + ":" + e.ID }

func parseMember(m string) Entity {
	k, id, _ := strings.Cut(m, ":")
	return Entity{Kind: EntityKind(k), ID: id}
}

func (v *Valkey) idxEnt() string            { return v.p + ":idx:ent" }
func (v *Valkey) idxStatus(s Status) string { return v.p + ":idx:status:" + string(s) }
func (v *Valkey) stream() string            { return v.p + ":ev:stream" }

// ---- pipeline helpers ----

// call is one pipeline entry: a script call or a plain command.
type call struct {
	s    *script
	keys []string
	args []string
	cmd  []string // plain command: name, then arguments; key is cmd[1] when key is set
	key  string
}

func scriptCall(s *script, keys, args []string) call { return call{s: s, keys: keys, args: args} }

func cmdCall(key string, cmd ...string) call { return call{key: key, cmd: cmd} }

func (v *Valkey) build(c call, eval bool) valkey.Completed {
	b := v.c.B()
	if c.s != nil {
		if eval {
			return b.Arbitrary("EVAL", c.s.src, strconv.Itoa(len(c.keys))).Keys(c.keys...).Args(c.args...).Build()
		}
		return b.Arbitrary("EVALSHA", c.s.sha, strconv.Itoa(len(c.keys))).Keys(c.keys...).Args(c.args...).Build()
	}
	if c.key != "" && len(c.cmd) > 1 && c.cmd[1] == c.key {
		return b.Arbitrary(c.cmd[0]).Keys(c.key).Args(c.cmd[2:]...).Build()
	}
	return b.Arbitrary(c.cmd[0]).Args(c.cmd[1:]...).Build()
}

// pipe runs calls in one pipeline; script entries answered with NOSCRIPT
// are retried once with EVAL (which also reloads the script cache).
func (v *Valkey) pipe(ctx context.Context, calls ...call) []valkey.ValkeyResult {
	cmds := make(valkey.Commands, len(calls))
	for i, c := range calls {
		cmds[i] = v.build(c, false)
	}
	res := v.c.DoMulti(ctx, cmds...)
	var retry []int
	for i, c := range calls {
		if c.s != nil && i < len(res) {
			if ve, ok := valkey.IsValkeyErr(res[i].Error()); ok && ve.IsNoScript() {
				retry = append(retry, i)
			}
		}
	}
	if len(retry) > 0 {
		cmds = cmds[:0]
		for _, i := range retry {
			cmds = append(cmds, v.build(calls[i], true))
		}
		again := v.c.DoMulti(ctx, cmds...)
		for j, i := range retry {
			res[i] = again[j]
		}
	}
	return res
}

func firstErr(res []valkey.ValkeyResult) error {
	for _, r := range res {
		if err := r.Error(); err != nil && !valkey.IsValkeyNil(err) {
			return err
		}
	}
	return nil
}

func i64(m valkey.ValkeyMessage) int64 {
	n, err := m.AsInt64()
	if err != nil {
		if f, ferr := m.AsFloat64(); ferr == nil {
			return int64(f)
		}
	}
	return n
}

func str(m valkey.ValkeyMessage) string {
	s, _ := m.ToString()
	return s
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func ms(t time.Time) string { return itoa(t.UnixMilli()) }

func micro(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

func msDur(n int64) time.Duration { return time.Duration(n) * time.Millisecond }

// ---- limits vector ----

// limArgs appends the 25 LIM values of s (README 3.1).
func limArgs(dst []string, s Subject, mask int) []string {
	l := s.Limits
	mode := "0"
	switch l.Mode {
	case "after_n":
		mode = "1"
	case "off":
		mode = "2"
	}
	algo := "0"
	if l.Algorithm == "fixed_window" {
		algo = "1"
	}
	qto := int64(0)
	if l.OnLimit == "queue" && l.QueueTimeout > 0 {
		qto = l.QueueTimeout.Milliseconds()
		if qto == 0 {
			qto = 1
		}
	}
	credits := int64(-1)
	if l.CreditsUSD != nil {
		credits = micro(*l.CreditsUSD)
	}
	var qmo int64
	var q5, qw float64
	if q := l.QuotaUSD; q != nil {
		qmo, q5, qw = micro(q.MonthlyUSD), q.Rolling5hPct, q.WeeklyPct
	}
	var rth int
	var rpct float64
	var rwin int64
	if r := l.Ramp; r != nil {
		rth, rpct, rwin = r.ThresholdTPM, r.MaxIncreasePct, r.Window.Milliseconds()
	}
	excl := "0"
	if l.ExcludeCacheRead {
		excl = "1"
	}
	f := func(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) }
	rps, burst := strconv.Itoa(l.RPS), strconv.Itoa(l.Burst)
	if s.Window > 0 && l.RPM > 0 {
		// The short window of the rpm goes in the rps slot (Subject.Window).
		rate, b, _, _ := s.requestRate()
		rps, burst = f(rate), f(b)
	}
	return append(dst,
		string(s.Entity.Kind), s.Entity.ID, mode, strconv.Itoa(l.AfterN), algo,
		rps, burst, strconv.Itoa(l.RPM), strconv.Itoa(l.TPM), strconv.Itoa(l.ITPM),
		strconv.Itoa(l.OTPM), strconv.Itoa(l.RPD), strconv.Itoa(l.TPD), strconv.Itoa(l.Concurrency), itoa(qto),
		itoa(micro(l.SpendMonthlyUSD)), itoa(credits), itoa(qmo), f(q5), f(qw),
		strconv.Itoa(rth), f(rpct), itoa(rwin), excl, strconv.Itoa(mask),
	)
}

func nextMonth(now time.Time) time.Time { return monthStart(now).AddDate(0, 1, 0) }

// ---- check ----

type checkReply struct {
	allowed bool
	idx     int
	limit   string
	retry   int64
	queue   bool
	st      [10]int64
	reason  string
	created int64
	reserve int64
	extra   int64
	taken   [10]int64
	proj    [3]int64 // project tokens: limit, remaining, reset ms (items 30-32)
}

func parseCheck(r valkey.ValkeyResult) (checkReply, error) {
	arr, err := r.ToArray()
	if err != nil {
		return checkReply{}, err
	}
	if len(arr) < 19 {
		return checkReply{}, fmt.Errorf("limits: check reply has %d items", len(arr))
	}
	var c checkReply
	c.allowed = i64(arr[0]) == 1
	c.idx = int(i64(arr[1]))
	c.limit = str(arr[2])
	c.retry = i64(arr[3])
	c.queue = i64(arr[4]) == 1
	for i := range 10 {
		c.st[i] = i64(arr[5+i])
	}
	c.reason = str(arr[15])
	c.created = i64(arr[16])
	c.reserve = i64(arr[17])
	c.extra = i64(arr[18])
	if len(arr) >= 29 {
		for i := range 10 {
			c.taken[i] = i64(arr[19+i])
		}
	}
	if len(arr) >= 32 {
		for i := range 3 {
			c.proj[i] = i64(arr[29+i])
		}
	}
	return c, nil
}

// mergeState folds one reply state into dst: each unit keeps the entity
// with the smallest remaining, the later one on a tie (memory.go mergeUnits).
func mergeState(dst *[10]int64, st [10]int64) {
	take := func(i int) bool { return st[i] > 0 && (dst[i] == 0 || st[i+1] <= dst[i+1]) }
	if take(0) {
		dst[0], dst[1], dst[2] = st[0], st[1], st[2]
	}
	tok := false
	for _, i := range [...]int{3, 6, 8} {
		if take(i) {
			dst[i], dst[i+1], tok = st[i], st[i+1], true
		}
	}
	if tok {
		dst[5] = st[5]
	}
}

// mergeProject keeps the project tokens of a reply that has them (one
// project per request; memory.go mergeUnits).
func mergeProject(dst *[3]int64, p [3]int64) {
	if p[0] > 0 {
		*dst = p
	}
}

func rlState(st [10]int64) core.RateLimitState {
	s := core.RateLimitState{
		RequestsLimit: int(st[0]), RequestsRemaining: int(st[1]), RequestsReset: msDur(st[2]),
		TokensLimit: int(st[3]), TokensRemaining: int(st[4]), TokensReset: msDur(st[5]),
		InputTokensLimit: int(st[6]), InputTokensRemain: int(st[7]),
		OutputTokensLimit: int(st[8]), OutputTokensRem: int(st[9]),
	}
	s.Present = st[0] > 0 || st[3] > 0 || st[6] > 0 || st[8] > 0
	return sumTokens(s)
}

// peekTokens reads the token units (tpm, itpm, otpm) of s at now without
// consuming them and folds them into st (local_first rpm denial, task 064).
// A failure leaves st as it is: the headers are best effort.
func (v *Valkey) peekTokens(ctx context.Context, s Subject, now time.Time, st *core.RateLimitState) {
	l := s.Limits
	if l.TPM <= 0 && l.ITPM <= 0 && l.OTPM <= 0 {
		return
	}
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	algo := "0"
	if l.Algorithm == "fixed_window" {
		algo = "1"
	}
	k := v.ekeys(s.Entity)
	arr, err := v.pipe(cctx, scriptCall(luaPeek, []string{k[1], k[2]}, []string{ms(now), strconv.Itoa(l.TPM), strconv.Itoa(l.ITPM), strconv.Itoa(l.OTPM), algo}))[0].ToArray()
	if err != nil || len(arr) < 10 {
		v.log.Debug("limits: token peek failed", "err", err)
		return
	}
	var p [10]int64
	for i := range 10 {
		p[i] = i64(arr[i])
	}
	// The token units only (the requests unit is not peeked; sumTokens runs
	// on the whole state later).
	t := core.RateLimitState{
		TokensLimit: int(p[3]), TokensRemaining: int(p[4]), TokensReset: msDur(p[5]),
		InputTokensLimit: int(p[6]), InputTokensRemain: int(p[7]),
		OutputTokensLimit: int(p[8]), OutputTokensRem: int(p[9]),
	}
	*st = mergeUnits(*st, projectUnits(s.Entity, t))
	st.Present = true
}

// accState reports whether a denial by limit returns the shared state
// (memory.go returns dec.State for rpm, tpm, itpm and otpm).
func accState(limit string) bool {
	switch limit {
	case "rpm", "tpm", "itpm", "otpm":
		return true
	}
	return false
}

// Check implements Store.
func (v *Valkey) Check(ctx context.Context, req CheckRequest) (Decision, error) {
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	req.Subjects = resolveWindow(req.Subjects, now)
	if d, ok := v.cache.deny(req.Subjects, now); ok {
		return d, nil
	}
	if v.lf != nil {
		return v.lf.check(ctx, req, now)
	}
	return v.checkRemote(ctx, req, now, nil)
}

// checkRemote evaluates req in Valkey. masks, when not nil, carries the
// local_first mask of each subject.
func (v *Valkey) checkRemote(ctx context.Context, req CheckRequest, now time.Time, masks []int) (Decision, error) {
	if len(req.Subjects) == 0 {
		return Decision{Allowed: true}, nil
	}
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	head := []string{ms(now), strconv.Itoa(req.EstPromptTokens), strconv.Itoa(req.EstOutputTokens), strconv.Itoa(req.CachedTokens), ms(monthStart(now)), ms(nextMonth(now))}
	mask := func(i int) int {
		if masks == nil {
			return 0
		}
		return masks[i]
	}
	if !v.perEntity {
		keys := make([]string, 0, 7*len(req.Subjects))
		args := make([]string, 0, 7+25*len(req.Subjects))
		args = append(args, head...)
		args = append(args, strconv.Itoa(len(req.Subjects)))
		for i, s := range req.Subjects {
			keys = append(keys, v.ekeys(s.Entity)...)
			args = limArgs(args, s, mask(i))
		}
		res := v.pipe(cctx, scriptCall(luaCheck, keys, args))
		r, err := parseCheck(res[0])
		if err != nil {
			return Decision{}, err
		}
		v.indexCreated(cctx, req.Subjects, r.created)
		st, proj := r.st, r.proj
		if !r.allowed && !accState(r.limit) {
			proj = [3]int64{}
			if r.limit != "rps" {
				st = [10]int64{}
			}
		}
		return v.decision(r, req, now, st, proj), nil
	}
	calls := make([]call, len(req.Subjects))
	for i, s := range req.Subjects {
		args := make([]string, 0, 6+25)
		args = append(args, head...)
		calls[i] = scriptCall(luaCheckEntity, v.ekeys(s.Entity), limArgs(args, s, mask(i)))
	}
	res := v.pipe(cctx, calls...)
	replies := make([]checkReply, len(res))
	var created int64
	deny := -1
	for i, r := range res {
		cr, err := parseCheck(r)
		if err != nil {
			// The entities that did answer keep their consumption until the
			// buckets refill; report the failure.
			return Decision{}, err
		}
		replies[i] = cr
		if cr.created == 1 {
			created |= 1 << i
		}
		if !cr.allowed && deny < 0 {
			deny = i
		}
	}
	v.indexCreated(cctx, req.Subjects, created)
	var st [10]int64
	var proj [3]int64
	if deny < 0 {
		for _, r := range replies {
			mergeState(&st, r.st)
			mergeProject(&proj, r.proj)
		}
		return v.decision(replies[0], req, now, st, proj), nil
	}
	var rel []call
	for i, r := range replies {
		if !r.allowed {
			continue
		}
		args := make([]string, 0, 11)
		args = append(args, ms(now))
		for _, t := range r.taken {
			args = append(args, itoa(t))
		}
		rel = append(rel, scriptCall(luaRelease, v.ekeys(req.Subjects[i].Entity), args))
	}
	if len(rel) > 0 {
		if err := firstErr(v.pipe(cctx, rel...)); err != nil {
			v.log.Warn("limits: release failed; buckets recover by refill", "err", err)
		}
	}
	d := replies[deny]
	switch {
	case accState(d.limit):
		for i := 0; i <= deny; i++ {
			mergeState(&st, replies[i].st)
			mergeProject(&proj, replies[i].proj)
		}
	case d.limit == "rps":
		st = d.st
	}
	d.idx = deny + 1
	return v.decision(d, req, now, st, proj), nil
}

// indexCreated adds new entities to idx:ent (bit i of mask = subject i).
func (v *Valkey) indexCreated(ctx context.Context, subs []Subject, mask int64) {
	if mask == 0 {
		return
	}
	var calls []call
	for i, s := range subs {
		if mask&(1<<i) != 0 {
			calls = append(calls, cmdCall(v.idxEnt(), "ZADD", v.idxEnt(), "NX", itoa(time.Now().UnixMilli()), member(s.Entity)))
		}
	}
	if err := firstErr(v.pipe(ctx, calls...)); err != nil {
		v.log.Debug("limits: idx:ent update failed", "err", err)
	}
}

// decision turns a script reply into a Decision with memory.go messages;
// proj is the project tokens unit (items 30-32), zero without one.
func (v *Valkey) decision(r checkReply, req CheckRequest, now time.Time, st [10]int64, proj [3]int64) Decision {
	state := rlState(st)
	if proj[0] > 0 {
		state.Present = true
		state.ProjectTokensLimit, state.ProjectTokensRemain, state.ProjectTokensReset = int(proj[0]), int(proj[1]), msDur(proj[2])
	}
	if r.allowed {
		return Decision{Allowed: true, State: state}
	}
	if r.idx < 1 || r.idx > len(req.Subjects) {
		return Decision{Allowed: false, Kind: core.ErrRateLimit, LimitName: r.limit, State: state}
	}
	s := req.Subjects[r.idx-1]
	l := s.Limits
	d := Decision{Allowed: false, Entity: s.Entity, LimitName: r.limit, RetryAfter: msDur(r.retry), State: state}
	switch r.limit {
	case "ban":
		d.Kind, d.Message, d.RetryAfter = core.ErrAccountBanned, r.reason, 0
		v.cache.put(s.Entity, true, r.reason, time.Time{})
	case "block":
		d.Kind, d.Message = core.ErrAccountSuspended, r.reason
		var until time.Time
		if r.retry > 0 {
			until = now.Add(d.RetryAfter)
		}
		v.cache.put(s.Entity, false, r.reason, until)
	case "rps":
		d.Kind, d.Message = core.ErrRateLimit, s.requestRateMessage()
		_, _, d.State.RequestsLimit, _ = s.requestRate()
	case "rpm":
		d.Kind, d.Message = core.ErrRateLimit, fmt.Sprintf("rate limit of %d requests per minute exceeded", l.RPM)
	case "tpm":
		d.Kind = core.ErrRateLimit
		if l.Algorithm == "fixed_window" {
			d.Message = fmt.Sprintf("rate limit of %d tokens per minute exceeded (requested %d)", l.TPM, r.reserve)
		} else {
			d.Message = fmt.Sprintf("rate limit of %d tokens per minute exceeded (requested %d, remaining %d)", l.TPM, r.reserve, r.extra)
		}
	case "itpm":
		d.Kind, d.Message = core.ErrRateLimit, fmt.Sprintf("input tokens per minute limit of %d exceeded", l.ITPM)
	case "otpm":
		d.Kind, d.Message = core.ErrRateLimit, fmt.Sprintf("output tokens per minute limit of %d exceeded", l.OTPM)
	case "rpd":
		d.Kind, d.RetryAfter, d.Message = core.ErrRateLimit, untilNextDay(now), fmt.Sprintf("daily request limit of %d exceeded", l.RPD)
	case "tpd":
		d.Kind, d.RetryAfter, d.Message = core.ErrRateLimit, untilNextDay(now), fmt.Sprintf("daily token limit of %d exceeded", l.TPD)
	case "spend_monthly":
		d.Kind, d.RetryAfter, d.Message = core.ErrSpendLimit, untilNextMonth(now), fmt.Sprintf("monthly spend limit of $%.2f reached", l.SpendMonthlyUSD)
	case "credits":
		d.Kind, d.RetryAfter, d.Message = core.ErrQuota, 0, "insufficient credits"
	case "quota_5h":
		d.Kind, d.Message = core.ErrQuota, "5 hour usage limit exceeded"
	case "quota_weekly":
		d.Kind, d.Message = core.ErrQuota, "weekly usage limit exceeded"
	case "quota_monthly":
		d.Kind, d.RetryAfter, d.Message = core.ErrQuota, untilNextMonth(now), "monthly usage limit exceeded"
	case "ramp":
		d.Kind, d.Message = core.ErrSlowDown, "traffic increased faster than the allowed ramp rate; slow down"
	case "concurrency":
		if r.queue {
			d.Kind, d.Queue, d.RetryAfter, d.Message = core.ErrOverloaded, true, l.QueueTimeout, "too many concurrent requests"
		} else {
			d.Kind, d.Message = core.ErrRateLimit, fmt.Sprintf("concurrency limit of %d exceeded", l.Concurrency)
		}
	default:
		d.Kind = core.ErrRateLimit
	}
	return d
}

// ---- finish and events ----

// finishCall builds the finish script call of one subject, or false when
// nothing has to be written for it.
func (v *Valkey) finishCall(s Subject, req FinishRequest, now time.Time, ms0 []string) (call, bool) {
	l := s.Limits
	give := int64(req.EstOutputTokens - req.ActualOutput)
	if give < 0 {
		give = 0
	}
	tpmMode := "0"
	if l.TPM > 0 {
		tpmMode = "1"
		if l.Algorithm == "fixed_window" {
			tpmMode = "2"
		}
	}
	tpd := "0"
	if l.TPD > 0 {
		tpd = "1"
	}
	conc := "0"
	if l.Concurrency > 0 {
		conc = "1"
	}
	quota := "0"
	if l.QuotaUSD != nil {
		quota = "1"
	}
	free := "0"
	if FreeModel(req.Model) {
		free = "1"
	}
	quotaDebit := quota == "1" && req.quotaDebit() > 0
	if v.lf != nil && conc == "0" && tpmMode == "0" && tpd == "0" && l.OTPM == 0 && req.CostMicroUSD <= 0 && !quotaDebit && free == "0" {
		// local_first: nothing in Valkey depends on this request.
		return call{}, false
	}
	k := v.ekeys(s.Entity)
	keys := []string{k[0], k[1], k[2], k[3], k[4], k[6]}
	args := []string{ms(now), conc, itoa(give), strconv.Itoa(l.OTPM), strconv.Itoa(req.ActualOutput), itoa(req.CostMicroUSD), quota, ms0[0], tpmMode, tpd, string(s.Entity.Kind), s.Entity.ID, itoa(req.quotaArg()), ms0[1], ms0[2], free}
	return scriptCall(luaFinish, keys, args), true
}

// periodStarts are the starts of the UTC month, day and week of now, in
// ms (ARGV 8, 14 and 15 of finish).
func periodStarts(now time.Time) []string {
	return []string{ms(monthStart(now)), ms(dayStart(now)), ms(weekStart(now))}
}

// Finish implements Store.
func (v *Valkey) Finish(ctx context.Context, req FinishRequest) error {
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	ms0 := periodStarts(now)
	calls := make([]call, 0, len(req.Subjects)+1)
	subj := make([]Subject, 0, len(req.Subjects))
	if req.Denial == nil {
		// A denial reserved nothing: only the event below is written.
		for _, s := range req.Subjects {
			if c, ok := v.finishCall(s, req, now, ms0); ok {
				calls = append(calls, c)
				subj = append(subj, s)
			}
		}
	}
	ev := req.event(now)
	if c, ok := v.eventCall(&ev, true); ok {
		calls = append(calls, c)
	}
	if len(calls) == 0 {
		return nil
	}
	res := v.pipe(cctx, calls...)
	var follow []call
	var usage []entUsage
	for i, s := range subj {
		arr, err := res[i].ToArray()
		if err != nil {
			return err
		}
		if len(arr) < 7 {
			continue
		}
		if req.OnUsage != nil && s.Entity.Kind == KindAccount {
			reqs := i64(arr[3])
			if v.lf != nil {
				reqs += v.lf.pendingRequests(s.Entity)
			}
			st := EntityState{Requests: reqs, Since: time.UnixMilli(i64(arr[4])), PurchasedUSD: float64(i64(arr[5])) / 1e6, SpentTotalUSD: float64(i64(arr[6])) / 1e6}
			usage = append(usage, entUsage{s.Entity, st.Usage(now)})
		}
		m := member(s.Entity)
		if i64(arr[0]) == 1 {
			follow = append(follow, cmdCall(v.idxEnt(), "ZADD", v.idxEnt(), "NX", itoa(time.Now().UnixMilli()), m))
		}
		if i64(arr[1]) == 1 && i64(arr[2]) < 0 {
			follow = append(follow, cmdCall(v.idxStatus(StatusLimited), "ZADD", v.idxStatus(StatusLimited), itoa(time.Now().UnixMilli()), m))
		}
	}
	if err := firstErr(res); err != nil {
		return err
	}
	for _, u := range usage {
		req.OnUsage(u.e, u.u)
	}
	if len(follow) > 0 {
		return firstErr(v.pipe(cctx, follow...))
	}
	return nil
}

// eventCall prepares the XADD of ev (or records it in the local ring when
// store.event_stream is off). sampled marks request/error events, subject
// to store.event_sample.
func (v *Valkey) eventCall(ev *Event, sampled bool) (call, bool) {
	ev.Instance = v.instance
	if sampled && v.cfg.EventSample < 1 {
		if mrand.Float64() >= v.cfg.EventSample { //nolint:gosec // sampling, not security
			return call{}, false
		}
		ev.Sample = v.cfg.EventSample
	}
	if !v.cfg.EventStream {
		v.pushRing(*ev)
		return call{}, false
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return call{}, false
	}
	return cmdCall(v.stream(), "XADD", v.stream(), "MAXLEN", "~", itoa(v.cfg.EventMaxLen), "*", "d", string(b)), true
}

func (v *Valkey) pushRing(ev Event) {
	v.ringMu.Lock()
	defer v.ringMu.Unlock()
	if len(v.ring) >= 10000 {
		v.ring = v.ring[len(v.ring)/2:]
	}
	v.ring = append(v.ring, ev)
}

// Events implements Store: XREVRANGE of ev:stream, newest first.
func (v *Valkey) Events(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	if limit > 10000 {
		limit = 10000
	}
	if !v.cfg.EventStream {
		v.ringMu.Lock()
		defer v.ringMu.Unlock()
		n := min(limit, len(v.ring))
		out := make([]Event, 0, n)
		for i := len(v.ring) - 1; i >= len(v.ring)-n; i-- {
			out = append(out, v.ring[i])
		}
		return out, nil
	}
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	xs, err := v.c.Do(cctx, v.c.B().Arbitrary("XREVRANGE").Keys(v.stream()).Args("+", "-", "COUNT", strconv.Itoa(limit)).Build()).AsXRange()
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(xs))
	for _, x := range xs {
		var ev Event
		if err := json.Unmarshal([]byte(x.FieldValues["d"]), &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// adminCtx bounds admin operations: they may scan, so they get at least 5 s.
func (v *Valkey) adminCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, max(v.timeout, 5*time.Second))
}

// ---- status ----

func (v *Valkey) setStatus(ctx context.Context, op string, e Entity, reason, by string, until time.Time) error {
	now := time.Now()
	u := int64(0)
	if !until.IsZero() {
		u = until.UnixMilli()
	}
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	res := v.pipe(cctx, scriptCall(luaSetStatus, []string{v.key("ent", e)}, []string{op, ms(now), reason, by, itoa(u), string(e.Kind), e.ID}))
	arr, err := res[0].ToArray()
	if err != nil {
		return err
	}
	changed := len(arr) > 0 && i64(arr[0]) == 1
	m := member(e)
	score := ms(now)
	calls := []call{cmdCall(v.idxEnt(), "ZADD", v.idxEnt(), score, m)}
	switch op {
	case "block":
		calls = append(calls, cmdCall("", "ZADD", v.idxStatus(StatusSuspended), score, m))
		v.cache.put(e, false, reason, until)
	case "ban":
		calls = append(calls, cmdCall("", "ZADD", v.idxStatus(StatusBanned), score, m))
		v.cache.put(e, true, reason, time.Time{})
	case "unblock":
		calls = append(calls, cmdCall("", "ZREM", v.idxStatus(StatusSuspended), m))
		v.cache.clear(e, false)
	case "unban":
		calls = append(calls, cmdCall("", "ZREM", v.idxStatus(StatusBanned), m))
		v.cache.clear(e, true)
	}
	// Index keys have no hash tag; route them by their own name.
	for i := range calls {
		calls[i].key = calls[i].cmd[1]
	}
	if changed {
		ev := Event{Time: now, Type: op, Entity: &e}
		switch op {
		case "block":
			ev.Fields = map[string]any{"reason": reason, "until": until, "by": by}
		case "ban":
			ev.Fields = map[string]any{"reason": reason, "by": by}
		}
		if c, ok := v.eventCall(&ev, false); ok {
			calls = append(calls, c)
		}
	}
	return firstErr(v.pipe(cctx, calls...))
}

// Block implements Store.
func (v *Valkey) Block(ctx context.Context, e Entity, reason string, d time.Duration, by string) error {
	var until time.Time
	if d > 0 {
		until = time.Now().Add(d)
	}
	return v.setStatus(ctx, "block", e, reason, by, until)
}

// Unblock implements Store.
func (v *Valkey) Unblock(ctx context.Context, e Entity) error {
	return v.setStatus(ctx, "unblock", e, "", "", time.Time{})
}

// Ban implements Store.
func (v *Valkey) Ban(ctx context.Context, e Entity, reason, by string) error {
	return v.setStatus(ctx, "ban", e, reason, by, time.Time{})
}

// Unban implements Store.
func (v *Valkey) Unban(ctx context.Context, e Entity) error {
	return v.setStatus(ctx, "unban", e, "", "", time.Time{})
}

// Strike implements Store.
func (v *Valkey) Strike(ctx context.Context, e Entity, reason string, ttl time.Duration) (int, error) {
	now := time.Now()
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	mem := ms(now) + ":" + hex.EncodeToString(rb[:])
	ttlMS := max(ttl.Milliseconds(), 1)
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	res := v.pipe(cctx, scriptCall(luaStrike, []string{v.key("strikes", e), v.key("ent", e)}, []string{ms(now), itoa(ttlMS), mem, string(e.Kind), e.ID}))
	arr, err := res[0].ToArray()
	if err != nil {
		return 0, err
	}
	if len(arr) < 2 {
		return 0, errors.New("limits: strike reply")
	}
	n := int(i64(arr[0]))
	var calls []call
	if i64(arr[1]) == 1 {
		calls = append(calls, cmdCall(v.idxEnt(), "ZADD", v.idxEnt(), "NX", ms(now), member(e)))
	}
	ev := Event{Time: now, Type: "strike", Entity: &e, Fields: map[string]any{"reason": reason, "strikes": n}}
	if c, ok := v.eventCall(&ev, false); ok {
		calls = append(calls, c)
	}
	if len(calls) > 0 {
		if err := firstErr(v.pipe(cctx, calls...)); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Escalate implements Escalator (script escalate, key esc).
func (v *Valkey) Escalate(ctx context.Context, e Entity, escalated bool, ttl time.Duration) (Escalation, error) {
	flag := "0"
	if escalated {
		flag = "1"
	}
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	res := v.pipe(cctx, scriptCall(luaEscalate, []string{v.key("esc", e)}, []string{flag, itoa(max(ttl.Milliseconds(), 1))}))
	arr, err := res[0].ToArray()
	if err != nil {
		return Escalation{}, err
	}
	if len(arr) < 2 {
		return Escalation{}, errors.New("limits: escalate reply")
	}
	return Escalation{Level: int(i64(arr[0])), Strikes: int(i64(arr[1]))}, nil
}

// ---- reads ----

func fromMS(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}
	}
	return time.UnixMilli(n)
}

func atoi64(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return 0
		}
		return int64(f)
	}
	return n
}

// entState is the panel view of an ent hash at wall time now (memory.go
// entityRec.view): ban > unexpired block > negative balance > active.
func entState(e Entity, h map[string]string, win []string, now time.Time) EntityState {
	st := EntityState{Entity: e, Status: StatusActive}
	if len(h) == 0 {
		return st
	}
	st.Since = fromMS(h["since"])
	st.Strikes = int(atoi64(h["strikes"]))
	st.Requests = atoi64(h["requests"])
	st.Tier = h["tier"]
	// win holds start and count of spend, sday, swk, free and qmo (getWin).
	at := func(i int, start time.Time) int64 {
		if i+1 < len(win) && win[i] != "" && atoi64(win[i]) >= start.UnixMilli() {
			return atoi64(win[i+1])
		}
		return 0
	}
	day := dayStart(now)
	st.SpendUSD = float64(at(0, monthStart(now))) / 1e6
	st.SpendDailyUSD = float64(at(2, day)) / 1e6
	st.SpendWeeklyUSD = float64(at(4, weekStart(now))) / 1e6
	st.FreeRequestsToday = at(6, day)
	st.QuotaMonthlyUSD = float64(at(8, monthStart(now))) / 1e6
	st.PurchasedUSD = float64(atoi64(h["purchased"])) / 1e6
	st.SpentTotalUSD = float64(atoi64(h["spent"])) / 1e6
	if c, ok := h["credits"]; ok {
		f := float64(atoi64(c)) / 1e6
		st.Credits = &f
		if f < 0 {
			st.Status = StatusLimited
		}
	}
	switch {
	case h["ban_since"] != "":
		st.Status, st.Reason, st.By, st.Since, st.Until = StatusBanned, h["ban_reason"], h["ban_by"], fromMS(h["ban_since"]), time.Time{}
	case h["blk_since"] != "" && blockActive(h["blk_until"], now):
		st.Status, st.Reason, st.By, st.Since, st.Until = StatusSuspended, h["blk_reason"], h["blk_by"], fromMS(h["blk_since"]), fromMS(h["blk_until"])
	}
	return st
}

func blockActive(until string, now time.Time) bool {
	u := atoi64(until)
	return u == 0 || now.UnixMilli() < u
}

func (v *Valkey) getCalls(e Entity) []call {
	ent, win := v.key("ent", e), v.key("win", e)
	return []call{cmdCall(ent, "HGETALL", ent), cmdCall(win, append([]string{"HMGET", win}, getWin...)...)}
}

// getWin are the fields of win that Get reads, in pairs (start, count):
// month spend, UTC day spend, UTC week spend, UTC day :free requests and
// month quota debit.
var getWin = []string{"spend:s", "spend:c", "sday:s", "sday:c", "swk:s", "swk:c", "free:s", "free:c", "qmo:s", "qmo:c"}

func parseGet(e Entity, hres, wres valkey.ValkeyResult, now time.Time) (EntityState, map[string]string, error) {
	h, err := hres.AsStrMap()
	if err != nil {
		return EntityState{}, nil, err
	}
	w, err := wres.ToArray()
	if err != nil {
		return EntityState{}, nil, err
	}
	win := make([]string, len(w))
	for i := range w {
		win[i] = str(w[i])
	}
	return entState(e, h, win, now), h, nil
}

// Get implements Store.
func (v *Valkey) Get(ctx context.Context, e Entity) (EntityState, error) {
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	q5h, qwk := v.key("q5h", e), v.key("qwk", e)
	res := v.pipe(cctx, append(v.getCalls(e), cmdCall(q5h, "HGETALL", q5h), cmdCall(qwk, "HGETALL", qwk))...)
	now := time.Now()
	st, _, err := parseGet(e, res[0], res[1], now)
	if err != nil {
		return st, err
	}
	b, err := res[2].AsStrMap()
	w, werr := res[3].AsStrMap()
	if err = cmp.Or(err, werr); err != nil {
		return EntityState{}, err
	}
	st.Quota5hUSD, st.Quota5hSince = quota5h(b, now)
	st.QuotaWeeklyUSD = quotaWeekly(w, now)
	return st, nil
}

// quotaWeekly is the quota debit of the UTC week in the qwk hour buckets,
// as the check script's quota_weekly reads it (the buckets since Monday
// 00:00 UTC). The week is the one of now or, when a bucket is newer (a
// finish with a later clock), the one of the newest bucket: like the
// calendar windows of win, the week never goes back (memory.go calWindow).
func quotaWeekly(b map[string]string, now time.Time) float64 {
	newest := now.UnixMilli()
	for f := range b {
		if s, err := strconv.ParseInt(f, 10, 64); err == nil && s > newest {
			newest = s
		}
	}
	wk := weekStart(time.UnixMilli(newest)).UnixMilli()
	var sum int64
	for f, val := range b {
		if s, err := strconv.ParseInt(f, 10, 64); err == nil && s >= wk {
			sum += atoi64(val)
		}
	}
	return float64(max(sum, 0)) / 1e6
}

// quota5h is the 5 h quota window of the q5h minute buckets at now, as the
// check script's slide reads it: the sum of the live buckets in dollars and
// the start of the oldest one (zero without spend).
func quota5h(b map[string]string, now time.Time) (float64, time.Time) {
	cut := now.Add(-5 * time.Hour).UnixMilli()
	var sum, oldest int64
	for f, val := range b {
		s, err := strconv.ParseInt(f, 10, 64)
		if err != nil || s+time.Minute.Milliseconds() <= cut {
			continue
		}
		sum += atoi64(val)
		if oldest == 0 || s < oldest {
			oldest = s
		}
	}
	if sum <= 0 {
		return 0, time.Time{}
	}
	return float64(sum) / 1e6, time.UnixMilli(oldest)
}

// List implements Store: pages of the index sorted by since (newest first),
// dropping stale index members.
func (v *Valkey) List(ctx context.Context, status Status, limit int) ([]EntityState, error) {
	if limit <= 0 {
		limit = 1000
	}
	limit = min(limit, 10000)
	idx := v.idxEnt()
	if status != "" && status != StatusActive {
		idx = v.idxStatus(status)
	}
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	now := time.Now()
	var out []EntityState
	for page := range 10 {
		start := int64(page * limit)
		members, err := v.c.Do(cctx, v.c.B().Arbitrary("ZREVRANGE").Keys(idx).Args(itoa(start), itoa(start+int64(limit)-1)).Build()).AsStrSlice()
		if err != nil {
			return nil, err
		}
		if len(members) == 0 {
			break
		}
		calls := make([]call, 0, 2*len(members))
		for _, m := range members {
			calls = append(calls, v.getCalls(parseMember(m))...)
		}
		res := v.pipe(cctx, calls...)
		var stale []string
		for i, m := range members {
			e := parseMember(m)
			st, h, err := parseGet(e, res[2*i], res[2*i+1], now)
			if err != nil {
				return nil, err
			}
			if len(h) == 0 {
				stale = append(stale, m)
				continue
			}
			if !markHeld(status, h, now) {
				stale = append(stale, m)
			}
			if status == "" || st.Status == status {
				if len(out) < limit {
					out = append(out, st)
				}
			}
		}
		if len(stale) > 0 {
			_ = v.c.Do(cctx, v.c.B().Arbitrary("ZREM").Keys(idx).Args(stale...).Build()).Error()
		}
		if len(out) >= limit || len(members) < limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	return out, nil
}

// markHeld reports whether h still holds the mark its status index is for.
func markHeld(status Status, h map[string]string, now time.Time) bool {
	switch status {
	case StatusSuspended:
		return h["blk_since"] != "" && blockActive(h["blk_until"], now)
	case StatusBanned:
		return h["ban_since"] != ""
	case StatusLimited:
		c, ok := h["credits"]
		return ok && atoi64(c) < 0
	}
	return true
}

// Attempts implements Store (MULTI/HINCRBY/PEXPIRE/EXEC, TTL per conversation).
func (v *Valkey) Attempts(ctx context.Context, conversation, step string, ttl time.Duration) (int, error) {
	k := v.p + ":att:{conv:" + escapeID(conversation) + "}"
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	b := v.c.B()
	res := v.c.DoMulti(cctx,
		b.Arbitrary("MULTI").Build(),
		b.Arbitrary("HINCRBY").Keys(k).Args(step, "1").Build(),
		b.Arbitrary("PEXPIRE").Keys(k).Args(itoa(max(ttl.Milliseconds(), 1))).Build(),
		b.Arbitrary("EXEC").Build(),
	)
	arr, err := res[3].ToArray()
	if err != nil {
		return 0, err
	}
	if len(arr) < 1 {
		return 0, errors.New("limits: attempts reply")
	}
	return int(i64(arr[0])), nil
}

// SetCredits implements Store.
func (v *Valkey) SetCredits(ctx context.Context, e Entity, usd float64) error {
	ent := v.key("ent", e)
	now := ms(time.Now())
	m := member(e)
	lim := v.idxStatus(StatusLimited)
	idxCall := cmdCall(lim, "ZREM", lim, m)
	if usd < 0 {
		idxCall = cmdCall(lim, "ZADD", lim, now, m)
	}
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	return firstErr(v.pipe(cctx,
		cmdCall(ent, "HSET", ent, "credits", itoa(micro(usd))),
		cmdCall(ent, "HSETNX", ent, "kind", string(e.Kind)),
		cmdCall(ent, "HSETNX", ent, "id", e.ID),
		cmdCall(ent, "HSETNX", ent, "since", now),
		cmdCall(ent, "HSETNX", ent, "status", string(StatusActive)),
		cmdCall(ent, "PERSIST", ent),
		cmdCall(v.idxEnt(), "ZADD", v.idxEnt(), "NX", now, m),
		idxCall,
	))
}

// AddCredits implements CreditAdder (README 4, ADR 0051): HINCRBY of
// credits and purchased plus the creation fields, in one pipeline with the
// read of the result; then the limited index by the resulting balance.
func (v *Valkey) AddCredits(ctx context.Context, e Entity, usd float64) (EntityState, error) {
	if err := checkPurchase(usd); err != nil {
		return EntityState{}, err
	}
	ent := v.key("ent", e)
	nowT := time.Now()
	now := ms(nowT)
	add := itoa(micro(usd))
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	calls := []call{
		cmdCall(ent, "HINCRBY", ent, "credits", add),
		cmdCall(ent, "HINCRBY", ent, "purchased", add),
		cmdCall(ent, "HSETNX", ent, "kind", string(e.Kind)),
		cmdCall(ent, "HSETNX", ent, "id", e.ID),
		cmdCall(ent, "HSETNX", ent, "since", now),
		cmdCall(ent, "HSETNX", ent, "status", string(StatusActive)),
		cmdCall(ent, "PERSIST", ent),
	}
	n := len(calls)
	res := v.pipe(cctx, append(calls, v.getCalls(e)...)...)
	if err := firstErr(res[:n]); err != nil {
		return EntityState{}, err
	}
	st, _, err := parseGet(e, res[n], res[n+1], nowT)
	if err != nil {
		return EntityState{}, err
	}
	m := member(e)
	lim := v.idxStatus(StatusLimited)
	idxCall := cmdCall(lim, "ZREM", lim, m)
	if st.Credits != nil && *st.Credits < 0 {
		idxCall = cmdCall(lim, "ZADD", lim, now, m)
	}
	return st, firstErr(v.pipe(cctx, cmdCall(v.idxEnt(), "ZADD", v.idxEnt(), "NX", now, m), idxCall))
}

// scan returns every key matching pattern on every node.
func (v *Valkey) scan(ctx context.Context, pattern string) ([]string, error) {
	var keys []string
	seen := map[string]bool{}
	for _, n := range v.c.Nodes() {
		cursor := uint64(0)
		for {
			e, err := n.Do(ctx, n.B().Arbitrary("SCAN").Args(strconv.FormatUint(cursor, 10), "MATCH", pattern, "COUNT", "1000").Build()).AsScanEntry()
			if err != nil {
				return nil, err
			}
			for _, k := range e.Elements {
				if !seen[k] {
					seen[k] = true
					keys = append(keys, k)
				}
			}
			cursor = e.Cursor
			if cursor == 0 {
				break
			}
		}
	}
	return keys, nil
}

func (v *Valkey) unlink(ctx context.Context, keys []string) error {
	for len(keys) > 0 {
		n := min(len(keys), 500)
		calls := make([]call, n)
		for i, k := range keys[:n] {
			calls[i] = cmdCall(k, "UNLINK", k)
		}
		if err := firstErr(v.pipe(ctx, calls...)); err != nil {
			return err
		}
		keys = keys[n:]
	}
	return nil
}

// Reset implements Store: deletes every key of the prefix except policies
// and instance heartbeats.
func (v *Valkey) Reset(ctx context.Context) error {
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	keys, err := v.scan(cctx, v.p+":*")
	if err != nil {
		return err
	}
	keep := func(k string) bool {
		r := strings.TrimPrefix(k, v.p+":")
		return r == "cfg:policies" || r == "idx:inst" || strings.HasPrefix(r, "inst:")
	}
	del := keys[:0]
	for _, k := range keys {
		if !keep(k) {
			del = append(del, k)
		}
	}
	if err := v.unlink(cctx, del); err != nil {
		return err
	}
	v.cache.reset()
	v.ringMu.Lock()
	v.ring = nil
	v.ringMu.Unlock()
	if v.lf != nil {
		v.lf.reset()
	}
	return nil
}

// ResetCounters implements CounterResetter.
func (v *Valkey) ResetCounters(ctx context.Context) error {
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	cursor := uint64(0)
	for {
		e, err := v.c.Do(cctx, v.c.B().Arbitrary("ZSCAN").Keys(v.idxEnt()).Args(strconv.FormatUint(cursor, 10), "COUNT", "500").Build()).AsScanEntry()
		if err != nil {
			return err
		}
		var calls []call
		for i := 0; i+1 < len(e.Elements); i += 2 {
			k := v.ekeys(parseMember(e.Elements[i]))
			calls = append(calls, scriptCall(luaResetCount, k[:6], nil))
		}
		if len(calls) > 0 {
			if err := firstErr(v.pipe(cctx, calls...)); err != nil {
				return err
			}
		}
		cursor = e.Cursor
		if cursor == 0 {
			break
		}
	}
	if v.lf != nil {
		v.lf.reset()
	}
	return nil
}

// ResetConversations implements ConversationResetter.
func (v *Valkey) ResetConversations(ctx context.Context, conversation string) (int, error) {
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	if conversation != "" {
		k := v.p + ":att:{conv:" + escapeID(conversation) + "}"
		n, err := v.c.Do(cctx, v.c.B().Arbitrary("UNLINK").Keys(k).Build()).AsInt64()
		return int(n), err
	}
	keys, err := v.scan(cctx, v.p+":att:*")
	if err != nil {
		return 0, err
	}
	return len(keys), v.unlink(cctx, keys)
}

// Ping checks that Valkey answers within the read timeout. When it does,
// it then touches the client's pipelined connections (commands with a key
// pick one at random) so connections broken during an outage reconnect
// here and not on the next data plane call.
func (v *Valkey) Ping(ctx context.Context) error {
	do := func(cmd ...string) error {
		cctx, cancel := v.ctx(ctx)
		defer cancel()
		b := v.c.B().Arbitrary(cmd[0])
		if len(cmd) > 1 {
			b = b.Keys(cmd[1])
		}
		return v.c.Do(cctx, b.Build()).Error()
	}
	if err := do("PING"); err != nil {
		return err
	}
	probe := v.p + ":probe"
	for range 48 {
		_ = do("EXISTS", probe)
	}
	return do("EXISTS", probe)
}

// ---- heartbeat ----

// beat publishes inst:{id} and prunes idx:inst (README 6).
func (v *Valkey) beat(ctx context.Context) {
	now := time.Now()
	k := v.p + ":inst:{" + escapeID(v.instance) + "}"
	idx := v.p + ":idx:inst"
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	res := v.pipe(cctx,
		cmdCall(k, "HSET", k, "id", v.instance, "version", v.opts.Version, "addr", v.opts.Addr, "started", ms(v.started), "beat", ms(now), "rps", "0", "conns", "0", "inflight", "0", "store", "ok"),
		cmdCall(k, "PEXPIRE", k, "15000"),
		cmdCall(idx, "ZADD", idx, ms(now), v.instance),
		cmdCall(idx, "ZREMRANGEBYSCORE", idx, "-inf", itoa(now.UnixMilli()-15000)),
		cmdCall(idx, "ZCOUNT", idx, itoa(now.UnixMilli()-15000), "+inf"),
	)
	if err := firstErr(res); err != nil {
		v.log.Debug("limits: heartbeat failed", "err", err)
		return
	}
	if v.lf != nil {
		if n, err := res[4].AsInt64(); err == nil {
			v.lf.setInstances(int(n))
		}
	}
}

// ---- ban/block cache ----

type markInfo struct {
	reason string
	until  time.Time // zero = no expiry
}

type cacheEnt struct {
	ban, block *markInfo
	exp        time.Time // wall clock
}

// banCache remembers for ttl the entities this instance knows to be banned
// or blocked, so their denials do not touch Valkey (README 6).
type banCache struct {
	mu  sync.RWMutex
	m   map[Entity]*cacheEnt
	ttl time.Duration
}

func (c *banCache) put(e Entity, ban bool, reason string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ce := c.m[e]
	if ce == nil {
		ce = &cacheEnt{}
		c.m[e] = ce
	}
	mi := &markInfo{reason: reason, until: until}
	if ban {
		ce.ban = mi
	} else {
		ce.block = mi
	}
	ce.exp = time.Now().Add(c.ttl)
}

func (c *banCache) clear(e Entity, ban bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ce := c.m[e]
	if ce == nil {
		return
	}
	if ban {
		ce.ban = nil
	} else {
		ce.block = nil
	}
	if ce.ban == nil && ce.block == nil {
		delete(c.m, e)
	}
}

func (c *banCache) reset() {
	c.mu.Lock()
	c.m = map[Entity]*cacheEnt{}
	c.mu.Unlock()
}

// deny answers from the cache when a subject is known banned or blocked.
func (c *banCache) deny(subs []Subject, now time.Time) (Decision, bool) {
	c.mu.RLock()
	empty := len(c.m) == 0
	c.mu.RUnlock()
	if empty {
		return Decision{}, false
	}
	wall := time.Now()
	for _, s := range subs {
		c.mu.RLock()
		ce := c.m[s.Entity]
		var ban, block *markInfo
		if ce != nil && !wall.After(ce.exp) {
			ban, block = ce.ban, ce.block
		}
		c.mu.RUnlock()
		if ban != nil {
			return Decision{Allowed: false, Kind: core.ErrAccountBanned, Entity: s.Entity, LimitName: "ban", Message: ban.reason}, true
		}
		if block != nil && (block.until.IsZero() || now.Before(block.until)) {
			ra := time.Duration(0)
			if !block.until.IsZero() {
				ra = block.until.Sub(now)
			}
			return Decision{Allowed: false, Kind: core.ErrAccountSuspended, Entity: s.Entity, LimitName: "block", RetryAfter: ra, Message: block.reason}, true
		}
	}
	return Decision{}, false
}
