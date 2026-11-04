package engine

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Catalog and effective price of a request (ADR 0072, items 15 and 16;
// spec 005, optional interface (c)).

// catalogOf is the catalog the route of in serves (driver.CatalogRouter);
// "" when the driver has none.
func catalogOf(in Input) string {
	return driver.RouteCatalog(in.Driver, driver.Route{Method: in.Method, Path: in.Path, Kind: in.Kind})
}

// modelsIn is the driver's catalog (Models) restricted to the models of
// catalog cat, each the model effective in cat (config.ModelConfig.
// ForCatalog: its limits, capabilities, extra and price) and carrying cat,
// so the driver lists that catalog. Without a catalog it is Models.
func (e *Engine) modelsIn(protocol, cat string) []driver.ModelInfo {
	models := e.Models(protocol)
	if cat == "" {
		return models
	}
	out := models[:0]
	for _, m := range models {
		if m.Cfg.InCatalog(cat) {
			m.Cfg, m.Catalog = m.Cfg.ForCatalog(cat), cat
			out = append(out, m)
		}
	}
	return out
}

// pricedModel is the model effective in catalog cat (ForCatalog) with the
// price of a request that starts at at with contextTokens input tokens
// (cache included): its pricing (the catalog's, else the model's) with
// the first matching override and the matching multipliers applied
// (config.Pricing.Effective). The effective model itself when its pricing
// is flat, so a model without catalogs, multipliers or overrides costs
// what it always did and nothing is copied; otherwise a shallow copy whose
// Pricing is the effective one, which every cost of the request
// (usage.cost, debit, spend, list_cost) reads. A model already effective
// in cat is passed with cat "" (ForCatalog of it is itself).
func pricedModel(m *config.ModelConfig, cat string, at time.Time, contextTokens int) *config.ModelConfig {
	if m == nil {
		return nil
	}
	eff := m.ForCatalog(cat)
	p := eff.Pricing
	if len(p.Multipliers) == 0 && len(p.Overrides) == 0 {
		return eff
	}
	c := *eff
	c.Pricing = p.Effective(at, contextTokens)
	return &c
}

// Billing origin and subjects (ADR 0072, items 4, 6, 7 and 10–14; spec
// 010 "Origem da cobrança no opencode"; spec 005, optional interface (d)).

// billingDecider is the driver's interface (d), if it has one.
func billingDecider(drv driver.Driver) (driver.BillingDecider, bool) {
	return driver.As[driver.BillingDecider](drv)
}

// anonymous reports whether cred is a credential without an account the
// engine honours: marked Anonymous by a driver that decides the billing
// (only such a driver can refuse it where the provider does).
func anonymous(drv driver.Driver, cred driver.Credential) bool {
	if !cred.Anonymous {
		return false
	}
	_, ok := billingDecider(drv)
	return ok
}

// billingQuery is what the driver decides the billing of a request on:
// the route and its catalog, the request, the credential, the key, its
// account and the effective tier with its plan, and the effective model
// (nil on the auxiliary routes).
func (e *Engine) billingQuery(cfg *config.Config, in Input, cat string, req *core.Request, cred driver.Credential, key *config.KeyConfig, model *config.ModelConfig) driver.BillingQuery {
	q := driver.BillingQuery{
		Route: driver.Route{Method: in.Method, Path: in.Path, Kind: in.Kind}, Catalog: cat,
		Request: req, Credential: cred, Key: key, Model: model,
	}
	if key != nil {
		acct := cfg.LimitsFor(key, e.accountTier(key)).Account
		q.AccountID, q.Tier = acct.ID, acct.Tier
		q.Account = cfg.FindAccount(acct.ID)
		q.Plan = cfg.Tiers[acct.Tier].Plan
	}
	return q
}

// withoutDollars is l without its dollar limits (spend, budgets, credits
// and quota); the count limits, the modes and how tokens count stay.
func withoutDollars(l config.Limits) config.Limits {
	l.SpendMonthlyUSD, l.SpendSoftUSD, l.SpendMonthlyModelsUSD = 0, 0, nil
	l.CreditsUSD, l.QuotaUSD = nil, nil
	return l
}

// modelKeyMatches reports whether k, a key of quota_usd.models or
// spend_monthly_models_usd, names the model id in catalog cat: the bare id
// (every catalog) or <cat>/<id> (only that catalog; config.ModelKeyKnown).
func modelKeyMatches(k, id, cat string) bool {
	return k == id || cat != "" && k == cat+"/"+id
}

// modelIDOfKey is the models[].id a key of quota_usd.models or
// spend_monthly_models_usd names: the key itself when it is an id (ids
// may contain "/"), else the part after <catalog>/.
func modelIDOfKey(cfg *config.Config, k string) string {
	for i := range cfg.Models {
		if cfg.Models[i].ID == k {
			return k
		}
	}
	if _, id, ok := strings.Cut(k, "/"); ok {
		return id
	}
	return k
}

// modeOf is l reduced to its mode (mode, after_n, window): the derived
// entities (model_quota, model_spend) apply when their level does.
func modeOf(l config.Limits) config.Limits {
	return config.Limits{Mode: l.Mode, AfterN: l.AfterN, Window: l.Window}
}

// modelSpendID is the model_spend entity of a level: "<kind>:<id>/<model
// id>" (cache/valkey/README.md).
func modelSpendID(kind limits.EntityKind, id, model string) string {
	return string(kind) + ":" + id + "/" + model
}

// modelSpendSubjects are the budgets of coverage "models" of the request
// (spend_monthly_models_usd): one model_spend entity per level of subs
// (account, project, key) whose map has a key that names model in catalog
// cat, limited by its amount (0 = no limit). A bare id and a <cat>/<id> of
// the same level share the entity; the smaller amount wins.
func modelSpendSubjects(subs []limits.Subject, model *config.ModelConfig, cat string) []limits.Subject {
	var out []limits.Subject
	for _, s := range subs {
		switch s.Entity.Kind {
		case limits.KindAccount, limits.KindProject, limits.KindKey:
		default:
			continue
		}
		amount := 0.0
		for k, v := range s.Limits.SpendMonthlyModelsUSD {
			if v > 0 && modelKeyMatches(k, model.ID, cat) && (amount == 0 || v < amount) {
				amount = v
			}
		}
		if amount == 0 {
			continue
		}
		l := modeOf(s.Limits)
		l.SpendMonthlyUSD = amount
		out = append(out, limits.Subject{Entity: limits.Entity{Kind: limits.KindModelSpend, ID: modelSpendID(s.Entity.Kind, s.Entity.ID, model.ID)}, Limits: l, Pricing: model.Pricing})
	}
	return out
}

// quotaOwner is the owner of the subscription quota of a key's request
// and the quota it counts: the account and its quota_usd; in Go Teams
// (driver.OwnerMember) the member, the key's project or else the key, with
// its own quota_usd when it declares one, else the account's (spec 010).
func quotaOwner(eff config.EffectiveLimits, owner driver.QuotaOwner) (kind limits.EntityKind, id string, quota *config.QuotaUSD) {
	quota = eff.Account.Limits.QuotaUSD
	if owner != driver.OwnerMember {
		return limits.KindAccount, eff.Account.ID, quota
	}
	member := eff.Key
	kind = limits.KindKey
	if eff.Project.ID != "" {
		member, kind = eff.Project, limits.KindProject
	}
	if member.Limits.QuotaUSD != nil {
		quota = member.Limits.QuotaUSD
	}
	return kind, member.ID, quota
}

// modelQuotaID is the model_quota entity of an owner: "<owner id>/<model
// id>" (cache/valkey/README.md).
func modelQuotaID(owner, model string) string { return owner + "/" + model }

// billedSubjects are the subjects of a request (subjects) shaped by the
// driver's billing b (spec 010). nil b (a driver without interface (d))
// keeps every limit of every level, with the budgets of coverage "models"
// added. free: no dollar limit, plus the daily free_models count (the
// account, or "ip:<address>" without a key). balance: credits, spend and
// budgets, no quota. go/go-plus: the quota, whole and per model, on its
// owner only, no credits nor budgets. The count limits hold on every
// source.
func (e *Engine) billedSubjects(cfg *config.Config, drv driver.Driver, key *config.KeyConfig, req *core.Request, model *config.ModelConfig, cat string, b *driver.Billing) []limits.Subject {
	subs := e.subjects(cfg, drv, key, req, model)
	switch {
	case b == nil:
		return append(subs, modelSpendSubjects(subs, model, cat)...)
	case b.Source == driver.BillingFree:
		for i := range subs {
			subs[i].Limits = withoutDollars(subs[i].Limits)
		}
		if b.FreeDailyRequests > 0 {
			id := "ip:" + req.ClientIP
			if key != nil {
				id = cfg.LimitsFor(key, e.accountTier(key)).Account.ID
			}
			if id != "" && id != "ip:" {
				subs = append(subs, limits.Subject{Entity: limits.Entity{Kind: limits.KindFreeModels, ID: id}, Limits: config.Limits{RPD: b.FreeDailyRequests}, Pricing: model.Pricing})
			}
		}
		return subs
	case b.Subscription():
		var ownKind limits.EntityKind
		var ownID string
		var quota *config.QuotaUSD
		if key != nil {
			ownKind, ownID, quota = quotaOwner(cfg.LimitsFor(key, e.accountTier(key)), b.QuotaOwner)
		}
		owner := -1
		for i := range subs {
			subs[i].Limits = withoutDollars(subs[i].Limits)
			if subs[i].Entity.Kind == ownKind && subs[i].Entity.ID == ownID {
				owner = i
			}
		}
		if owner < 0 || quota == nil {
			return subs
		}
		q := *quota
		q.Models = nil
		subs[owner].Limits.QuotaUSD = &q
		amount := 0.0
		for k, v := range quota.Models {
			if v > 0 && modelKeyMatches(k, model.ID, cat) && (amount == 0 || v < amount) {
				amount = v
			}
		}
		if amount > 0 {
			l := modeOf(subs[owner].Limits)
			l.QuotaUSD = &config.QuotaUSD{MonthlyUSD: amount, Rolling5hPct: quota.Rolling5hPct, WeeklyPct: quota.WeeklyPct}
			subs = append(subs, limits.Subject{Entity: limits.Entity{Kind: limits.KindModelQuota, ID: modelQuotaID(ownID, model.ID)}, Limits: l, Pricing: model.Pricing})
		}
		return subs
	default: // balance
		for i := range subs {
			subs[i].Limits.QuotaUSD = nil
		}
		return append(subs, modelSpendSubjects(subs, model, cat)...)
	}
}

// extraUsage reports whether a denial dec of a request billed by b goes on
// as balance (Extra Usage, ADR 0072 item 12): a subscription quota window
// denied it and the account has Extra Usage. The first Check consumed
// nothing, so the engine checks again with the balance subjects.
func extraUsage(b *driver.Billing, dec limits.Decision) bool {
	return b != nil && b.Subscription() && b.ExtraUsage && !dec.Allowed && strings.HasPrefix(dec.LimitName, "quota_")
}

// billedFinish sets the debit of a served request billed by b on fr (spec
// 010): free debits nothing; go/go-plus cost 0 and the quota by the
// allowance (list cost × AllowanceFactor); balance the cost, never the
// quota. Without b the cost debits everything, as before.
func billedFinish(fr *limits.FinishRequest, b *driver.Billing) {
	if b == nil {
		return
	}
	fr.BillingSource = b.Source
	fr.QuotaSet = true
	switch {
	case b.Source == driver.BillingFree:
		fr.CostMicroUSD = 0
	case b.Subscription():
		fr.QuotaMicroUSD = b.Allowance(fr.CostMicroUSD)
		fr.CostMicroUSD = 0
	}
}

// state is the store's view of ent for the billing view (nil on a store
// error), the quota windows included (5 h, week and month; balanceOf).
func (e *Engine) state(ctx context.Context, ent limits.Entity) *driver.Balance {
	st, err := e.Store.Get(ctx, ent)
	if err != nil {
		e.Log.Warn("limits store error", "err", err)
		return nil
	}
	b := balanceOf(st)
	b.Quota5hUSD, b.Quota5hSince = st.Quota5hUSD, st.Quota5hSince
	return b
}

// billingView fills the billing fields of xc for an auxiliary route of a
// driver with interface (d) and a request with a key (OpenCode's usage and
// Console): the account, the plan, the service account flag, the quota
// owner (asked of the driver with no model) with its quota and state, the
// quota of each model and the budgets of every level with their spend.
func (e *Engine) billingView(ctx context.Context, cfg *config.Config, in Input, cat string, req *core.Request, cred driver.Credential, key *config.KeyConfig, xc *driver.ExtraContext) {
	bd, ok := billingDecider(in.Driver)
	if !ok || key == nil {
		return
	}
	q := e.billingQuery(cfg, in, cat, req, cred, key, nil)
	b, _ := bd.Billing(q)
	xc.AccountID, xc.Plan, xc.ServiceAccount = q.AccountID, q.Plan, key.ServiceAccount
	eff := cfg.LimitsFor(key, e.accountTier(key))
	if kind, id, quota := quotaOwner(eff, b.QuotaOwner); quota != nil && id != "" {
		whole := *quota
		whole.Models = nil
		xc.QuotaOwner = &driver.QuotaView{Kind: string(kind), ID: id, Quota: whole, State: e.state(ctx, limits.Entity{Kind: kind, ID: id})}
		for _, k := range slices.Sorted(maps.Keys(quota.Models)) {
			mq := config.QuotaUSD{MonthlyUSD: quota.Models[k], Rolling5hPct: quota.Rolling5hPct, WeeklyPct: quota.WeeklyPct}
			ent := limits.Entity{Kind: limits.KindModelQuota, ID: modelQuotaID(id, modelIDOfKey(cfg, k))}
			xc.ModelQuotas = append(xc.ModelQuotas, driver.ModelQuotaView{Model: k, Quota: mq, State: e.state(ctx, ent)})
		}
	}
	for _, lv := range [...]struct {
		kind limits.EntityKind
		l    config.LevelLimits
	}{{limits.KindAccount, eff.Account}, {limits.KindProject, eff.Project}, {limits.KindKey, eff.Key}} {
		if lv.l.ID == "" {
			continue
		}
		if v := lv.l.Limits.SpendMonthlyUSD; v > 0 {
			xc.Budgets = append(xc.Budgets, driver.BudgetView{Level: string(lv.kind), ID: lv.l.ID, LimitUSD: v, SpentUSD: spendOf(e.state(ctx, limits.Entity{Kind: lv.kind, ID: lv.l.ID}))})
		}
		models := lv.l.Limits.SpendMonthlyModelsUSD
		for _, k := range slices.Sorted(maps.Keys(models)) {
			if models[k] <= 0 {
				continue
			}
			ent := limits.Entity{Kind: limits.KindModelSpend, ID: modelSpendID(lv.kind, lv.l.ID, modelIDOfKey(cfg, k))}
			xc.Budgets = append(xc.Budgets, driver.BudgetView{Level: string(lv.kind), ID: lv.l.ID, Model: k, LimitUSD: models[k], SpentUSD: spendOf(e.state(ctx, ent))})
		}
	}
}

// spendOf is the month's spend of a store view, 0 when it was not read.
func spendOf(b *driver.Balance) float64 {
	if b == nil {
		return 0
	}
	return b.SpendUSD
}
