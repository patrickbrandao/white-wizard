package opencode

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// The Console (ADR 0072, item 17; spec 004 "OpenCode: modo public e
// Console"): the device-flow login of the official client and the account
// routes it reads, under <prefix>/console (the real is
// https://opencode.ai/console). Nothing is kept in the store: the
// device_code and the tokens are signed with HMAC-SHA256, the secret being
// the value of the keys[] entry drivers[].options.console_key names, so
// every instance with the same configuration accepts the tokens of the
// others and only who knows that key forges one. The access token stands
// for that key on the Console and on inference (ResolveKey). Shapes, times
// and the error answers are the wwz's (not conferred).

// consolePrefix starts every Console path (relative to the mount prefix).
const consolePrefix = "/console/"

// The Console's routes.
const (
	pathDeviceCode = "/console/auth/device/code"
	pathDeviceTok  = "/console/auth/device/token"
	pathUser       = "/console/api/user"
	pathOrgs       = "/console/api/orgs"
	pathConfig     = "/console/api/v2/config"
	pathBudgets    = "/console/api/policies/current-budgets"
)

// Times the wwz chose (ADR 0072, item 17).
const (
	deviceInterval = 5 * time.Second
	deviceTTL      = 900 * time.Second
	accessTTL      = 3600 * time.Second
	refreshTTL     = 30 * 24 * time.Hour
)

// The grant types of the token route.
const (
	grantDevice  = "urn:ietf:params:oauth:grant-type:device_code"
	grantRefresh = "refresh_token"
)

// The kinds of signed token.
const (
	tokDevice  = "device"
	tokAccess  = "access"
	tokRefresh = "refresh"
)

// The Console's error bodies.
var (
	unauthorized = oai.O("_tag", "Unauthorized")
	forbidden    = oai.O("_tag", "Forbidden")
)

// consoleRoutes are the Console's routes. They share driver.KindUsage, the
// kind of the driver's account routes the engine serves through Extra
// (KindResource is the Gemini dialect's to drivertest), and Extra tells
// them apart by path; a wrong method or an unknown path under /console is
// a route miss (the site's HTML 404, WriteRouteMiss).
func consoleRoutes() []driver.Route {
	return []driver.Route{
		{Method: "POST", Path: pathDeviceCode, Kind: driver.KindUsage},
		{Method: "POST", Path: pathDeviceTok, Kind: driver.KindUsage},
		{Method: "GET", Path: pathUser, Kind: driver.KindUsage},
		{Method: "GET", Path: pathOrgs, Kind: driver.KindUsage},
		{Method: "GET", Path: pathConfig, Kind: driver.KindUsage},
		{Method: "GET", Path: pathBudgets, Kind: driver.KindUsage},
	}
}

// isConsolePath reports whether path (relative to the mount prefix) is
// under /console.
func isConsolePath(path string) bool {
	return strings.HasPrefix(path, consolePrefix)
}

var _ driver.PublicRouter = (*Driver)(nil)

// PublicRoute: the device flow is answered without a credential.
func (d *Driver) PublicRoute(route driver.Route) bool {
	return route.Method == "POST" && (route.Path == pathDeviceCode || route.Path == pathDeviceTok)
}

var _ driver.KeyResolver = (*Driver)(nil)

// ResolveKey: an access token signed with the console_key's value and not
// expired stands for that key (on inference too, Messages included).
func (d *Driver) ResolveKey(token string, keys driver.KeyLookup, now time.Time) (string, bool) {
	secret, ok := d.consoleSecret(keys)
	if !ok {
		return "", false
	}
	t, ok := verifyToken(secret, token, tokAccess)
	if !ok || !now.Before(t.expires()) {
		return "", false
	}
	return d.consoleKey, true
}

// consoleSecret is the value of the console_key entry; false without the
// option or the entry.
func (d *Driver) consoleSecret(keys driver.KeyLookup) (string, bool) {
	k := d.consoleEntry(keys)
	if k == nil {
		return "", false
	}
	return k.Key, true
}

// consoleEntry is the keys[] entry of console_key; nil without it.
func (d *Driver) consoleEntry(keys driver.KeyLookup) *config.KeyConfig {
	if d.consoleKey == "" || keys == nil {
		return nil
	}
	return keys(d.consoleKey)
}

// serveConsole answers the Console's routes (Extra).
func (d *Driver) serveConsole(w core.ResponseWriter, h driver.Headers, body []byte, ctx driver.ExtraContext) {
	now := ctx.Now
	if now.IsZero() {
		now = time.Now()
	}
	_, path := driver.RouteOf(h)
	switch path {
	case pathDeviceCode:
		d.deviceCode(w, h, ctx, now)
		return
	case pathDeviceTok:
		d.deviceToken(w, h, body, ctx, now)
		return
	}
	// api/*: a key of keys[] or an access token (the engine resolved it);
	// x-org-id is accepted and ignored.
	if ctx.Key == nil {
		writeConsoleJSON(w, 401, unauthorized)
		return
	}
	account := ctx.AccountID
	if account == "" {
		account = ctx.Key.Account
	}
	switch path {
	case pathUser:
		writeConsoleJSON(w, 200, oai.O("id", "usr_"+hashID("user:"+ctx.Key.Label), "email", ctx.Key.Label+"@wwz.invalid"))
	case pathOrgs:
		writeConsoleJSON(w, 200, []oai.Obj{oai.O("id", workspaceOf(account), "name", account)})
	case pathConfig:
		providers := oai.O("opencode", oai.O())
		if ctx.Plan != planNone {
			providers = append(providers, oai.Field{Key: "opencode-go", Val: oai.O()})
		}
		writeConsoleJSON(w, 200, oai.O("providers", providers))
	case pathBudgets:
		if ctx.ServiceAccount || ctx.Key.ServiceAccount {
			writeConsoleJSON(w, 403, forbidden)
			return
		}
		writeConsoleJSON(w, 200, currentBudgets(ctx.Budgets, now))
	default:
		d.WriteRouteMiss(w, nil, driver.RouteMiss{Method: "GET", Path: path}, nil)
	}
}

// deviceCode answers POST auth/device/code: a signed device_code, the
// user_code derived from it and the verification URL. The body
// ({client_id, supports_org_scope?}) is not checked. Without console_key
// the code is signed with an empty secret and never approved.
func (d *Driver) deviceCode(w core.ResponseWriter, h driver.Headers, ctx driver.ExtraContext, now time.Time) {
	secret, _ := d.consoleSecret(ctx.Keys)
	code := signToken(secret, newToken(tokDevice, now, deviceTTL))
	user := userCode(code)
	writeConsoleJSON(w, 200, oai.O(
		"device_code", code,
		"user_code", user,
		"verification_uri_complete", siteURL(h)+"/console/device?code="+user,
		"expires_in", int(deviceTTL/time.Second),
		"interval", int(deviceInterval/time.Second),
	))
}

// deviceToken answers POST auth/device/token, with a JSON or a form body:
//
//   - the device code grant, by the time since the code was issued: before
//     interval slow_down, until 2 × interval authorization_pending, then the
//     tokens; after expires_in expired_token. Without console_key it stays
//     pending until it expires. A code that does not verify (tampered,
//     signed with another secret) is invalid_grant;
//   - the refresh grant: a valid refresh token gets new tokens; an expired,
//     tampered or foreign one is invalid_grant.
//
// A grant without its token is invalid_request and another grant type
// unsupported_grant_type (RFC 6749; not conferred).
func (d *Driver) deviceToken(w core.ResponseWriter, h driver.Headers, body []byte, ctx driver.ExtraContext, now time.Time) {
	form := consoleForm(h, body)
	secret, approves := d.consoleSecret(ctx.Keys)
	switch form.Get("grant_type") {
	case grantDevice:
		code := form.Get("device_code")
		if code == "" {
			writeOAuthError(w, "invalid_request")
			return
		}
		t, ok := verifyToken(secret, code, tokDevice)
		elapsed := now.Sub(t.issued())
		switch {
		case !ok:
			writeOAuthError(w, "invalid_grant")
		case !now.Before(t.expires()):
			writeOAuthError(w, "expired_token")
		case elapsed < deviceInterval:
			writeOAuthError(w, "slow_down")
		case elapsed < 2*deviceInterval || !approves:
			writeOAuthError(w, "authorization_pending")
		default:
			d.writeTokens(w, secret, ctx, now)
		}
	case grantRefresh:
		tok := form.Get("refresh_token")
		if tok == "" {
			writeOAuthError(w, "invalid_request")
			return
		}
		t, ok := verifyToken(secret, tok, tokRefresh)
		if !approves || !ok || !now.Before(t.expires()) {
			writeOAuthError(w, "invalid_grant")
			return
		}
		d.writeTokens(w, secret, ctx, now)
	default:
		writeOAuthError(w, "unsupported_grant_type")
	}
}

// writeTokens answers a new access and refresh token pair; org_id is the
// workspace of the console_key's account.
func (d *Driver) writeTokens(w core.ResponseWriter, secret string, ctx driver.ExtraContext, now time.Time) {
	account := ""
	if k := d.consoleEntry(ctx.Keys); k != nil {
		account = k.Account
	}
	writeConsoleJSON(w, 200, oai.O(
		"access_token", signToken(secret, newToken(tokAccess, now, accessTTL)),
		"refresh_token", signToken(secret, newToken(tokRefresh, now, refreshTTL)),
		"token_type", "Bearer",
		"expires_in", int(accessTTL/time.Second),
		"org_id", workspaceOf(account),
	))
}

// consoleForm reads the token route's body: JSON when the content type
// says so or the body is an object, else application/x-www-form-urlencoded.
// JSON values that are not strings are left out.
func consoleForm(h driver.Headers, body []byte) url.Values {
	trimmed := strings.TrimSpace(string(body))
	if strings.Contains(h.Get("content-type"), "json") || strings.HasPrefix(trimmed, "{") {
		var m map[string]any
		out := url.Values{}
		if json.Unmarshal(body, &m) == nil {
			for k, v := range m {
				if s, ok := v.(string); ok {
					out.Set(k, s)
				}
			}
		}
		return out
	}
	v, _ := url.ParseQuery(trimmed)
	return v
}

// siteURL is the origin of wwz the server computed (driver.OriginOf, ADR
// 0073), which https://opencode.ai of the real stands for. Outside the
// server it is "": the URL is relative.
func siteURL(h driver.Headers) string { return driver.OriginOf(h) }

// userCodeAlphabet are the 20 consonants RFC 8628 suggests for user codes.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

// userCode is XXXX-XXXX derived from the device code (stateless).
func userCode(deviceCode string) string {
	sum := sha256.Sum256([]byte(deviceCode))
	b := make([]byte, 0, 9)
	for i := range 8 {
		if i == 4 {
			b = append(b, '-')
		}
		b = append(b, userCodeAlphabet[int(sum[i])%len(userCodeAlphabet)])
	}
	return string(b)
}

// consoleToken is the payload of a signed token: its kind, when it was
// issued and when it expires (Unix milliseconds) and a random id, so two
// tokens issued in the same millisecond differ.
type consoleToken struct {
	Typ string `json:"typ"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

func (t consoleToken) issued() time.Time  { return time.UnixMilli(t.Iat) }
func (t consoleToken) expires() time.Time { return time.UnixMilli(t.Exp) }

// newToken is a token of kind typ issued at now that lives ttl.
func newToken(typ string, now time.Time, ttl time.Duration) consoleToken {
	var jti [8]byte
	_, _ = rand.Read(jti[:])
	return consoleToken{Typ: typ, Iat: now.UnixMilli(), Exp: now.Add(ttl).UnixMilli(), Jti: hex.EncodeToString(jti[:])}
}

// b64 is the token's encoding: base64url without padding.
var b64 = base64.RawURLEncoding

// signToken is <payload>.<signature>: the JSON payload and its
// HMAC-SHA256 under secret, both base64url.
func signToken(secret string, t consoleToken) string {
	raw, _ := json.Marshal(t)
	payload := b64.EncodeToString(raw)
	return payload + "." + b64.EncodeToString(tokenMAC(secret, payload))
}

// tokenMAC is the HMAC-SHA256 of the encoded payload under secret.
func tokenMAC(secret, payload string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("wwz-console." + payload))
	return m.Sum(nil)
}

// verifyToken checks the signature of tok under secret and that it is of
// kind typ; it does not look at the expiry.
func verifyToken(secret, tok, typ string) (consoleToken, bool) {
	payload, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return consoleToken{}, false
	}
	got, err := b64.DecodeString(sig)
	if err != nil || !hmac.Equal(got, tokenMAC(secret, payload)) {
		return consoleToken{}, false
	}
	raw, err := b64.DecodeString(payload)
	if err != nil {
		return consoleToken{}, false
	}
	var t consoleToken
	if json.Unmarshal(raw, &t) != nil || t.Typ != typ {
		return consoleToken{}, false
	}
	return t, true
}

// currentBudgets is the list of api/policies/current-budgets: one rule per
// budget that holds for the key (account, project and key levels, as the
// engine read them). scope is workspace, member or key; coverage all for
// spend_monthly_usd and models for spend_monthly_models_usd, whose models
// name the catalogs the key covers (a bare id covers both); picked marks
// the rule with the least left, the one that refuses first; shared the
// workspace's, which every member spends from; resetsAt the 1st of the
// next month, 00:00 UTC. Every field but the amounts is the wwz's reading
// (not conferred).
func currentBudgets(budgets []driver.BudgetView, now time.Time) []oai.Obj {
	out := make([]oai.Obj, 0, len(budgets))
	picked, least := -1, math.Inf(1)
	for i, b := range budgets {
		if left := b.LimitUSD - b.SpentUSD; left < least {
			picked, least = i, left
		}
	}
	reset := nextMonth(now).Format(time.RFC3339)
	for i, b := range budgets {
		scope := budgetScope(b.Level)
		coverage, models := "all", []string{}
		if b.Model != "" {
			coverage, models = "models", budgetModels(b.Model)
		}
		out = append(out, oai.O(
			"ruleID", "rul_"+hashID(b.Level+"|"+b.ID+"|"+b.Model),
			"limitDollars", b.LimitUSD,
			"spentDollars", b.SpentUSD,
			"scope", scope,
			"per", "month",
			"coverage", coverage,
			"models", models,
			"vendors", []string{},
			"picked", i == picked,
			"shared", scope == "workspace",
			"resetsAt", reset,
		))
	}
	return out
}

// budgetScope is the Console's scope of a budget level.
func budgetScope(level string) string {
	switch level {
	case "account":
		return "workspace"
	case "project":
		return "member"
	}
	return "key"
}

// budgetModels are the models of a spend_monthly_models_usd key:
// opencode/<id> or opencode-go/<id> as declared, a bare id in both
// catalogs.
func budgetModels(k string) []string {
	if strings.HasPrefix(k, catalogPAYG+"/") || strings.HasPrefix(k, catalogGo+"/") {
		return []string{k}
	}
	return []string{catalogPAYG + "/" + k, catalogGo + "/" + k}
}

// writeOAuthError is the token route's 400 {"error": code}.
func writeOAuthError(w core.ResponseWriter, code string) {
	writeConsoleJSON(w, 400, oai.O("error", code))
}

// writeConsoleJSON writes a Console answer.
func writeConsoleJSON(w core.ResponseWriter, status int, v any) {
	b, _ := jsonFormat.Marshal(v)
	w.Status(status)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
	_ = w.Flush()
}
