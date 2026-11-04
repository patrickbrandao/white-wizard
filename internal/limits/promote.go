package limits

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

// AccountTier is the tier the cluster policies in effect assign to an
// account (ent:account:<id>, else a legacy ent:key:<label> of one of its
// keys; ADR 0051), "" when they assign none: the caller then uses the
// configured tier (Config.AccountTier). It is the accountTier argument of
// Config.LimitsFor and is safe on a nil applier. Lock-free: it reads the
// snapshot Committed took.
func (a *PolicyApplier) AccountTier(account string) string {
	if a == nil {
		return ""
	}
	if m := a.tierSnap.Load(); m != nil {
		return (*m)[account]
	}
	return ""
}

// refreshTiers recomputes the AccountTier snapshot for policies p on the
// effective configuration c (the key → account map of legacy fields).
func (a *PolicyApplier) refreshTiers(p *Policies, c *config.Config) {
	if p == nil || len(p.Entities) == 0 || c == nil {
		a.tierSnap.Store(nil)
		return
	}
	m := p.AccountTiers(c)
	a.tierSnap.Store(&m)
}

// Promoter evaluates promote_when of the tier of an account (ADR 0051 §6)
// after each finish (FinishRequest.OnUsage) and after a credit purchase
// (CreditAdder): when the account's effective tier has promote_when
// satisfied it writes ent:account:<id> = next with TierSwapper.SwapTier
// (compare-and-swap on the field read from the policies in effect), one
// step per evaluation, only up. Each instance tries a (account, tier of
// origin) at most once per version of the policies.
type Promoter struct {
	// Store writes the new tier; nil turns the promotion off.
	Store TierSwapper
	// Policies is the source of the assigned tier, of the expected value of
	// the swap and of the policies version; nil = no cluster policies.
	Policies *PolicyApplier
	// Config returns the effective configuration (tiers, promote_when).
	Config func() *config.Config
	// OnPromote is called once per promotion this instance wrote
	// (wwz_tier_promotions_total{from, to}).
	OnPromote func(from, to string)
	Log       *slog.Logger
	// Timeout bounds one asynchronous promotion; default 5s.
	Timeout time.Duration

	mu    sync.Mutex
	tried map[promoKey]int64 // (account, from) → policies version tried

	// has caches whether a configuration has any promote_when, so requests
	// skip everything else when none does.
	has atomic.Pointer[promoCfg]
	wg  sync.WaitGroup
}

type promoKey struct{ account, from string }

type promoCfg struct {
	cfg *config.Config
	any bool
}

func (p *Promoter) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// anyPromotion reports whether some tier of c has promote_when.
func (p *Promoter) anyPromotion(c *config.Config) bool {
	if h := p.has.Load(); h != nil && h.cfg == c {
		return h.any
	}
	h := &promoCfg{cfg: c}
	for _, t := range c.Tiers {
		if t.PromoteWhen != nil && t.Next != "" {
			h.any = true
			break
		}
	}
	p.has.Store(h)
	return h.any
}

// promotion is one swap to try.
type promotion struct {
	account, from, to, expected string
	version                     int64
}

// due returns the promotion account deserves with usage u, if any and not
// yet tried under the policies in effect; it marks it as tried.
func (p *Promoter) due(account string, u config.PromotionUsage) (promotion, bool) {
	if p == nil || p.Store == nil || p.Config == nil || account == "" {
		return promotion{}, false
	}
	c := p.Config()
	if c == nil || !p.anyPromotion(c) {
		return promotion{}, false
	}
	from := p.Policies.AccountTier(account)
	if from == "" {
		from = c.AccountTier(account)
	}
	to, ok := c.Promotion(from, u)
	if !ok {
		return promotion{}, false
	}
	var version int64
	var expected string
	if p.Policies != nil {
		version = p.Policies.Version()
		expected = p.Policies.EntityTier(Entity{Kind: KindAccount, ID: account})
	}
	k := promoKey{account: account, from: from}
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, seen := p.tried[k]; seen && v == version {
		return promotion{}, false
	}
	if p.tried == nil {
		p.tried = map[promoKey]int64{}
	}
	p.tried[k] = version
	return promotion{account: account, from: from, to: to, expected: expected, version: version}, true
}

// run swaps the tier; a store error forgets the attempt so a later
// evaluation retries it.
func (p *Promoter) run(ctx context.Context, pr promotion) (bool, error) {
	swapped, err := p.Store.SwapTier(ctx, Entity{Kind: KindAccount, ID: pr.account}, pr.expected, pr.to)
	if err != nil {
		p.mu.Lock()
		if v, ok := p.tried[promoKey{pr.account, pr.from}]; ok && v == pr.version {
			delete(p.tried, promoKey{pr.account, pr.from})
		}
		p.mu.Unlock()
		p.log().Warn("tier promotion failed", "account", pr.account, "from", pr.from, "to", pr.to, "err", err)
		return false, err
	}
	if !swapped {
		p.log().Debug("tier promotion lost the race", "account", pr.account, "from", pr.from, "to", pr.to)
		return false, nil
	}
	p.log().Info("tier promoted", "account", pr.account, "from", pr.from, "to", pr.to)
	if p.OnPromote != nil {
		p.OnPromote(pr.from, pr.to)
	}
	return true, nil
}

// Evaluate promotes account synchronously when u satisfies promote_when of
// its effective tier (POST .../credits with add_usd). promoted is false
// when nothing was due, it was already tried under these policies, or
// another writer swapped the field first.
func (p *Promoter) Evaluate(ctx context.Context, account string, u config.PromotionUsage) (promoted bool, err error) {
	pr, ok := p.due(account, u)
	if !ok {
		return false, nil
	}
	return p.run(ctx, pr)
}

// Observe is the FinishRequest.OnUsage hook: it evaluates the account
// subjects on the caller's goroutine (no store call) and, when a promotion
// is due, writes it in the background, off the request path.
func (p *Promoter) Observe(e Entity, u config.PromotionUsage) {
	if e.Kind != KindAccount {
		return
	}
	pr, ok := p.due(e.ID, u)
	if !ok {
		return
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	p.wg.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_, _ = p.run(ctx, pr)
	})
}

// Wait blocks until the background promotions started by Observe end
// (shutdown, tests).
func (p *Promoter) Wait() {
	if p != nil {
		p.wg.Wait()
	}
}
