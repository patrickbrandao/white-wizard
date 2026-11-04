package engine

import (
	"context"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Subscription tokens (ADR 0074; spec 005, interface (c)): the refusals of
// a driver's tokens beyond "invalid" and the quota windows its answers to a
// subscription carry. A driver without driver.TokenChecker or
// driver.SubscriptionReporter costs a type assertion.

// checkToken is the refusal of cred on a route of in.Kind: the driver's
// (driver.TokenChecker) when it has one for cred, else authErr. With
// server.auth: none every credential passes, as before, and a key that is
// a keys[] entry is the engine's alone.
func (e *Engine) checkToken(cfg *config.Config, in Input, cred driver.Credential, authErr *core.Error) *core.Error {
	if cred.Key == "" || cred.Anonymous || cfg.Server.Auth == "none" {
		return authErr
	}
	tc, ok := driver.As[driver.TokenChecker](in.Driver)
	if !ok || cred.KeyLabel == "" && cfg.FindKey(cred.Key) != nil {
		return authErr
	}
	if er := tc.CheckToken(in.Kind, cred, in.Headers, keyLookup(cfg), e.now()); er != nil {
		return er
	}
	return authErr
}

// subscriptionView stores in r's request the quota windows of its account
// (driver.NewSubscriptionView over the account's quota_usd and the store's
// state) when the driver reports a subscription for it; one store read,
// only for those requests.
func (e *Engine) subscriptionView(ctx context.Context, r *request) {
	sr, ok := driver.As[driver.SubscriptionReporter](r.in.Driver)
	if !ok || !sr.Subscription(r.in.Kind, r.cred) {
		return
	}
	var quota *config.QuotaUSD
	var state *driver.Balance
	if r.key != nil {
		acct := r.cfg.LimitsFor(r.key, e.accountTier(r.key)).Account
		if quota = acct.Limits.QuotaUSD; quota != nil && acct.ID != "" {
			state = e.state(ctx, limits.Entity{Kind: limits.KindAccount, ID: acct.ID})
		}
	}
	driver.SetSubscription(r.req.Headers, driver.NewSubscriptionView(quota, state, e.now()))
}
