package driver

import (
	"math"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// CatalogRouter is the optional interface (c) of spec 005 (ADR 0072): the
// driver tells which catalog of models[].catalogs (spec 003) a route
// serves. The engine finds it with As, lists and accepts only the models
// in that catalog and prices the request with the catalog's pricing. ""
// is no catalog: every model of the driver, priced by models[].pricing. A
// driver without the interface has no catalogs.
type CatalogRouter interface {
	// CatalogOf is the catalog of route: Method and Path (relative to the
	// mount prefix) of the request and the Kind it maps to.
	CatalogOf(route Route) string
}

// RouteCatalog is the catalog d serves on route (CatalogRouter); "" when
// d has none.
func RouteCatalog(d Driver, route Route) string {
	if cr, ok := As[CatalogRouter](d); ok {
		return cr.CatalogOf(route)
	}
	return ""
}

// catalogHeader is the request-header slot that carries the catalog the
// engine resolved for the request; like the account slots, the NUL
// prefix keeps clients from sending it.
const catalogHeader = "\x00wwz-catalog"

// SetCatalog stores the catalog of the request in h; "" removes it.
func SetCatalog(h MapHeaders, name string) {
	if h == nil {
		return
	}
	if name == "" {
		delete(h, catalogHeader)
		return
	}
	h[catalogHeader] = name
}

// CatalogFrom reads the catalog SetCatalog stored in h; "" when none.
func CatalogFrom(h Headers) string {
	if h == nil {
		return ""
	}
	return h.Get(catalogHeader)
}

// CatalogOfRequest is CatalogFrom over the headers of req (nil-safe).
func CatalogOfRequest(req *core.Request) string {
	if req == nil {
		return ""
	}
	return req.Header(catalogHeader)
}

// Pricing is the list pricing of the model in the catalog the listing
// serves (models[].catalogs.<catalog>.pricing, else models[].pricing),
// multipliers and overrides included, as the driver shows it.
func (m ModelInfo) Pricing() config.Pricing {
	if m.Cfg == nil {
		return config.Pricing{}
	}
	return m.Cfg.PricingFor(m.Catalog)
}

// The billing sources of interface (d) (ADR 0072, item 7; spec 010
// "Origem da cobrança no opencode").
const (
	// BillingFree: a free model; no dollar limit, nothing is debited, only
	// the daily free-models count (FreeDailyRequests) applies.
	BillingFree = "free"
	// BillingBalance: the cost is debited from the balance (credits_usd)
	// and counted in spend_* and the budgets; the quota is not touched.
	BillingBalance = "balance"
	// BillingGo and BillingGoPlus: the cost is 0 and the quota of the
	// QuotaOwner is debited by the allowance (list cost × AllowanceFactor);
	// balance and budgets are not touched.
	BillingGo     = "go"
	BillingGoPlus = "go-plus"
)

// QuotaOwner is the entity the quota of a subscription counts on.
type QuotaOwner uint8

const (
	// OwnerAccount: the account (the workspace) owns the quota.
	OwnerAccount QuotaOwner = iota
	// OwnerMember: the member owns it (OpenCode Go Teams): the key's
	// project, or the key itself without a project.
	OwnerMember
)

// Billing is a driver's decision on where the money of a request comes
// from (interface (d) of spec 005).
type Billing struct {
	// Source is BillingFree, BillingBalance, BillingGo or BillingGoPlus.
	Source string
	// AllowanceFactor scales the list cost into the quota debit of the
	// subscription sources (Allowance).
	AllowanceFactor float64
	// QuotaOwner is who the subscription quota counts on.
	QuotaOwner QuotaOwner
	// ExtraUsage: once the subscription quota runs out, the request is
	// billed to the balance instead of refused.
	ExtraUsage bool
	// FreeDailyRequests is the daily count of free-model requests of the
	// account (or of the IP without one); 0 = no daily count.
	FreeDailyRequests int
}

// Subscription reports whether the source debits a subscription quota.
func (b Billing) Subscription() bool {
	return b.Source == BillingGo || b.Source == BillingGoPlus
}

// Allowance is the quota debit, in micro-dollars, of a request whose list
// cost is listMicroUSD: list × AllowanceFactor, rounded; 0 outside the
// subscription sources. The engine debits it and the driver reports it.
func (b Billing) Allowance(listMicroUSD int64) int64 {
	if !b.Subscription() || listMicroUSD <= 0 || b.AllowanceFactor <= 0 {
		return 0
	}
	return int64(math.Round(float64(listMicroUSD) * b.AllowanceFactor))
}

// BillingQuery is what a BillingDecider decides on.
type BillingQuery struct {
	// Route is the route of the request (method and path relative to the
	// mount prefix) and Catalog the catalog it serves (CatalogRouter).
	Route   Route
	Catalog string
	// Request is the parsed request (body fields and headers); on the
	// auxiliary routes it carries only the headers.
	Request *core.Request
	// Credential is what Authenticate extracted.
	Credential Credential
	// Key is the keys[] entry; nil for an Anonymous credential (and for
	// auth: none without a matching key).
	Key *config.KeyConfig
	// AccountID is the account of the key ("" without a key) and Account
	// its accounts[] entry (nil when the account is implicit).
	AccountID string
	Account   *config.AccountConfig
	// Tier is the effective tier of the account and Plan its
	// tiers.<tier>.plan ("" = the driver's default).
	Tier string
	Plan string
	// Model is the effective model (catalog pricing, overrides and
	// multipliers applied). It is nil on the auxiliary routes (usage,
	// Console), where only QuotaOwner and ExtraUsage are read and a
	// refusal is ignored.
	Model *config.ModelConfig
}

// BillingDecider is the optional interface (d) of spec 005 (ADR 0072): the
// driver says, per request, where the money comes from, and the engine
// builds the subjects and the debit from it (spec 010). A non-nil error is
// a refusal before Check (OpenCode: public with a paid model or on the Go
// base, a free model outside the OpenCode client, plan none on the Go
// base): the engine answers it and nothing is counted in the store. A
// driver without the interface is billed as before: every limit of every
// level, the cost debited from balance, spend and quota.
type BillingDecider interface {
	Billing(q BillingQuery) (Billing, *core.Error)
}

// billingHeader is the request-header slot that carries the Billing the
// engine settled for the request (after Extra Usage), for the driver to
// write the usage; the NUL prefix keeps clients from sending it.
const billingHeader = "\x00wwz-billing"

// SetBilling stores b in h.
func SetBilling(h MapHeaders, b Billing) {
	if h == nil {
		return
	}
	extra := "0"
	if b.ExtraUsage {
		extra = "1"
	}
	h[billingHeader] = strings.Join([]string{
		b.Source,
		strconv.FormatFloat(b.AllowanceFactor, 'g', -1, 64),
		strconv.Itoa(int(b.QuotaOwner)),
		extra,
		strconv.Itoa(b.FreeDailyRequests),
	}, "|")
}

// BillingFrom reads the Billing SetBilling stored in h; false when none.
func BillingFrom(h Headers) (Billing, bool) {
	if h == nil {
		return Billing{}, false
	}
	return parseBilling(h.Get(billingHeader))
}

// BillingOf is BillingFrom over the headers of req (nil-safe).
func BillingOf(req *core.Request) (Billing, bool) {
	if req == nil {
		return Billing{}, false
	}
	return parseBilling(req.Header(billingHeader))
}

func parseBilling(v string) (Billing, bool) {
	f := strings.Split(v, "|")
	if v == "" || len(f) != 5 {
		return Billing{}, false
	}
	b := Billing{Source: f[0], ExtraUsage: f[3] == "1"}
	b.AllowanceFactor, _ = strconv.ParseFloat(f[1], 64)
	if f[2] == strconv.Itoa(int(OwnerMember)) {
		b.QuotaOwner = OwnerMember
	}
	b.FreeDailyRequests, _ = strconv.Atoi(f[4])
	return b, true
}
