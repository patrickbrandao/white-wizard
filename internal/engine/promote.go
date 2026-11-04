package engine

import (
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// EnablePromotion makes the engine count every key on the tier the cluster
// policies of pol assign to its account (ADR 0051) and evaluate
// tiers.*.promote_when after each request, writing promotions through the
// store when it is a limits.TierSwapper. It registers
// wwz_tier_promotions_total{from, to}. Call it once, before serving; pol
// may be nil (no cluster policies: only the promotion, with the
// configured tiers as origin).
func (e *Engine) EnablePromotion(pol *limits.PolicyApplier) *limits.Promoter {
	p := &limits.Promoter{Policies: pol, Config: e.Config, Log: e.Log}
	if ts, ok := e.Store.(limits.TierSwapper); ok {
		p.Store = ts
	}
	if e.M != nil && e.M.Registry != nil {
		c := e.M.Registry.NewCounter("wwz_tier_promotions_total", "Automatic tier promotions written by this instance (promote_when, ADR 0051)", "from", "to")
		p.OnPromote = func(from, to string) { c.Inc(from, to) }
	}
	e.promoter = p
	return p
}

// Promoter is the promoter of EnablePromotion (nil when off); the admin
// runs it after a credit purchase.
func (e *Engine) Promoter() *limits.Promoter { return e.promoter }

// accountTier is the tier the cluster assigns to the account of key, the
// accountTier of Config.LimitsFor ("" = the configured one).
func (e *Engine) accountTier(key *config.KeyConfig) string {
	if e.promoter == nil || key == nil || key.Account == "" {
		return ""
	}
	return e.promoter.Policies.AccountTier(key.Account)
}

// onUsage is FinishRequest.OnUsage: nil (no history read back) when the
// promotion is off.
func (e *Engine) onUsage() func(limits.Entity, config.PromotionUsage) {
	if e.promoter == nil || e.promoter.Store == nil {
		return nil
	}
	return e.promoter.Observe
}
