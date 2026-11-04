package limits

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// bucket is a token bucket with continuous refill (Anthropic/OpenAI style).
type bucket struct {
	tokens float64
	cap    float64
	rate   float64 // tokens per second
	last   time.Time
}

func (b *bucket) refill(now time.Time, cap, rate float64) {
	if b.last.IsZero() {
		b.tokens, b.cap, b.rate, b.last = cap, cap, rate, now
		return
	}
	if cap != b.cap || rate != b.rate {
		b.cap, b.rate = cap, rate
		if b.tokens > cap {
			b.tokens = cap
		}
	}
	el := now.Sub(b.last).Seconds()
	if el > 0 {
		b.tokens = math.Min(b.cap, b.tokens+el*b.rate)
		b.last = now
	}
}

// take removes n tokens if available; returns remaining and the time until
// the bucket is full again.
func (b *bucket) take(n float64) (ok bool, remaining float64, resetIn time.Duration) {
	if b.tokens >= n {
		b.tokens -= n
		ok = true
	}
	return ok, math.Max(0, b.tokens), b.resetIn()
}

// resetIn is the time until the bucket is full again.
func (b *bucket) resetIn() time.Duration {
	if b.rate <= 0 {
		return 0
	}
	return time.Duration((b.cap - b.tokens) / b.rate * float64(time.Second))
}

func (b *bucket) give(n float64) { b.tokens = math.Min(b.cap, b.tokens+n) }

// window is a fixed window counter.
type window struct {
	start time.Time
	count float64
}

func (w *window) add(now time.Time, length time.Duration, n float64) float64 {
	if now.Sub(w.start) >= length {
		w.start = now.Truncate(length)
		w.count = 0
	}
	w.count += n
	return w.count
}

// sliding sums (t, amount) pairs over a rolling duration.
type sliding struct {
	items []slideItem
}
type slideItem struct {
	t time.Time
	v float64
}

func (s *sliding) add(now time.Time, d time.Duration, v float64) float64 {
	cut := now.Add(-d)
	i := 0
	for i < len(s.items) && s.items[i].t.Before(cut) {
		i++
	}
	s.items = s.items[i:]
	s.items = append(s.items, slideItem{now, v})
	sum := 0.0
	for _, it := range s.items {
		sum += it.v
	}
	return sum
}

// pop removes the newest sample (rollback of a denied request).
func (s *sliding) pop() {
	if len(s.items) > 0 {
		s.items = s.items[:len(s.items)-1]
	}
}

// sum drops the samples older than d at now and sums the rest, without
// adding a sample (the 5 h quota reads its window this way, so its oldest
// sample is the oldest spend).
func (s *sliding) sum(now time.Time, d time.Duration) float64 {
	cut := now.Add(-d)
	i := 0
	for i < len(s.items) && s.items[i].t.Before(cut) {
		i++
	}
	s.items = s.items[i:]
	sum := 0.0
	for _, it := range s.items {
		sum += it.v
	}
	return sum
}

func (s *sliding) oldest() time.Time {
	if len(s.items) == 0 {
		return time.Time{}
	}
	return s.items[0].t
}

// monthWindow accumulates within a UTC calendar month and resets on the 1st.
type monthWindow struct {
	start time.Time
	count float64
}

func monthStart(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// add moves the window to the month of now (never backwards, so an
// out-of-order timestamp does not drop the current month) and adds n.
func (w *monthWindow) add(now time.Time, n float64) float64 {
	if ms := monthStart(now); ms.After(w.start) {
		w.start, w.count = ms, 0
	}
	w.count += n
	return w.count
}

// at returns the accumulated value as seen at now (zero once the month ended).
func (w *monthWindow) at(now time.Time) float64 {
	if monthStart(now).After(w.start) {
		return 0
	}
	return w.count
}

// mark is a block or a ban. Blocks and bans are independent (block:{id} and
// ban:{id} in spec 012): lifting one keeps the other.
type mark struct {
	reason string
	by     string
	since  time.Time
	until  time.Time // zero = no expiry
}

func (k *mark) activeAt(now time.Time) bool {
	return k != nil && (k.until.IsZero() || now.Before(k.until))
}

type entityRec struct {
	entity  Entity
	since   time.Time
	block   *mark
	ban     *mark
	rps     bucket
	rpm     bucket
	tpm     bucket
	itpm    bucket
	otpm    bucket
	rpmWin  window
	tpmWin  window
	rpd     window
	tpd     window
	conc    int
	spend   monthWindow // USD per UTC calendar month
	credits *float64
	// purchased and spent are the accumulated purchases and spend in
	// microdollars (ADR 0051, fields purchased/spent of ent).
	purchased int64
	spent     int64
	q5h       sliding
	qweek     calWindow // quota spend of the UTC week (from Monday 00:00)
	qmonth    monthWindow
	strikes   []time.Time
	requests  int64
	count     int64   // for after_n
	ramp      sliding // TPM samples for acceleration limit
	tier      string  // tier assigned by SetTier (display)
	// spendDay and spendWeek are the spend of the UTC day and week (USD),
	// freeDay the requests of the UTC day to :free models (task 258).
	spendDay  calWindow
	spendWeek calWindow
	freeDay   calWindow
	esc       escRec // escalation record of the abuse ladder (Escalator)
}

// calWindow accumulates within a calendar period (UTC day or week) that
// starts at start; a later start resets it, an earlier one never does.
type calWindow struct {
	start time.Time
	count float64
}

func (w *calWindow) add(start time.Time, n float64) {
	if start.After(w.start) {
		w.start, w.count = start, 0
	}
	w.count += n
}

// at is the accumulated value of the period that starts at start.
func (w *calWindow) at(start time.Time) float64 {
	if start.After(w.start) {
		return 0
	}
	return w.count
}

// escRec is the escalation record of an entity; zero once exp passed.
type escRec struct {
	level, strikes int
	exp            time.Time
}

// Memory is the in-process Store (single instance; lost on restart).
type Memory struct {
	mu       sync.Mutex
	ents     map[Entity]*entityRec
	attempts map[string]attempt
	events   []Event
	maxEv    int
	pol      memPolicies // local cluster policies (policies.go)
	pcOnce   sync.Once
	pc       *pcShards // prompt-cache registry (promptcache.go)
	respOnce sync.Once
	resp     *respLRU // Responses API registry (responses.go)
	respMax  int
	ccOnce   sync.Once
	cc       *ccMem // Gemini cachedContents (cachedcontents.go)
}

type attempt struct {
	n   int
	exp time.Time
}

// NewMemory creates an in-memory store.
func NewMemory() *Memory {
	return &Memory{ents: map[Entity]*entityRec{}, attempts: map[string]attempt{}, maxEv: 10000}
}

func (m *Memory) Kind() string { return "memory" }
func (m *Memory) Close() error { return nil }

func (m *Memory) rec(e Entity) *entityRec {
	r, ok := m.ents[e]
	if !ok {
		r = &entityRec{entity: e, since: time.Now()}
		m.ents[e] = r
	}
	return r
}

// view is the panel state of r at now: ban > unexpired block > negative
// balance (limited) > active.
func (r *entityRec) view(now time.Time) EntityState {
	st := EntityState{Entity: r.entity, Status: StatusActive, Since: r.since, Strikes: len(r.strikes), Tier: r.tier, Requests: r.requests, SpendUSD: r.spend.at(now), PurchasedUSD: float64(r.purchased) / 1e6, SpentTotalUSD: float64(r.spent) / 1e6}
	day := dayStart(now)
	st.SpendDailyUSD, st.SpendWeeklyUSD = r.spendDay.at(day), r.spendWeek.at(weekStart(now))
	st.FreeRequestsToday = int64(r.freeDay.at(day))
	st.Quota5hUSD = r.q5h.sum(now, 5*time.Hour)
	st.QuotaWeeklyUSD, st.QuotaMonthlyUSD = r.qweek.at(weekStart(now)), r.qmonth.at(now)
	if st.Quota5hUSD > 0 {
		st.Quota5hSince = r.q5h.oldest()
	}
	if r.credits != nil {
		v := *r.credits
		st.Credits = &v
		if v < 0 {
			st.Status = StatusLimited
		}
	}
	var k *mark
	switch {
	case r.ban != nil:
		st.Status, k = StatusBanned, r.ban
	case r.block.activeAt(now):
		st.Status, k = StatusSuspended, r.block
	}
	if k != nil {
		st.Reason, st.By, st.Since, st.Until = k.reason, k.by, k.since, k.until
	}
	return st
}

// dropExpiredBlock forgets a block whose TTL passed on the wall clock (the
// equivalent of the block:{id} key expiring in Valkey).
func (r *entityRec) dropExpiredBlock(wall time.Time) {
	if r.block != nil && !r.block.activeAt(wall) {
		r.block = nil
	}
}

func (m *Memory) pushEvent(ev Event) {
	if len(m.events) >= m.maxEv {
		m.events = m.events[len(m.events)/2:]
	}
	m.events = append(m.events, ev)
}

// resolveWindow rewrites `mode: window` (spec 010) into `always` when now is
// inside the enforced part of the window and `off` otherwise, so every store
// (and the local-first path) sees only the stateless modes. The phase comes
// from the request clock, aligned to the Unix epoch: Memory and Valkey agree
// and nothing about the window lives in Valkey. subs is copied on write.
func resolveWindow(subs []Subject, now time.Time) []Subject {
	out := subs
	cloned := false
	for i, s := range subs {
		if s.Limits.Mode != "window" {
			continue
		}
		if !cloned {
			out, cloned = slices.Clone(subs), true
		}
		mode := "always"
		if w := s.Limits.Window; w != nil && !w.Active(now) {
			mode = "off"
		}
		out[i].Limits.Mode = mode
	}
	return out
}

// Check implements Store.
func (m *Memory) Check(_ context.Context, req CheckRequest) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	req.Subjects = resolveWindow(req.Subjects, now)
	dec := m.check(req, now)
	if !dec.Allowed && accState(dec.LimitName) {
		// A 429 by rpm/tpm/itpm still reports the token units of the
		// denying entity that the evaluation did not reach (task 064).
		for _, s := range req.Subjects {
			if s.Entity == dec.Entity {
				var p core.RateLimitState
				m.rec(s.Entity).peekTokens(s.Limits, now, dec.LimitName, &p)
				dec.State = mergeUnits(dec.State, projectUnits(s.Entity, p))
				break
			}
		}
	}
	dec.State = sumTokens(dec.State)
	return dec, nil
}

// unitRank orders the units evaluated with shared state (rpm, tpm, itpm,
// otpm), the same order as check and lua/lib.lua eval.
func unitRank(limit string) int {
	switch limit {
	case "rpm":
		return 1
	case "tpm":
		return 2
	case "itpm":
		return 3
	case "otpm":
		return 4
	}
	return 0
}

// peekTokens fills st with the token units (tpm, itpm, otpm) evaluated after
// the denying limit, without consuming anything (lua/lib.lua peek).
func (r *entityRec) peekTokens(l config.Limits, now time.Time, denied string, st *core.RateLimitState) {
	after := unitRank(denied)
	if l.TPM > 0 && after < 2 {
		if l.Algorithm == "fixed_window" {
			start, c := r.tpmWin.start, r.tpmWin.count
			if now.Sub(start) >= time.Minute {
				start, c = now.Truncate(time.Minute), 0
			}
			st.TokensLimit, st.TokensRemaining, st.TokensReset = l.TPM, int(math.Max(0, float64(l.TPM)-c)), time.Minute-now.Sub(start)
		} else {
			r.tpm.refill(now, float64(l.TPM), float64(l.TPM)/60)
			st.TokensLimit, st.TokensRemaining, st.TokensReset = l.TPM, int(math.Max(0, r.tpm.tokens)), r.tpm.resetIn()
		}
		st.Present = true
	}
	if l.ITPM > 0 && after < 3 {
		r.itpm.refill(now, float64(l.ITPM), float64(l.ITPM)/60)
		st.Present, st.InputTokensLimit, st.InputTokensRemain, st.TokensReset = true, l.ITPM, int(math.Max(0, r.itpm.tokens)), r.itpm.resetIn()
	}
	if l.OTPM > 0 && after < 4 {
		r.otpm.refill(now, float64(l.OTPM), float64(l.OTPM)/60)
		st.Present, st.OutputTokensLimit, st.OutputTokensRem = true, l.OTPM, int(math.Max(0, r.otpm.tokens))
		st.TokensReset = otpmReset(l, st.TokensReset, r.otpm.resetIn())
	}
}

// otpmReset is the token reset once otpm is evaluated: the later of the
// tpm/itpm reset of the same entity and the otpm one, or the otpm reset alone.
func otpmReset(l config.Limits, cur, out time.Duration) time.Duration {
	if l.TPM > 0 || l.ITPM > 0 {
		return max(cur, out)
	}
	return out
}

// mergeUnits folds the units of one level (src) into the request state
// (dst): per unit (requests, tokens, input, output) the state is that of the
// level with the smallest remaining, with its limit; on a tie the later
// level wins (ADR 0051, spec 010: "the most restrictive limit currently in
// effect"). The token reset follows the last token unit taken from src.
func mergeUnits(dst, src core.RateLimitState) core.RateLimitState {
	take := func(dl, dr, sl, sr int) bool { return sl > 0 && (dl == 0 || sr <= dr) }
	tok := false
	if take(dst.RequestsLimit, dst.RequestsRemaining, src.RequestsLimit, src.RequestsRemaining) {
		dst.RequestsLimit, dst.RequestsRemaining, dst.RequestsReset = src.RequestsLimit, src.RequestsRemaining, src.RequestsReset
	}
	if take(dst.TokensLimit, dst.TokensRemaining, src.TokensLimit, src.TokensRemaining) {
		dst.TokensLimit, dst.TokensRemaining, tok = src.TokensLimit, src.TokensRemaining, true
	}
	if take(dst.InputTokensLimit, dst.InputTokensRemain, src.InputTokensLimit, src.InputTokensRemain) {
		dst.InputTokensLimit, dst.InputTokensRemain, tok = src.InputTokensLimit, src.InputTokensRemain, true
	}
	if take(dst.OutputTokensLimit, dst.OutputTokensRem, src.OutputTokensLimit, src.OutputTokensRem) {
		dst.OutputTokensLimit, dst.OutputTokensRem, tok = src.OutputTokensLimit, src.OutputTokensRem, true
	}
	if tok {
		dst.TokensReset = src.TokensReset
	}
	if src.ProjectTokensLimit > 0 {
		dst.ProjectTokensLimit, dst.ProjectTokensRemain, dst.ProjectTokensReset = src.ProjectTokensLimit, src.ProjectTokensRemain, src.ProjectTokensReset
	}
	dst.Present = dst.Present || src.Present
	return dst
}

// projectUnits copies the tokens unit of a project level with tpm into the
// project fields of st, which mergeUnits carries as they are (the openai
// x-ratelimit-*-project-tokens; spec 010, task 234). The reset is the
// level's token reset (with itpm/otpm also declared on the project, the
// later of them, as for the tokens headers).
func projectUnits(e Entity, st core.RateLimitState) core.RateLimitState {
	if e.Kind == KindProject && st.TokensLimit > 0 {
		st.ProjectTokensLimit, st.ProjectTokensRemain, st.ProjectTokensReset = st.TokensLimit, st.TokensRemaining, st.TokensReset
	}
	return st
}

// sumTokens reports anthropic-ratelimit-tokens-* as input + output when only
// itpm/otpm are configured (spec 006): the driver emits the tokens headers
// only with TokensLimit set.
func sumTokens(s core.RateLimitState) core.RateLimitState {
	if s.TokensLimit > 0 || (s.InputTokensLimit == 0 && s.OutputTokensLimit == 0) {
		return s
	}
	s.TokensLimit = s.InputTokensLimit + s.OutputTokensLimit
	s.TokensRemaining = max(0, s.InputTokensRemain) + max(0, s.OutputTokensRem)
	return s
}

// check evaluates req under m.mu.
func (m *Memory) check(req CheckRequest, now time.Time) Decision {
	dec := Decision{Allowed: true}
	// undo reverts every counter touched by earlier subjects (and earlier
	// limits of the current one) when a later check denies: a denied request
	// consumes nothing (spec 012, step 2).
	var undo []func()
	rollback := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	admitted := make([]*entityRec, 0, len(req.Subjects))
	for _, s := range req.Subjects {
		r := m.rec(s.Entity)
		r.count++
		// Bans, then blocks, before any limit.
		if r.ban != nil {
			rollback()
			return Decision{Allowed: false, Kind: core.ErrAccountBanned, Entity: s.Entity, LimitName: "ban", Message: r.ban.reason}
		}
		if r.block.activeAt(now) {
			rollback()
			ra := time.Duration(0)
			if !r.block.until.IsZero() {
				ra = r.block.until.Sub(now)
			}
			return Decision{Allowed: false, Kind: core.ErrAccountSuspended, Entity: s.Entity, LimitName: "block", RetryAfter: ra, Message: r.block.reason}
		}
		l := s.Limits
		var st core.RateLimitState // this level's units, folded into dec.State by mergeUnits
		if l.Mode == "off" || (l.Mode == "after_n" && r.count <= int64(l.AfterN)) {
			continue
		}
		fixed := l.Algorithm == "fixed_window"
		// rps (or the short window of the rpm, Subject.Window)
		if rate, burst, limit, has := s.requestRate(); has {
			r.rps.refill(now, burst, rate)
			ok, rem, reset := r.rps.take(1)
			if !ok {
				rollback()
				return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "rps", RetryAfter: maxDur(reset, 100*time.Millisecond), Message: s.requestRateMessage(), State: core.RateLimitState{Present: true, RequestsLimit: limit, RequestsRemaining: int(rem), RequestsReset: reset}}
			}
			undo = append(undo, giveBack(&r.rps, 1))
		}
		// rpm
		if l.RPM > 0 {
			if fixed {
				c := r.rpmWin.add(now, time.Minute, 1)
				reset := time.Minute - now.Sub(r.rpmWin.start)
				st.Present, st.RequestsLimit, st.RequestsRemaining, st.RequestsReset = true, l.RPM, int(math.Max(0, float64(l.RPM)-c)), reset
				if c > float64(l.RPM) {
					r.rpmWin.count--
					rollback()
					return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "rpm", RetryAfter: reset, Message: fmt.Sprintf("rate limit of %d requests per minute exceeded", l.RPM), State: mergeUnits(dec.State, projectUnits(s.Entity, st))}
				}
				undo = append(undo, unadd(&r.rpmWin, 1))
			} else {
				r.rpm.refill(now, float64(l.RPM), float64(l.RPM)/60)
				ok, rem, reset := r.rpm.take(1)
				st.Present, st.RequestsLimit, st.RequestsRemaining, st.RequestsReset = true, l.RPM, int(rem), reset
				if !ok {
					rollback()
					return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "rpm", RetryAfter: maxDur(time.Duration(60.0/float64(l.RPM)*float64(time.Second)), time.Second), Message: fmt.Sprintf("rate limit of %d requests per minute exceeded", l.RPM), State: mergeUnits(dec.State, projectUnits(s.Entity, st))}
				}
				undo = append(undo, giveBack(&r.rpm, 1))
			}
		}
		// tokens per minute (reserve prompt + output estimate)
		reserve := float64(req.EstPromptTokens + req.EstOutputTokens)
		if l.ExcludeCacheRead {
			reserve -= float64(req.CachedTokens)
			if reserve < 0 {
				reserve = 0
			}
		}
		if l.TPM > 0 {
			if fixed {
				c := r.tpmWin.add(now, time.Minute, reserve)
				reset := time.Minute - now.Sub(r.tpmWin.start)
				st.Present, st.TokensLimit, st.TokensRemaining, st.TokensReset = true, l.TPM, int(math.Max(0, float64(l.TPM)-c)), reset
				if c > float64(l.TPM) {
					r.tpmWin.count -= reserve
					rollback()
					return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "tpm", RetryAfter: reset, Message: fmt.Sprintf("rate limit of %d tokens per minute exceeded (requested %d)", l.TPM, int(reserve)), State: mergeUnits(dec.State, projectUnits(s.Entity, st))}
				}
				undo = append(undo, unadd(&r.tpmWin, reserve))
			} else {
				r.tpm.refill(now, float64(l.TPM), float64(l.TPM)/60)
				ok, rem, reset := r.tpm.take(reserve)
				st.Present, st.TokensLimit, st.TokensRemaining, st.TokensReset = true, l.TPM, int(rem), reset
				if !ok {
					rollback()
					need := reserve - r.tpm.tokens
					ra := time.Duration(need / r.tpm.rate * float64(time.Second))
					return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "tpm", RetryAfter: maxDur(ra, 100*time.Millisecond), Message: fmt.Sprintf("rate limit of %d tokens per minute exceeded (requested %d, remaining %d)", l.TPM, int(reserve), int(rem)), State: mergeUnits(dec.State, projectUnits(s.Entity, st))}
				}
				undo = append(undo, giveBack(&r.tpm, reserve))
			}
		}
		if l.ITPM > 0 {
			in := float64(req.EstPromptTokens)
			if l.ExcludeCacheRead {
				in -= float64(req.CachedTokens)
			}
			r.itpm.refill(now, float64(l.ITPM), float64(l.ITPM)/60)
			ok, rem, reset := r.itpm.take(in)
			st.Present, st.InputTokensLimit, st.InputTokensRemain, st.TokensReset = true, l.ITPM, int(rem), reset
			if !ok {
				rollback()
				return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "itpm", RetryAfter: maxDur(time.Duration((in-r.itpm.tokens)/r.itpm.rate*float64(time.Second)), 100*time.Millisecond), Message: fmt.Sprintf("input tokens per minute limit of %d exceeded", l.ITPM), State: mergeUnits(dec.State, projectUnits(s.Entity, st))}
			}
			undo = append(undo, giveBack(&r.itpm, in))
		}
		if l.OTPM > 0 {
			// Output tokens are counted as generated (Finish); here only check headroom.
			r.otpm.refill(now, float64(l.OTPM), float64(l.OTPM)/60)
			st.Present, st.OutputTokensLimit, st.OutputTokensRem = true, l.OTPM, int(r.otpm.tokens)
			st.TokensReset = otpmReset(l, st.TokensReset, r.otpm.resetIn())
			if r.otpm.tokens < 1 {
				rollback()
				return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "otpm", RetryAfter: time.Second, Message: fmt.Sprintf("output tokens per minute limit of %d exceeded", l.OTPM), State: mergeUnits(dec.State, projectUnits(s.Entity, st))}
			}
		}
		// daily
		if l.RPD > 0 {
			c := r.rpd.add(now, 24*time.Hour, 1)
			if c > float64(l.RPD) {
				r.rpd.count--
				rollback()
				return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "rpd", RetryAfter: untilNextDay(now), Message: fmt.Sprintf("daily request limit of %d exceeded", l.RPD)}
			}
			undo = append(undo, unadd(&r.rpd, 1))
		}
		if l.TPD > 0 {
			c := r.tpd.add(now, 24*time.Hour, reserve)
			if c > float64(l.TPD) {
				r.tpd.count -= reserve
				rollback()
				return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "tpd", RetryAfter: untilNextDay(now), Message: fmt.Sprintf("daily token limit of %d exceeded", l.TPD)}
			}
			undo = append(undo, unadd(&r.tpd, reserve))
		}
		// spend / credits / quota
		if l.SpendMonthlyUSD > 0 {
			cur := r.spend.add(now, 0)
			if cur >= l.SpendMonthlyUSD {
				rollback()
				return Decision{Allowed: false, Kind: core.ErrSpendLimit, Entity: s.Entity, LimitName: "spend_monthly", RetryAfter: untilNextMonth(now), Message: fmt.Sprintf("monthly spend limit of $%.2f reached", l.SpendMonthlyUSD)}
			}
		}
		if l.CreditsUSD != nil {
			if r.credits == nil {
				v := *l.CreditsUSD
				r.credits = &v
			}
			if *r.credits <= 0 {
				rollback()
				return Decision{Allowed: false, Kind: core.ErrQuota, Entity: s.Entity, LimitName: "credits", Message: "insufficient credits"}
			}
		}
		if q := l.QuotaUSD; q != nil && q.MonthlyUSD > 0 {
			m5 := r.q5h.sum(now, 5*time.Hour)
			if pct := q.Rolling5hPct; pct > 0 && m5 >= q.MonthlyUSD*pct/100 {
				rollback()
				return Decision{Allowed: false, Kind: core.ErrQuota, Entity: s.Entity, LimitName: "quota_5h", RetryAfter: r.q5h.oldest().Add(5 * time.Hour).Sub(now), Message: "5 hour usage limit exceeded"}
			}
			// The week turns on Monday 00:00 UTC, as the real's usage route
			// reports it (spec 006, task 296).
			wk := weekStart(now)
			if pct := q.WeeklyPct; pct > 0 && r.qweek.at(wk) >= q.MonthlyUSD*pct/100 {
				rollback()
				return Decision{Allowed: false, Kind: core.ErrQuota, Entity: s.Entity, LimitName: "quota_weekly", RetryAfter: wk.AddDate(0, 0, 7).Sub(now), Message: "weekly usage limit exceeded"}
			}
			mm := r.qmonth.add(now, 0)
			if mm >= q.MonthlyUSD {
				rollback()
				return Decision{Allowed: false, Kind: core.ErrQuota, Entity: s.Entity, LimitName: "quota_monthly", RetryAfter: untilNextMonth(now), Message: "monthly usage limit exceeded"}
			}
		}
		// ramp (acceleration limit)
		if l.Ramp != nil && l.Ramp.ThresholdTPM > 0 && l.Ramp.MaxIncreasePct > 0 {
			win := l.Ramp.Window
			if win == 0 {
				win = 15 * time.Minute
			}
			prev := 0.0
			for _, it := range r.ramp.items {
				prev += it.v
			}
			cur := r.ramp.add(now, win, reserve)
			if cur > float64(l.Ramp.ThresholdTPM) && prev > 0 && cur > prev*(1+l.Ramp.MaxIncreasePct/100) {
				r.ramp.pop()
				rollback()
				return Decision{Allowed: false, Kind: core.ErrSlowDown, Entity: s.Entity, LimitName: "ramp", RetryAfter: win / 4, Message: "traffic increased faster than the allowed ramp rate; slow down"}
			}
			undo = append(undo, r.ramp.pop)
		}
		// concurrency
		if l.Concurrency > 0 {
			if r.conc >= l.Concurrency {
				if l.OnLimit == "queue" && l.QueueTimeout > 0 {
					rollback()
					return Decision{Allowed: false, Queue: true, Kind: core.ErrOverloaded, Entity: s.Entity, LimitName: "concurrency", RetryAfter: l.QueueTimeout, Message: "too many concurrent requests"}
				}
				rollback()
				return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "concurrency", RetryAfter: time.Second, Message: fmt.Sprintf("concurrency limit of %d exceeded", l.Concurrency)}
			}
			r.conc++
			undo = append(undo, func() { r.conc-- })
		}
		dec.State = mergeUnits(dec.State, projectUnits(s.Entity, st))
		admitted = append(admitted, r)
	}
	// Requests only count once every level admitted the request.
	for _, r := range admitted {
		r.requests++
	}
	return dec
}

// giveBack returns n tokens to b on rollback.
func giveBack(b *bucket, n float64) func() { return func() { b.give(n) } }

// unadd reverts an add of n to a fixed window on rollback.
func unadd(w *window, n float64) func() { return func() { w.count -= n } }

// refundWindow returns n reserved units to a fixed window, but only while
// the window that took the reservation is still the current one.
func refundWindow(w *window, now time.Time, length time.Duration, n float64) {
	if n <= 0 || now.Sub(w.start) >= length || now.Before(w.start) {
		return
	}
	w.count = math.Max(0, w.count-n)
}

// Finish implements Store.
func (m *Memory) Finish(_ context.Context, req FinishRequest) error {
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	// Outside the lock: the callback may call back into the store
	// (SwapTier on promotion).
	for _, u := range m.finish(req, now) {
		req.OnUsage(u.e, u.u)
	}
	return nil
}

// entUsage is the promotion history of one account subject of Finish.
type entUsage struct {
	e Entity
	u config.PromotionUsage
}

func (m *Memory) finish(req FinishRequest, now time.Time) []entUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.Denial != nil {
		m.pushEvent(req.event(now))
		return nil
	}
	var usage []entUsage
	usd := float64(req.CostMicroUSD) / 1e6
	quota := float64(req.quotaDebit()) / 1e6
	free := FreeModel(req.Model)
	day := dayStart(now)
	for _, s := range req.Subjects {
		r := m.rec(s.Entity)
		if req.CostMicroUSD > 0 {
			r.spent += req.CostMicroUSD
		}
		if free {
			r.freeDay.add(day, 1)
		}
		if req.OnUsage != nil && s.Entity.Kind == KindAccount {
			usage = append(usage, entUsage{s.Entity, r.view(now).Usage(now)})
		}
		l := s.Limits
		if l.Concurrency > 0 && r.conc > 0 {
			r.conc--
		}
		// Return unused reserved output tokens (spec 010: tpm reserves
		// max_tokens and adjusts at the end, for every algorithm).
		diff := float64(req.EstOutputTokens - req.ActualOutput)
		if l.TPM > 0 && diff > 0 {
			if l.Algorithm == "fixed_window" {
				refundWindow(&r.tpmWin, now, time.Minute, diff)
			} else {
				r.tpm.give(diff)
			}
		}
		if l.TPD > 0 {
			refundWindow(&r.tpd, now, 24*time.Hour, diff)
		}
		if l.OTPM > 0 {
			r.otpm.refill(now, float64(l.OTPM), float64(l.OTPM)/60)
			r.otpm.take(float64(req.ActualOutput))
		}
		if usd > 0 {
			r.spend.add(now, usd)
			r.spendDay.add(day, usd)
			r.spendWeek.add(weekStart(now), usd)
			if r.credits != nil {
				*r.credits -= usd
			}
		}
		// The quota windows follow the quota debit, which is the cost
		// unless the driver reports its own (ADR 0072, README 3.5).
		if quota > 0 && l.QuotaUSD != nil {
			r.q5h.add(now, 5*time.Hour, quota)
			r.qweek.add(weekStart(now), quota)
			r.qmonth.add(now, quota)
		}
	}
	m.pushEvent(req.event(now))
	return usage
}

func (m *Memory) Block(_ context.Context, e Entity, reason string, d time.Duration, by string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	k := &mark{reason: reason, by: by, since: now}
	if d > 0 {
		k.until = now.Add(d)
	}
	m.rec(e).block = k
	m.pushEvent(Event{Time: now, Type: "block", Entity: &e, Fields: map[string]any{"reason": reason, "until": k.until, "by": by}})
	return nil
}

func (m *Memory) Unblock(_ context.Context, e Entity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.ents[e]
	if !ok || r.block == nil {
		return nil
	}
	now := time.Now()
	active := r.block.activeAt(now)
	r.block = nil
	if active {
		m.pushEvent(Event{Time: now, Type: "unblock", Entity: &e})
	}
	return nil
}

func (m *Memory) Ban(_ context.Context, e Entity, reason, by string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.rec(e).ban = &mark{reason: reason, by: by, since: now}
	m.pushEvent(Event{Time: now, Type: "ban", Entity: &e, Fields: map[string]any{"reason": reason, "by": by}})
	return nil
}

func (m *Memory) Unban(_ context.Context, e Entity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.ents[e]
	if !ok || r.ban == nil {
		return nil
	}
	r.ban = nil
	m.pushEvent(Event{Time: time.Now(), Type: "unban", Entity: &e})
	return nil
}

// Escalate implements Escalator.
func (m *Memory) Escalate(_ context.Context, e Entity, escalated bool, ttl time.Duration) (Escalation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rec(e)
	now := time.Now()
	if !now.Before(r.esc.exp) {
		r.esc = escRec{}
	}
	r.esc.strikes++
	if escalated {
		r.esc.level++
	}
	r.esc.exp = now.Add(max(ttl, time.Millisecond))
	return Escalation{Level: r.esc.level, Strikes: r.esc.strikes}, nil
}

func (m *Memory) Strike(_ context.Context, e Entity, reason string, ttl time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rec(e)
	now := time.Now()
	cut := now.Add(-ttl)
	kept := r.strikes[:0]
	for _, t := range r.strikes {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	r.strikes = append(kept, now)
	m.pushEvent(Event{Time: now, Type: "strike", Entity: &e, Fields: map[string]any{"reason": reason, "strikes": len(r.strikes)}})
	return len(r.strikes), nil
}

func (m *Memory) Get(_ context.Context, e Entity) (EntityState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.ents[e]
	if !ok {
		return EntityState{Entity: e, Status: StatusActive}, nil
	}
	now := time.Now()
	r.dropExpiredBlock(now)
	return r.view(now), nil
}

func (m *Memory) List(_ context.Context, status Status, limit int) ([]EntityState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var out []EntityState
	for _, r := range m.ents {
		r.dropExpiredBlock(now)
		if st := r.view(now); status == "" || st.Status == status {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) Attempts(_ context.Context, conversation, step string, ttl time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := conversation + "\x00" + step
	now := time.Now()
	a := m.attempts[k]
	if !a.exp.IsZero() && now.After(a.exp) {
		a = attempt{}
	}
	a.n++
	a.exp = now.Add(ttl)
	m.attempts[k] = a
	if len(m.attempts) > 100000 {
		for kk, v := range m.attempts {
			if now.After(v.exp) {
				delete(m.attempts, kk)
			}
		}
	}
	return a.n, nil
}

func (m *Memory) SetCredits(_ context.Context, e Entity, usd float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := usd
	m.rec(e).credits = &v
	return nil
}

// AddCredits implements CreditAdder.
func (m *Memory) AddCredits(_ context.Context, e Entity, usd float64) (EntityState, error) {
	if err := checkPurchase(usd); err != nil {
		return EntityState{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rec(e)
	add := micro(usd)
	if r.credits == nil {
		r.credits = new(float64)
	}
	// Through microdollars, like the HINCRBY of the Valkey store.
	*r.credits = float64(micro(*r.credits)+add) / 1e6
	r.purchased += add
	now := time.Now()
	r.dropExpiredBlock(now)
	return r.view(now), nil
}

func (m *Memory) Reset(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ents = map[Entity]*entityRec{}
	m.attempts = map[string]attempt{}
	m.events = nil
	m.pcache().reset()
	m.responses().reset()
	m.cachedContents().reset()
	return nil
}

func (m *Memory) Events(_ context.Context, limit int) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.events)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Event, 0, limit)
	for i := n - 1; i >= n-limit; i-- {
		out = append(out, m.events[i])
	}
	return out, nil
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func untilNextDay(now time.Time) time.Duration {
	n := now.UTC()
	next := time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, time.UTC)
	return next.Sub(n)
}

func untilNextMonth(now time.Time) time.Duration {
	n := now.UTC()
	next := time.Date(n.Year(), n.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	return next.Sub(n)
}
