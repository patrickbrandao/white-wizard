package opencode

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Catalogs, billing and the gate of the free models (ADR 0072, items 3–14;
// spec 006 16.3 "Detalhes para o driver"; spec 010 "Origem da cobrança no
// opencode"). The engine asks the driver through the optional interfaces
// (c) CatalogRouter and (d) BillingDecider of spec 005.

// The two catalogs of models[].catalogs: the pay-as-you-go bases
// (/inference, /zen) and the Go ones (/inference/go, /zen/go).
const (
	catalogPAYG = "opencode"
	catalogGo   = "opencode-go"
)

// The plans of tiers.<name>.plan; "" is planGo.
const (
	planGo      = "go"
	planGoPlus  = "go-plus"
	planGoTeams = "go-teams"
	planNone    = "none"
)

// publicKey is the credential of the free mode without an account (ADR
// 0072, item 4): never looked up in keys[].
const publicKey = "public"

// defaultFreePerDay is drivers[].options.free_requests_per_day when unset:
// the real publishes no number, 200 is the wwz's choice.
const defaultFreePerDay = 200

// Texts of the gateway's own refusals.
const (
	// freeUsageMessage is the 429 FreeUsageLimitError (conferred,
	// free/public_generic_client.json and public_messages.json).
	freeUsageMessage = "Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"
	// missingAPIKey is the 401 AuthError of a credential absent on the
	// pay-as-you-go bases (conferred, free/public_paid_model.json).
	missingAPIKey = "Missing API key." //nolint:gosec // G101: an error text, not a credential
	// noGoMessage is the 403 of a workspace without Go (ADR 0072, item 9;
	// not conferred).
	noGoMessage = "This workspace does not have an active OpenCode Go subscription"
	// suspendedMessage is the block of a workspace (ADR 0072, item 9; not
	// conferred).
	suspendedMessage = "This workspace has been suspended"
)

// The gateway's own error types the refusals use.
const (
	typeFreeUsage = "FreeUsageLimitError"
	typeForbidden = "ForbiddenError"
	typeAuth      = "AuthError"
)

// extraFreeModels marks the daily count of the free models (the engine's
// denial of the free_models entity, and the gate of a client outside
// OpenCode): the 429 FreeUsageLimitError.
const extraFreeModels = "free_models"

var _ driver.CatalogRouter = (*Driver)(nil)

// CatalogOf: /inference/go and /zen/go serve the Go catalog, /inference
// and /zen the pay-as-you-go one (ADR 0072, item 16).
func (d *Driver) CatalogOf(route driver.Route) string {
	return catalogOfPath(route.Path)
}

// catalogOfPath is the catalog of a path relative to the mount prefix; ""
// outside the four bases.
func catalogOfPath(path string) string {
	for _, b := range []string{"/inference/go/", "/zen/go/"} {
		if strings.HasPrefix(path, b) {
			return catalogGo
		}
	}
	for _, b := range []string{"/inference/", "/zen/"} {
		if strings.HasPrefix(path, b) {
			return catalogPAYG
		}
	}
	return ""
}

var _ driver.BillingDecider = (*Driver)(nil)

// Billing decides where the money of a request comes from, and refuses
// what the real refuses before counting anything, in the order credential
// → client (session, body and model came before, in the engine):
//
//   - public on a Go base, any model, free included: the 401 of the
//     missing credential of Go (conferred, free/public_go_base.json);
//   - public with a paid model on a pay-as-you-go base: 401 AuthError
//     "Missing API key." (conferred, free/public_paid_model.json);
//   - a workspace with plan none on a Go base: 403 ForbiddenError;
//   - a free model outside the OpenCode client (User-Agent not starting
//     with opencode/): the daily free count is zero, so the 429
//     FreeUsageLimitError (conferred, free/public_generic_client.json;
//     "zero" is ADR 0072's reading, not conferred).
//
// The source: a free model is free (the daily count only on the
// pay-as-you-go bases); a pay-as-you-go base is balance; a Go base is go,
// go-plus on that plan, and balance for a service account. The quota
// counts on the member in Go Teams; Extra Usage is the account's, else on
// only in Go Teams. On the auxiliary routes (no model) only QuotaOwner and
// ExtraUsage are read.
func (d *Driver) Billing(q driver.BillingQuery) (driver.Billing, *core.Error) {
	plan := q.Plan
	if plan == "" {
		plan = planGo
	}
	b := driver.Billing{QuotaOwner: driver.OwnerAccount, ExtraUsage: plan == planGoTeams}
	if plan == planGoTeams {
		b.QuotaOwner = driver.OwnerMember
	}
	if q.Account != nil && q.Account.ExtraUsage != nil {
		b.ExtraUsage = *q.Account.ExtraUsage
	}
	cat := q.Catalog
	if cat == "" {
		cat = catalogOfPath(q.Route.Path)
	}
	goBase := cat == catalogGo
	if q.Model == nil {
		b.Source = driver.BillingBalance
		if goBase {
			b.Source = goSource(plan)
		}
		return b, nil
	}
	free := q.Model.PricingFor(q.Catalog).Free()
	switch {
	case q.Credential.Anonymous && (goBase || !free):
		// public stands for a missing credential, which only the free
		// models of the pay-as-you-go bases accept.
		return b, &core.Error{Kind: core.ErrInvalidAPIKey, Message: noKeyMessage}
	case goBase && q.Key != nil && plan == planNone:
		return b, &core.Error{Kind: core.ErrPermission, Status: 403, Code: typeForbidden, Message: noGoMessage}
	case free && !openCodeClient(q.Request):
		return b, freeUsageError(time.Now())
	}
	switch {
	case free:
		b.Source = driver.BillingFree
		if !goBase {
			b.FreeDailyRequests = d.freePerDay
		}
	case !goBase || q.Key != nil && q.Key.ServiceAccount:
		b.Source = driver.BillingBalance
	default:
		b.Source = goSource(plan)
	}
	if b.Subscription() {
		b.AllowanceFactor = defaultAllowance(q.Model.ID)
		if f, ok := q.Model.AllowanceFactor(catalogGo); ok {
			b.AllowanceFactor = f
		}
	}
	return b, nil
}

// goSource is the billing source of a Go base on plan: go-plus on that
// plan, go on the others (go-teams included; not conferred).
func goSource(plan string) string {
	if plan == planGoPlus {
		return driver.BillingGoPlus
	}
	return driver.BillingGo
}

// defaultAllowance is allowance / list_cost without models[].catalogs.
// opencode-go.allowance_factor: 2 on the native Gemini models and 0.6 on
// the others (conferred on deepseek-v4.1-flash and gemini-4-argon).
func defaultAllowance(model string) float64 {
	if nativeGoogle(model) {
		return 2
	}
	return 0.6
}

// openCodeClient reports whether the request comes from the OpenCode
// client: a User-Agent opencode/<channel>/<version>/<app> (ADR 0072, item
// 5). The wwz never sends it to the real provider.
func openCodeClient(req *core.Request) bool {
	return req != nil && strings.HasPrefix(req.Header("user-agent"), "opencode/")
}

// freeUsageError is the 429 FreeUsageLimitError of the gate; retry-after
// runs to the next 00:00 UTC.
func freeUsageError(now time.Time) *core.Error {
	return &core.Error{Kind: core.ErrRateLimit, RetryAfter: untilMidnight(now), Extra: map[string]any{"limit": "rpd", extraFreeModels: true}}
}

// untilMidnight is the time from now to the next 00:00 UTC.
func untilMidnight(now time.Time) time.Duration {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC).Sub(now)
}

// freePerDayOption reads drivers[].options.free_requests_per_day (an
// integer >= 1; the configuration validates it with
// config.OptionPositiveInt).
func freePerDayOption(opts map[string]string) (int, error) {
	v, ok := opts["free_requests_per_day"]
	if !ok {
		return defaultFreePerDay, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("opencode: option free_requests_per_day=%q is invalid (want an integer >= 1)", v)
	}
	return n, nil
}
