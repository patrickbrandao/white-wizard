package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// abuseReason prefixes every reason the abuse ladder writes and abuseBy is
// its by. The reason is operator text for the admin API and the panel; the
// client sees the provider's own text (denialError).
const (
	abuseReason = "abuse ladder"
	abuseBy     = "abuse-ladder"
)

// defaultStrikeTTL is the life of the escalation record when the
// configuration has no abuse.strike_ttl (config.Load fills 30 days).
const defaultStrikeTTL = 30 * 24 * time.Hour

// onStrike handles strike number n of the strike_429 window of ent (spec
// 012, abuse policy). The strike that reaches strike_429.count escalates,
// and so does each one after it in the same window. The rung comes from
// the escalation record of the store (limits.Escalator, TTL
// abuse.strike_ttl), which outlives the window, so the ladder keeps
// climbing after a block longer than the window (1 min, 5 min, 30 min,
// 6 h) and only starts over once the record expires; past the end the last
// rung repeats. ban_after_strikes counts the strikes of the record (30 days
// by default) and bans whatever the rung. A store without Escalator, or one
// that fails, falls back to the window: rung strikes - count.
func (e *Engine) onStrike(ctx context.Context, ab config.AbuseConfig, ent limits.Entity, n int) {
	count := max(ab.Strike429.Count, 1)
	escalated := n >= ab.Strike429.Count
	level, strikes := n-count+1, n
	if es, ok := e.Store.(limits.Escalator); ok {
		ttl := ab.StrikeTTL
		if ttl <= 0 {
			ttl = defaultStrikeTTL
		}
		rec, err := es.Escalate(ctx, ent, escalated, ttl)
		if err == nil {
			level, strikes = rec.Level, rec.Strikes
		} else {
			e.Log.Warn("limits store error", "err", err)
		}
	}
	if ab.BanAfterStrikes > 0 && strikes >= ab.BanAfterStrikes {
		_ = e.Store.Ban(ctx, ent, fmt.Sprintf("%s: %d strikes", abuseReason, strikes), abuseBy)
		return
	}
	if escalated {
		e.escalate(ctx, ab, ent, level)
	}
}

// escalate applies rung number level (1 = ladder[0]) of the ladder; past
// the end the last rung repeats. Without a ladder every escalation blocks
// for a minute.
func (e *Engine) escalate(ctx context.Context, ab config.AbuseConfig, ent limits.Entity, level int) {
	if len(ab.Ladder) == 0 {
		_ = e.Store.Block(ctx, ent, abuseReason+": repeated rate limiting", time.Minute, abuseBy)
		return
	}
	step := ab.Ladder[min(max(level-1, 0), len(ab.Ladder)-1)]
	switch step.Action {
	case "ban":
		_ = e.Store.Ban(ctx, ent, abuseReason+": ban", abuseBy)
	case "suspend", "block", "throttle":
		d := step.Duration
		if d == 0 {
			d = time.Minute
		}
		_ = e.Store.Block(ctx, ent, abuseReason+": "+step.Action, d, abuseBy)
	}
}

// balanceOf is the store's view of an entity as the key-info routes read
// it (task 258: spend of the day, week and month, accumulated spend,
// credits, purchases and :free requests of the day) and the quota debit
// of the week and of the month (task 432).
func balanceOf(st limits.EntityState) *driver.Balance {
	return &driver.Balance{
		SpendUSD:          st.SpendUSD,
		SpendDailyUSD:     st.SpendDailyUSD,
		SpendWeeklyUSD:    st.SpendWeeklyUSD,
		SpentTotalUSD:     st.SpentTotalUSD,
		CreditsUSD:        st.Credits,
		PurchasedUSD:      st.PurchasedUSD,
		FreeRequestsToday: st.FreeRequestsToday,
		QuotaWeeklyUSD:    st.QuotaWeeklyUSD,
		QuotaMonthlyUSD:   st.QuotaMonthlyUSD,
	}
}
