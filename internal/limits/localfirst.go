package limits

import (
	"context"
	"crypto/sha1" //nolint:gosec // G505: SHA-1 is the EVALSHA script id, not a security primitive
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s)) //nolint:gosec // G401: EVALSHA id
	return hex.EncodeToString(sum[:])
}

// localFirst keeps rps and rpm token buckets in memory, with cap/N and
// rate/N (N = live instances), and folds their consumption into the global
// buckets every interval with sync_local (spec 014, README 6).
type localFirst struct {
	v        *Valkey
	interval time.Duration
	n        atomic.Int32

	mu       sync.Mutex
	ents     map[Entity]*lfEnt
	lastSync time.Time
}

type lfEnt struct {
	rps, rpm         bucket
	rpsCap, rpsRate  float64 // global limits last seen
	rpmCap, rpmRate  float64
	usedRPS, usedRPM float64
	reqs             int64
	last             time.Time // wall clock of the last use
}

func newLocalFirst(v *Valkey, interval time.Duration) *localFirst {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	lf := &localFirst{v: v, interval: interval, ents: map[Entity]*lfEnt{}}
	lf.n.Store(1)
	return lf
}

func (lf *localFirst) setInstances(n int) {
	lf.n.Store(int32(max(1, min(n, math.MaxInt32))))
}

func (lf *localFirst) reset() {
	lf.mu.Lock()
	lf.ents = map[Entity]*lfEnt{}
	lf.mu.Unlock()
}

// pendingRequests is the request count of e taken locally and not yet sent
// by sync_local (Finish adds it to the requests reported to OnUsage).
func (lf *localFirst) pendingRequests(e Entity) int64 {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	if le := lf.ents[e]; le != nil {
		return le.reqs
	}
	return 0
}

// remoteLimits reports whether l has something only Valkey can evaluate.
func remoteLimits(l config.Limits) bool {
	return l.TPM > 0 || l.ITPM > 0 || l.OTPM > 0 || l.RPD > 0 || l.TPD > 0 || l.Concurrency > 0 ||
		l.SpendMonthlyUSD > 0 || l.CreditsUSD != nil || (l.QuotaUSD != nil && l.QuotaUSD.MonthlyUSD > 0) ||
		(l.Ramp != nil && l.Ramp.ThresholdTPM > 0 && l.Ramp.MaxIncreasePct > 0) ||
		l.Mode == "after_n" || (l.Algorithm == "fixed_window" && l.RPM > 0)
}

type lfTake struct {
	e        *lfEnt
	rps, rpm bool
}

func (lf *localFirst) undo(takes []lfTake) {
	for _, t := range takes {
		if t.rps {
			t.e.rps.give(1)
			t.e.usedRPS--
		}
		if t.rpm {
			t.e.rpm.give(1)
			t.e.usedRPM--
		}
	}
}

// check takes the local tokens and goes to Valkey only when a subject has
// limits that live there.
func (lf *localFirst) check(ctx context.Context, req CheckRequest, now time.Time) (Decision, error) {
	n := float64(lf.n.Load())
	masks := make([]int, len(req.Subjects))
	remote := false
	var st core.RateLimitState
	takes := make([]lfTake, 0, len(req.Subjects))
	wall := time.Now()
	lf.mu.Lock()
	for i, s := range req.Subjects {
		l := s.Limits
		if l.Mode == "off" {
			continue
		}
		if s.Window > 0 {
			// A short evaluation window holds a request or two: split
			// across instances it would admit none. Valkey keeps it.
			remote = true
			continue
		}
		if remoteLimits(l) {
			remote = true
			if l.Mode == "after_n" || l.Algorithm == "fixed_window" {
				continue
			}
		}
		e := lf.ents[s.Entity]
		if e == nil {
			e = &lfEnt{}
			lf.ents[s.Entity] = e
		}
		e.last = wall
		t := lfTake{e: e}
		if l.RPS > 0 {
			masks[i] |= 1
			burst := float64(l.Burst)
			if burst <= 0 {
				burst = float64(l.RPS)
			}
			e.rpsCap, e.rpsRate = burst, float64(l.RPS)
			e.rps.refill(now, burst/n, float64(l.RPS)/n)
			ok, rem, reset := e.rps.take(1)
			if !ok {
				lf.undo(takes)
				lf.mu.Unlock()
				return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "rps", RetryAfter: maxDur(reset, 100*time.Millisecond), Message: fmt.Sprintf("rate limit of %d requests per second exceeded", l.RPS), State: core.RateLimitState{Present: true, RequestsLimit: l.RPS, RequestsRemaining: int(rem * n), RequestsReset: reset}}, nil
			}
			e.usedRPS++
			t.rps = true
		}
		if l.RPM > 0 {
			masks[i] |= 2
			e.rpmCap, e.rpmRate = float64(l.RPM), float64(l.RPM)/60
			e.rpm.refill(now, float64(l.RPM)/n, float64(l.RPM)/60/n)
			ok, rem, reset := e.rpm.take(1)
			st = mergeUnits(st, core.RateLimitState{Present: true, RequestsLimit: l.RPM, RequestsRemaining: min(l.RPM, int(rem*n)), RequestsReset: reset})
			if !ok {
				if t.rps {
					e.rps.give(1)
					e.usedRPS--
				}
				lf.undo(takes)
				lf.mu.Unlock()
				// The token units live in Valkey: read them so the 429
				// carries the token headers too (task 064).
				lf.v.peekTokens(ctx, s, now, &st)
				return Decision{Allowed: false, Kind: core.ErrRateLimit, Entity: s.Entity, LimitName: "rpm", RetryAfter: maxDur(time.Duration(60.0/float64(l.RPM)*float64(time.Second)), time.Second), Message: fmt.Sprintf("rate limit of %d requests per minute exceeded", l.RPM), State: sumTokens(st)}, nil
			}
			e.usedRPM++
			t.rpm = true
		}
		takes = append(takes, t)
	}
	if !remote {
		for _, t := range takes {
			t.e.reqs++
		}
		lf.mu.Unlock()
		return Decision{Allowed: true, State: st}, nil
	}
	lf.mu.Unlock()
	d, err := lf.v.checkRemote(ctx, req, now, masks)
	if err != nil || !d.Allowed {
		lf.mu.Lock()
		lf.undo(takes)
		lf.mu.Unlock()
		return d, err
	}
	if st.Present {
		// The requests unit counted here folds in like the remote levels.
		d.State = mergeUnits(d.State, core.RateLimitState{Present: true, RequestsLimit: st.RequestsLimit, RequestsRemaining: st.RequestsRemaining, RequestsReset: st.RequestsReset})
	}
	return d, nil
}

type lfSync struct {
	e      Entity
	le     *lfEnt
	rps    float64
	rpm    float64
	reqs   int64
	rpsCap float64
	rpmCap float64
}

// sync sends sync_local for every entity used since the last round and
// adjusts the local buckets to min(cap/N, global/N).
func (lf *localFirst) sync(ctx context.Context) {
	now := time.Now()
	lf.mu.Lock()
	var batch []lfSync
	for e, le := range lf.ents {
		// Entities touched since the last round are synced even when they
		// consumed nothing (denied), to learn bans and blocks.
		if le.usedRPS == 0 && le.usedRPM == 0 && le.reqs == 0 && le.last.Before(lf.lastSync) {
			if now.Sub(le.last) > time.Minute {
				delete(lf.ents, e)
			}
			continue
		}
		batch = append(batch, lfSync{e: e, le: le, rps: le.usedRPS, rpm: le.usedRPM, reqs: le.reqs, rpsCap: le.rpsCap, rpmCap: le.rpmCap})
		le.usedRPS, le.usedRPM, le.reqs = 0, 0, 0
	}
	lf.lastSync = now
	rates := make([][2]float64, len(batch))
	for i, b := range batch {
		rates[i] = [2]float64{b.le.rpsRate, b.le.rpmRate}
	}
	lf.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	v := lf.v
	f := func(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }
	calls := make([]call, len(batch))
	for i, b := range batch {
		calls[i] = scriptCall(luaSyncLocal, []string{v.key("ent", b.e), v.key("tb", b.e)},
			[]string{ms(now), f(b.rps), f(b.rpm), itoa(b.reqs), f(b.rpsCap), f(rates[i][0]), f(b.rpmCap), f(rates[i][1]), string(b.e.Kind), b.e.ID})
	}
	cctx, cancel := context.WithTimeout(ctx, max(v.timeout, 50*time.Millisecond)*4)
	defer cancel()
	res := v.pipe(cctx, calls...)
	n := float64(lf.n.Load())
	var created []call
	lf.mu.Lock()
	for i, b := range batch {
		arr, err := res[i].ToArray()
		if err != nil || len(arr) < 6 {
			// Keep the consumption for the next round.
			b.le.usedRPS += b.rps
			b.le.usedRPM += b.rpm
			b.le.reqs += b.reqs
			continue
		}
		if g := i64(arr[0]); g >= 0 && b.rpsCap > 0 {
			b.le.rps.tokens = math.Min(b.le.rps.tokens, float64(g)/n)
		}
		if g := i64(arr[1]); g >= 0 && b.rpmCap > 0 {
			b.le.rpm.tokens = math.Min(b.le.rpm.tokens, float64(g)/n)
		}
		switch i64(arr[2]) {
		case 2:
			v.cache.put(b.e, true, str(arr[5]), time.Time{})
		case 1:
			var until time.Time
			if u := i64(arr[3]); u > 0 {
				until = time.UnixMilli(u)
			}
			v.cache.put(b.e, false, str(arr[5]), until)
		default:
			v.cache.clear(b.e, true)
			v.cache.clear(b.e, false)
		}
		if i64(arr[4]) == 1 {
			created = append(created, cmdCall(v.idxEnt(), "ZADD", v.idxEnt(), "NX", ms(now), member(b.e)))
		}
	}
	lf.mu.Unlock()
	if len(created) > 0 {
		_ = firstErr(v.pipe(cctx, created...))
	}
}
