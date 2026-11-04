package openrouter

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// The key and account routes (KindKeyInfo), recorded against the real API
// on 2026-10-31 (testdata/real/openrouter/key/, task 193): the key info and
// its legacy alias, the account credits, and the key-management API, which
// refuses an inference key.
const (
	keyPath     = "/api/v1/key"
	authKeyPath = "/api/v1/auth/key"
	creditsPath = "/api/v1/credits" //nolint:gosec // G101: a route, not a credential
	keysPath    = "/api/v1/keys"
)

// keyCacheControl is the Cache-Control of every answer of the key routes,
// 401 included; POST /api/v1/keys answers without one.
const keyCacheControl = "private, no-store"

// keyRouteCacheControl reports whether the answer of the route carries
// keyCacheControl.
func keyRouteCacheControl(method, path string) bool {
	switch path {
	case keyPath, authKeyPath, creditsPath:
		return true
	case keysPath:
		return method != "POST"
	}
	return false
}

// allowOrigin is the Access-Control-Allow-Origin of every answer of the
// driver, errors included (ADR 0061, fixtures of every route).
const allowOrigin = "*"

// freeModelDailyLimit is the daily cap on :free models: 50, or 1000 once
// the account bought at least US$ 10 of credits (spec 006 16.4).
func freeModelDailyLimit(purchased float64) int {
	if purchased >= 10 {
		return 1000
	}
	return 50
}

// freeModelsPerMinute is the per-minute cap on :free models, per account
// (spec 006 16.4).
const freeModelsPerMinute = 20

// FreeModelLimits are the account's caps on requests to :free models given
// the credits it got (configured credits_usd plus purchases): 20 a minute
// and 50 or 1000 a day (spec 006 16.4, ADR 0060; not conferred against the
// real API, whose free counters a test cannot drain without spending the
// account's real allowance).
func (d *Driver) FreeModelLimits(creditsUSD float64) (rpm, rpd int) {
	return freeModelsPerMinute, freeModelDailyLimit(creditsUSD)
}

// Extra answers the key routes. The engine stores the route in the headers
// (driver.SetRoute); without one it is the key info.
func (d *Driver) Extra(kind driver.Kind, w core.ResponseWriter, h driver.Headers, _ []byte, ctx driver.ExtraContext) {
	if kind != driver.KindKeyInfo {
		d.WriteError(w, nil, &core.Error{Kind: core.ErrNotFound, Message: "Not found"}, nil)
		return
	}
	method, path := driver.RouteOf(h)
	var hdr []string
	if path == "" || keyRouteCacheControl(method, path) {
		hdr = append(hdr, "Cache-Control", keyCacheControl)
	}
	switch path {
	case keysPath:
		// The key-management API needs a provisioning key; the simulator
		// has none, so every key is an inference key.
		writeRaw(w, 401, `{"error":{"message":"Invalid API key","code":401}}`, hdr...)
	case creditsPath:
		total, used := accountCredits(ctx)
		writeObj(w, 200, oai.O("data", oai.O("total_credits", total, "total_usage", used)), hdr...)
	default:
		writeObj(w, 200, oai.O("data", keyInfo(h, ctx)), hdr...)
	}
}

// accountCredits is what GET /api/v1/credits reports: every credit the
// account got (the configured credits_usd plus the purchases) and its
// accumulated spend (spent of the entity, task 258).
func accountCredits(ctx driver.ExtraContext) (total, used float64) {
	var bal driver.Balance
	if ctx.AccountBalance != nil {
		bal = *ctx.AccountBalance
	}
	total = bal.PurchasedUSD
	if ctx.Limits.CreditsUSD != nil {
		total += *ctx.Limits.CreditsUSD
	}
	return total, bal.SpentTotalUSD
}

// freeRequestsUsed is free_model_daily_requests.used: the :free requests
// of the UTC day of the account, whose cap it is (every key has an
// account: keys[].account or acct-<label>).
func freeRequestsUsed(ctx driver.ExtraContext) int64 {
	if ctx.AccountBalance == nil {
		return 0
	}
	return ctx.AccountBalance.FreeRequestsToday
}

// keyInfo is the data of GET /api/v1/key, members in the real order. limit
// is the key's own ceiling (keys[].limits.credits_usd, else
// spend_monthly_usd with limit_reset "monthly"), null without one; the
// account balance is not in it. usage is the key's accumulated spend and
// usage_daily/weekly/monthly its spend of the UTC day, week (from Monday)
// and month (task 258).
func keyInfo(h driver.Headers, ctx driver.ExtraContext) oai.Obj {
	var kb driver.Balance
	if ctx.KeyBalance != nil {
		kb = *ctx.KeyBalance
	}
	var limit, reset, remaining any
	var expires any
	if k := ctx.Key; k != nil {
		if k.ExpiresAt != "" {
			expires = k.ExpiresAt
		}
		if l := k.Limits; l != nil {
			switch {
			case l.CreditsUSD != nil:
				limit, remaining = *l.CreditsUSD, *l.CreditsUSD
				if kb.CreditsUSD != nil {
					remaining = max(0, *kb.CreditsUSD)
				}
			case l.SpendMonthlyUSD > 0:
				limit, reset, remaining = l.SpendMonthlyUSD, "monthly", max(0, l.SpendMonthlyUSD-kb.SpendUSD)
			}
		}
	}
	total, _ := accountCredits(ctx)
	daily := int64(freeModelDailyLimit(total))
	used := freeRequestsUsed(ctx)
	account := ""
	if ctx.Key != nil {
		account = ctx.Key.Account
	}
	return oai.O(
		"label", maskKey(presentedKeyOf(h, ctx)),
		"is_management_key", false,
		"is_provisioning_key", false,
		"limit", limit,
		"limit_reset", reset,
		"limit_remaining", remaining,
		"include_byok_in_limit", false,
		"budgets", []any{},
		"effective_budget", nil,
		"usage", kb.SpentTotalUSD,
		"usage_daily", kb.SpendDailyUSD,
		"usage_weekly", kb.SpendWeeklyUSD,
		"usage_monthly", kb.SpendUSD,
		"byok_usage", 0,
		"byok_usage_daily", 0,
		"byok_usage_weekly", 0,
		"byok_usage_monthly", 0,
		"is_free_tier", total <= 0,
		"expires_at", expires,
		"creator_user_id", "user_wwz",
		"organization_id", nil,
		"workspace_id", workspaceID(account),
		"allowed_data_regions", []any{"global"},
		"free_model_daily_requests", oai.O("used", used, "limit", daily, "remaining", max(0, daily-used)),
		"rate_limit", oai.O("requests", -1, "interval", "10s", "note", "This field is deprecated and safe to ignore."),
	)
}

// presentedKeyOf is the key of the request: the one the engine resolved,
// else the configured one.
func presentedKeyOf(h driver.Headers, ctx driver.ExtraContext) string {
	if k := driver.AccountOf(&core.Request{Headers: headerMap(h)}).Key; k != "" {
		return k
	}
	if ctx.Key != nil {
		return ctx.Key.Key
	}
	return ""
}

// headerMap is h as a map when it is one (MapHeaders), else nil.
func headerMap(h driver.Headers) map[string]string {
	if m, ok := h.(driver.MapHeaders); ok {
		return m
	}
	return nil
}

// maskKey is the label of a key: its first 12 characters (sk-or-v1- and
// three more), "..." and its last three, as the real API shows it. A key
// too short for that keeps only its first three characters.
func maskKey(k string) string {
	switch {
	case len(k) >= 18:
		return k[:12] + "..." + k[len(k)-3:]
	case len(k) > 3:
		return k[:3] + "..."
	}
	return "..."
}

// workspaceID is a stable UUID-shaped id of the account's workspace.
func workspaceID(account string) string {
	sum := sha256.Sum256([]byte("wwz-workspace:" + account))
	x := hex.EncodeToString(sum[:16])
	return x[:8] + "-" + x[8:12] + "-4" + x[13:16] + "-8" + x[17:20] + "-" + x[20:32]
}

// keyErrorHeaders adds the Cache-Control
// of the account routes to the headers of an error answered on one of them.
func keyErrorHeaders(req *core.Request, hdr map[string]string) {
	method, path := driver.RouteOfRequest(req)
	if keyRouteCacheControl(method, path) {
		hdr["Cache-Control"] = keyCacheControl
	}
}
