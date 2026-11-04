package source

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	panelcfg "github.com/patrickbrandao/white-wizard/internal/panel/config"
)

// Errors of the Valkey source.
var (
	// ErrValkeyOff means the panel runs without Valkey (store: memory).
	ErrValkeyOff = errors.New("source: painel sem Valkey")
	// ErrPolicyConflict means cfg:policies changed since the version the
	// caller read (PUT /api/v1/policies → 409).
	ErrPolicyConflict = errors.New("source: cfg:policies mudou desde a versão lida")
	// ErrInvalidPolicy means a tier name or body is invalid (→ 400).
	ErrInvalidPolicy = errors.New("source: política inválida")
)

const (
	minuteMS = int64(60_000)
	hourMS   = 60 * minuteMS
	dayMS    = 24 * hourMS
	// instAlive is how old a heartbeat may be (README §2.2, 15 s).
	instAlive = 15 * time.Second
)

// ValkeyOptions configures NewValkey.
type ValkeyOptions struct {
	// DB selects a logical database (standalone only; tests).
	DB int
	// ClientName is sent with CLIENT SETNAME; "" = "wwz-panel:<hostname>".
	ClientName string
	// Timeout bounds every call; 0 = 2 s.
	Timeout time.Duration
	// Now replaces time.Now (tests).
	Now func() time.Time
	// Client reuses an existing client instead of dialing (tests); Close
	// does not close it.
	Client valkey.Client
}

// Valkey reads the cluster state of cache/valkey/README.md. It connects
// lazily: a Valkey that is down when the panel boots is retried on the next
// call (at most once per second) instead of failing the boot. The only
// write is ReplaceTiers (cfg:policies + PUBLISH cfg:changed, ADR 0025).
type Valkey struct {
	p       string
	opt     valkey.ClientOption
	timeout time.Duration
	now     func() time.Time
	dial    func(valkey.ClientOption) (valkey.Client, error)
	owned   bool

	mu       sync.Mutex
	c        valkey.Client
	lastDial time.Time
	dialErr  error
}

// NewValkey prepares the source for cfg (it does not dial). cfg without
// addresses returns ErrValkeyOff.
func NewValkey(cfg panelcfg.Valkey, o ValkeyOptions) (*Valkey, error) {
	if !cfg.Enabled() && o.Client == nil {
		return nil, ErrValkeyOff
	}
	if o.ClientName == "" {
		o.ClientName = "wwz-panel:" + hostname()
	}
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = "wwz"
	}
	opt := valkey.ClientOption{
		InitAddress:  cfg.Addr,
		Password:     cfg.Password,
		DisableCache: true,
		ClientName:   o.ClientName,
	}
	opt.Dialer.Timeout = o.Timeout
	if !cfg.Cluster {
		opt.ForceSingleClient = true
		opt.SelectDB = o.DB
	}
	return &Valkey{
		p: prefix, opt: opt, timeout: o.Timeout, now: o.Now,
		dial: valkey.NewClient, c: o.Client, owned: o.Client == nil,
	}, nil
}

func hostname() string {
	h, _ := os.Hostname()
	return cmp.Or(h, "wwz-panel")
}

// client returns the connected client, dialing if needed.
func (v *Valkey) client() (valkey.Client, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.c != nil {
		return v.c, nil
	}
	if !v.lastDial.IsZero() && time.Since(v.lastDial) < time.Second {
		return nil, v.dialErr
	}
	v.lastDial = time.Now()
	c, err := v.dial(v.opt)
	if err != nil {
		v.dialErr = fmt.Errorf("source: valkey %s: %w", strings.Join(v.opt.InitAddress, ","), err)
		return nil, v.dialErr
	}
	v.c = c
	return c, nil
}

// Prefix is store.prefix (key prefix P).
func (v *Valkey) Prefix() string { return v.p }

// Close closes the client it dialed.
func (v *Valkey) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.c != nil && v.owned {
		v.c.Close()
	}
	v.c = nil
}

func (v *Valkey) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, v.timeout)
}

// pipe sends cmds (each {cmd, key, args...}) in one round trip.
func (v *Valkey) pipe(ctx context.Context, cmds ...[]string) ([]valkey.ValkeyResult, error) {
	c, err := v.client()
	if err != nil {
		return nil, err
	}
	return v.pipeC(ctx, c, cmds...), nil
}

// pipeC is pipe on an already connected client.
func (v *Valkey) pipeC(ctx context.Context, c valkey.Client, cmds ...[]string) []valkey.ValkeyResult {
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	built := make(valkey.Commands, len(cmds))
	for i, cmd := range cmds {
		built[i] = c.B().Arbitrary(cmd[0]).Keys(cmd[1]).Args(cmd[2:]...).Build()
	}
	return c.DoMulti(cctx, built...)
}

// Ping checks the connection (state of /health/ready).
func (v *Valkey) Ping(ctx context.Context) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	return c.Do(cctx, c.B().Ping().Build()).Error()
}

// ---- keys ----

// escapeID is the README §1 escape (same as internal/limits).
func escapeID(id string) string {
	var b strings.Builder
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
	if b.Len() > 200 {
		sum := sha256.Sum256([]byte(id))
		return "~" + hex.EncodeToString(sum[:])[:32]
	}
	return b.String()
}

func idSafe(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("._~@+=/-", c) >= 0
}

func (v *Valkey) key(sub string, e limits.Entity) string {
	return v.p + ":" + sub + ":{" + string(e.Kind) + ":" + escapeID(e.ID) + "}"
}

func (v *Valkey) policiesKey() string { return v.p + ":cfg:policies" }

// StreamKey is P:ev:stream.
func (v *Valkey) StreamKey() string { return v.p + ":ev:stream" }

func parseMember(m string) limits.Entity {
	k, id, _ := strings.Cut(m, ":")
	return limits.Entity{Kind: limits.EntityKind(k), ID: id}
}

// ---- numbers ----

func atoi(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, _ := strconv.ParseFloat(s, 64)
		return int64(f)
	}
	return n
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func fromMS(s string) time.Time {
	n := atoi(s)
	if n == 0 {
		return time.Time{}
	}
	return time.UnixMilli(n)
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// ---- entities ----

// entState is the view of an ent hash at now, with the precedence of
// README §2.1: ban > unexpired block > negative balance > active.
func entState(e limits.Entity, h map[string]string, win []string, now time.Time) limits.EntityState {
	st := limits.EntityState{Entity: e, Status: limits.StatusActive}
	if len(h) == 0 {
		return st
	}
	st.Since = fromMS(h["since"])
	st.Strikes = int(atoi(h["strikes"]))
	st.Requests = atoi(h["requests"])
	st.Tier = h["tier"]
	// win holds start and count of spend, sday, swk and free (winFields).
	at := func(i int, start time.Time) int64 {
		if i+1 < len(win) && win[i] != "" && atoi(win[i]) >= start.UnixMilli() {
			return atoi(win[i+1])
		}
		return 0
	}
	n := now.UTC()
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	st.SpendUSD = float64(at(0, monthStart(now))) / 1e6
	st.SpendDailyUSD = float64(at(2, day)) / 1e6
	st.SpendWeeklyUSD = float64(at(4, day.AddDate(0, 0, -((int(day.Weekday())+6)%7)))) / 1e6
	st.FreeRequestsToday = at(6, day)
	st.PurchasedUSD = float64(atoi(h["purchased"])) / 1e6
	st.SpentTotalUSD = float64(atoi(h["spent"])) / 1e6
	if c, ok := h["credits"]; ok {
		f := float64(atoi(c)) / 1e6
		st.Credits = &f
		if f < 0 {
			st.Status = limits.StatusLimited
		}
	}
	switch {
	case h["ban_since"] != "":
		st.Status, st.Reason, st.By, st.Since, st.Until = limits.StatusBanned, h["ban_reason"], h["ban_by"], fromMS(h["ban_since"]), time.Time{}
	case h["blk_since"] != "" && blockActive(h["blk_until"], now):
		st.Status, st.Reason, st.By, st.Since, st.Until = limits.StatusSuspended, h["blk_reason"], h["blk_by"], fromMS(h["blk_since"]), fromMS(h["blk_until"])
	}
	return st
}

func blockActive(until string, now time.Time) bool {
	u := atoi(until)
	return u == 0 || now.UnixMilli() < u
}

func (v *Valkey) getCmds(e limits.Entity) [][]string {
	ent, win := v.key("ent", e), v.key("win", e)
	return [][]string{{"HGETALL", ent}, append([]string{"HMGET", win}, winFields...)}
}

// winFields are the fields of win read with ent, in pairs (start, count):
// month spend, UTC day spend, UTC week spend (from Monday) and UTC day
// :free requests (cache/valkey/README.md 2.1, task 258).
var winFields = []string{"spend:s", "spend:c", "sday:s", "sday:c", "swk:s", "swk:c", "free:s", "free:c"}

func parseGet(e limits.Entity, hres, wres valkey.ValkeyResult, now time.Time) (limits.EntityState, map[string]string, error) {
	h, err := hres.AsStrMap()
	if err != nil {
		return limits.EntityState{}, nil, err
	}
	w, err := wres.ToArray()
	if err != nil {
		return limits.EntityState{}, nil, err
	}
	win := make([]string, len(w))
	for i := range w {
		win[i], _ = w[i].ToString()
	}
	return entState(e, h, win, now), h, nil
}

// Entity reads ent: (and the monthly spend of win:) of e. An entity without
// state is active with zero counters, like GET /admin/entities/{tipo}/{id}.
func (v *Valkey) Entity(ctx context.Context, e limits.Entity) (limits.EntityState, error) {
	res, err := v.pipe(ctx, v.getCmds(e)...)
	if err != nil {
		return limits.EntityState{}, err
	}
	st, _, err := parseGet(e, res[0], res[1], v.now())
	return st, err
}

// Entities lists entities like limits.Valkey.List (idx:ent or
// idx:status:<status>, newest first, pages of limit, at most 10 pages),
// without repairing the index: the panel never writes those keys. limit <= 0
// = 1000 (maximum 10 000); status "" or active = every entity.
func (v *Valkey) Entities(ctx context.Context, status limits.Status, limit int) ([]limits.EntityState, error) {
	if limit <= 0 {
		limit = 1000
	}
	limit = min(limit, 10000)
	idx := v.p + ":idx:ent"
	if status != "" && status != limits.StatusActive {
		idx = v.p + ":idx:status:" + string(status)
	}
	c, err := v.client()
	if err != nil {
		return nil, err
	}
	now := v.now()
	out := []limits.EntityState{}
	for page := range 10 {
		start := int64(page * limit)
		members, err := v.pipeC(ctx, c, []string{"ZREVRANGE", idx, strconv.FormatInt(start, 10), strconv.FormatInt(start+int64(limit)-1, 10)})[0].AsStrSlice()
		if err != nil {
			return nil, err
		}
		if len(members) == 0 {
			break
		}
		cmds := make([][]string, 0, 2*len(members))
		for _, m := range members {
			cmds = append(cmds, v.getCmds(parseMember(m))...)
		}
		res := v.pipeC(ctx, c, cmds...)
		for i, m := range members {
			st, h, err := parseGet(parseMember(m), res[2*i], res[2*i+1], now)
			if err != nil {
				return nil, err
			}
			if len(h) == 0 || (status != "" && st.Status != status) || len(out) >= limit {
				continue
			}
			out = append(out, st)
		}
		if len(out) >= limit || len(members) < limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	return out, nil
}

// ---- live limits ----

// LimitState is one limit of an entity computed from tb:, win:, q5h:, qwk:
// and conc: with the effective limits of its tier (README §2.1 and §3).
// Money limits are in USD.
type LimitState struct {
	// Name is the limit (rps, rpm, tpm, itpm, otpm, rpd, tpd, spend_monthly,
	// quota_5h, quota_weekly, quota_monthly, concurrency).
	Name string `json:"name"`
	// Window is the consumption window: second, minute, day, 5h, week,
	// month, or "" (concurrency).
	Window    string  `json:"window,omitempty"`
	Limit     float64 `json:"limit"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	// Reset is when the limit is whole again (zero = nothing to recover).
	Reset time.Time `json:"reset,omitzero"`
}

type bucketView struct{ t, cap, rate float64 }

// bucket reads <u>:* of tb at now (lib.lua bucket): full when missing,
// continuous refill, cut at cap when cap/rate changed.
func bucket(tb map[string]string, u string, now int64, capacity, rate float64) bucketView {
	ts, okTS := tb[u+":ts"]
	ts0, okT := tb[u+":t"]
	if !okTS || !okT {
		return bucketView{capacity, capacity, rate}
	}
	t := atof(ts0)
	if atof(tb[u+":cap"]) != capacity || atof(tb[u+":rate"]) != rate {
		t = min(t, capacity)
	}
	if last := atoi(ts); now > last {
		t = min(capacity, t+float64(now-last)/1000*rate)
	}
	return bucketView{t, capacity, rate}
}

func bucketState(name, window string, b bucketView, now time.Time) LimitState {
	rem := max(0, b.t)
	st := LimitState{Name: name, Window: window, Limit: b.cap, Used: b.cap - rem, Remaining: rem}
	if rem < b.cap && b.rate > 0 {
		st.Reset = now.Add(time.Duration((b.cap - rem) / b.rate * float64(time.Second)))
	}
	return st
}

// fixedWindow reads <w>:s/<w>:c of win (lib.lua fwin).
func fixedWindow(win map[string]string, w string, now, length int64) (start, count int64) {
	s, ok := win[w+":s"]
	start, count = atoi(s), atoi(win[w+":c"])
	if !ok || now-start >= length {
		return now / length * length, 0
	}
	return start, count
}

// monthWindow reads <w>:s/<w>:c of win (lib.lua mwin).
func monthWindow(win map[string]string, w string, ms int64) int64 {
	s, ok := win[w+":s"]
	if !ok || ms > atoi(s) {
		return 0
	}
	return atoi(win[w+":c"])
}

// slide sums the live buckets of a sliding window (lib.lua slide).
func slide(m map[string]string, now, span, width int64) (sum float64, oldest int64) {
	oldest = -1
	for f, val := range m {
		s, err := strconv.ParseInt(f, 10, 64)
		if err != nil || s+width <= now-span {
			continue
		}
		sum += atof(val)
		if oldest < 0 || s < oldest {
			oldest = s
		}
	}
	return sum, oldest
}

func countState(name, window string, limit, used float64, reset time.Time) LimitState {
	return LimitState{Name: name, Window: window, Limit: limit, Used: used, Remaining: max(0, limit-used), Reset: reset}
}

// Limits computes limit/remaining/reset of every limit set in lim (the
// effective limits of the entity's tier) from the keys of e. Zero limits
// are omitted, in the evaluation order of README §3.2.
func (v *Valkey) Limits(ctx context.Context, e limits.Entity, lim config.Limits) ([]LimitState, error) {
	res, err := v.pipe(ctx,
		[]string{"HGETALL", v.key("tb", e)},
		[]string{"HGETALL", v.key("win", e)},
		[]string{"HGETALL", v.key("q5h", e)},
		[]string{"HGETALL", v.key("qwk", e)},
		[]string{"GET", v.key("conc", e)},
	)
	if err != nil {
		return nil, err
	}
	maps := make([]map[string]string, 4)
	for i := range maps {
		if maps[i], err = res[i].AsStrMap(); err != nil {
			return nil, err
		}
	}
	conc, err := res[4].AsInt64()
	if err != nil && !valkey.IsValkeyNil(err) {
		return nil, err
	}
	return liveLimits(maps[0], maps[1], maps[2], maps[3], conc, lim, v.now()), nil
}

func liveLimits(tb, win, q5h, qwk map[string]string, conc int64, lim config.Limits, nowT time.Time) []LimitState {
	now := nowT.UnixMilli()
	fixed := lim.Algorithm == "fixed_window"
	at := func(ms int64) time.Time { return time.UnixMilli(ms) }
	var out []LimitState
	if lim.RPS > 0 {
		burst := lim.Burst
		if burst <= 0 {
			burst = lim.RPS
		}
		out = append(out, bucketState("rps", "second", bucket(tb, "rps", now, float64(burst), float64(lim.RPS)), nowT))
	}
	perMinute := func(name string, l int, canFix bool) {
		if l <= 0 {
			return
		}
		if canFix && fixed {
			s, c := fixedWindow(win, name, now, minuteMS)
			reset := time.Time{}
			if c > 0 {
				reset = at(s + minuteMS)
			}
			out = append(out, countState(name, "minute", float64(l), float64(c), reset))
			return
		}
		out = append(out, bucketState(name, "minute", bucket(tb, name, now, float64(l), float64(l)/60), nowT))
	}
	perMinute("rpm", lim.RPM, true)
	perMinute("tpm", lim.TPM, true)
	perMinute("itpm", lim.ITPM, false)
	perMinute("otpm", lim.OTPM, false)
	for _, d := range []struct {
		name string
		l    int
	}{{"rpd", lim.RPD}, {"tpd", lim.TPD}} {
		if d.l <= 0 {
			continue
		}
		s, c := fixedWindow(win, d.name, now, dayMS)
		reset := time.Time{}
		if c > 0 {
			reset = at(s + dayMS)
		}
		out = append(out, countState(d.name, "day", float64(d.l), float64(c), reset))
	}
	ms := monthStart(nowT).UnixMilli()
	next := monthStart(nowT).AddDate(0, 1, 0)
	monthly := func(name, w string, l float64) {
		c := float64(monthWindow(win, w, ms)) / 1e6
		reset := time.Time{}
		if c > 0 {
			reset = next
		}
		out = append(out, countState(name, "month", l, c, reset))
	}
	if lim.SpendMonthlyUSD > 0 {
		monthly("spend_monthly", "spend", lim.SpendMonthlyUSD)
	}
	if q := lim.QuotaUSD; q != nil && q.MonthlyUSD > 0 {
		for _, s := range []struct {
			name, window string
			pct          float64
			m            map[string]string
			span, width  int64
		}{
			{"quota_5h", "5h", q.Rolling5hPct, q5h, 5 * hourMS, minuteMS},
			{"quota_weekly", "week", q.WeeklyPct, qwk, 7 * dayMS, hourMS},
		} {
			if s.pct <= 0 {
				continue
			}
			sum, oldest := slide(s.m, now, s.span, s.width)
			reset := time.Time{}
			if oldest >= 0 {
				reset = at(oldest + s.span)
			}
			out = append(out, countState(s.name, s.window, q.MonthlyUSD*s.pct/100, sum/1e6, reset))
		}
		monthly("quota_monthly", "qmo", q.MonthlyUSD)
	}
	if lim.Concurrency > 0 {
		out = append(out, countState("concurrency", "", float64(lim.Concurrency), float64(conc), time.Time{}))
	}
	return out
}

// ---- instances ----

// Instance is a live wwz instance (heartbeat inst:{id}, README §2.2).
type Instance struct {
	ID       string    `json:"id"`
	Version  string    `json:"version"`
	Addr     string    `json:"addr,omitempty"`
	Started  time.Time `json:"started,omitzero"`
	Beat     time.Time `json:"beat"`
	RPS      float64   `json:"rps"`
	Conns    int64     `json:"conns"`
	InFlight int64     `json:"inflight"`
	// Store is ok, open or local.
	Store string `json:"store"`
}

// Instances lists the instances whose heartbeat is younger than 15 s
// (idx:inst + inst:{id}), sorted by id. Members whose hash expired are
// skipped.
func (v *Valkey) Instances(ctx context.Context) ([]Instance, error) {
	idx := v.p + ":idx:inst"
	from := strconv.FormatInt(v.now().Add(-instAlive).UnixMilli(), 10)
	c, err := v.client()
	if err != nil {
		return nil, err
	}
	ids, err := v.pipeC(ctx, c, []string{"ZRANGEBYSCORE", idx, from, "+inf"})[0].AsStrSlice()
	if err != nil {
		return nil, err
	}
	out := []Instance{}
	if len(ids) == 0 {
		return out, nil
	}
	cmds := make([][]string, len(ids))
	for i, id := range ids {
		cmds[i] = []string{"HGETALL", v.p + ":inst:{" + escapeID(id) + "}"}
	}
	res := v.pipeC(ctx, c, cmds...)
	for i, id := range ids {
		h, err := res[i].AsStrMap()
		if err != nil {
			return nil, err
		}
		if len(h) == 0 {
			continue
		}
		if h["id"] != "" {
			id = h["id"]
		}
		out = append(out, Instance{
			ID: id, Version: h["version"], Addr: h["addr"], Started: fromMS(h["started"]), Beat: fromMS(h["beat"]),
			RPS: atof(h["rps"]), Conns: atoi(h["conns"]), InFlight: atoi(h["inflight"]), Store: h["store"],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- events stream ----

// StreamLag is the lag field of XINFO GROUPS for group (wwz_panel_stream_lag);
// -1 when Valkey does not know it, 0 when the stream or the group does not
// exist yet.
func (v *Valkey) StreamLag(ctx context.Context, group string) (int64, error) {
	c, err := v.client()
	if err != nil {
		return 0, err
	}
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	groups, err := c.Do(cctx, c.B().XinfoGroups().Key(v.StreamKey()).Build()).ToArray()
	if err != nil {
		if strings.Contains(err.Error(), "no such key") {
			return 0, nil
		}
		return 0, err
	}
	for _, g := range groups {
		m, _ := g.AsMap()
		name := m["name"]
		if s, _ := name.ToString(); s != group {
			continue
		}
		lag := m["lag"]
		if lag.IsNil() {
			return -1, nil
		}
		return lag.AsInt64()
	}
	return 0, nil
}

// ---- policies (ADR 0025) ----

// Policies is the content of P:cfg:policies (README §2.3).
type Policies struct {
	// Version is 0 when the hash has no version (no policies).
	Version int64 `json:"version"`
	// Tiers maps tier name → JSON limits object (fields tier:<name>).
	Tiers map[string]json.RawMessage `json:"tiers"`
	// Entities maps entity → assigned tier (fields ent:<tipo>:<id>, written
	// by wwz).
	Entities map[limits.Entity]string `json:"-"`
}

func parsePolicies(h map[string]string) Policies {
	p := Policies{Tiers: map[string]json.RawMessage{}, Entities: map[limits.Entity]string{}}
	for f, val := range h {
		switch {
		case f == "version":
			p.Version = atoi(val)
		case strings.HasPrefix(f, "tier:"):
			p.Tiers[strings.TrimPrefix(f, "tier:")] = json.RawMessage(val)
		case strings.HasPrefix(f, "ent:"):
			p.Entities[parseMember(strings.TrimPrefix(f, "ent:"))] = val
		}
	}
	return p
}

// Policies reads P:cfg:policies (HGETALL).
func (v *Valkey) Policies(ctx context.Context) (Policies, error) {
	res, err := v.pipe(ctx, []string{"HGETALL", v.policiesKey()})
	if err != nil {
		return Policies{}, err
	}
	h, err := res[0].AsStrMap()
	if err != nil {
		return Policies{}, err
	}
	return parsePolicies(h), nil
}

func compactTier(raw json.RawMessage) (string, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return "", errors.New("não é um objeto JSON")
	}
	var b bytes.Buffer
	_ = json.Compact(&b, raw)
	return b.String(), nil
}

// ReplaceTiers makes tiers the whole set of tier:<name> fields of
// cfg:policies, with the optimistic concurrency of README §2.3: WATCH,
// version check (ErrPolicyConflict when it is not version), check(current)
// (its error aborts and is returned as is; use it for the 400 of removing a
// tier still cited by ent:*), then MULTI/HDEL/HSET/HINCRBY version 1/EXEC
// (aborted EXEC = ErrPolicyConflict) and PUBLISH P:cfg:changed
// {"version": n}. Fields ent:* are never touched. Returns the new version.
func (v *Valkey) ReplaceTiers(ctx context.Context, version int64, tiers map[string]json.RawMessage, check func(Policies) error) (int64, error) {
	set := make(map[string]string, len(tiers))
	for name, raw := range tiers {
		if !config.ValidTierName(name) {
			return 0, fmt.Errorf("%w: nome de tier %q", ErrInvalidPolicy, name)
		}
		s, err := compactTier(raw)
		if err != nil {
			return 0, fmt.Errorf("%w: tier %q: %v", ErrInvalidPolicy, name, err)
		}
		set[name] = s
	}
	c, err := v.client()
	if err != nil {
		return 0, err
	}
	cctx, cancel := v.ctx(ctx)
	defer cancel()
	key := v.policiesKey()
	var n int64
	err = c.Dedicated(func(dc valkey.DedicatedClient) error {
		unwatch := func() { _ = dc.Do(cctx, dc.B().Unwatch().Build()).Error() }
		rd := dc.DoMulti(cctx, dc.B().Watch().Key(key).Build(), dc.B().Hgetall().Key(key).Build())
		h, herr := rd[1].AsStrMap()
		if err := cmp.Or(rd[0].Error(), herr); err != nil {
			unwatch()
			return err
		}
		cur := parsePolicies(h)
		if cur.Version != version {
			unwatch()
			return fmt.Errorf("%w: versão atual %d, enviada %d", ErrPolicyConflict, cur.Version, version)
		}
		if check != nil {
			if err := check(cur); err != nil {
				unwatch()
				return err
			}
		}
		cmds := valkey.Commands{dc.B().Multi().Build()}
		var del []string
		for name := range cur.Tiers {
			if _, ok := set[name]; !ok {
				del = append(del, "tier:"+name)
			}
		}
		if len(del) > 0 {
			sort.Strings(del)
			cmds = append(cmds, dc.B().Hdel().Key(key).Field(del...).Build())
		}
		names := make([]string, 0, len(set))
		for name := range set {
			if string(cur.Tiers[name]) != set[name] {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			hs := dc.B().Hset().Key(key).FieldValue()
			for _, name := range names {
				hs = hs.FieldValue("tier:"+name, set[name])
			}
			cmds = append(cmds, hs.Build())
		}
		cmds = append(cmds, dc.B().Hincrby().Key(key).Field("version").Increment(1).Build(), dc.B().Exec().Build())
		res := dc.DoMulti(cctx, cmds...)
		arr, err := res[len(res)-1].ToArray()
		if valkey.IsValkeyNil(err) {
			return fmt.Errorf("%w: outro escritor alterou cfg:policies", ErrPolicyConflict)
		}
		if err == nil {
			// EXEC answers one reply per queued command; HINCRBY is last.
			n, err = arr[len(arr)-1].AsInt64()
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	msg, _ := json.Marshal(map[string]int64{"version": n})
	return n, publishErr(n, c.Do(cctx, c.B().Publish().Channel(v.p+":cfg:changed").Message(string(msg)).Build()).Error())
}

// publishErr reports a PUBLISH that failed after cfg:policies was written
// (the instances still apply version n on their next read).
func publishErr(n int64, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("source: cfg:policies versão %d gravada, mas o PUBLISH falhou: %w", n, err)
}
