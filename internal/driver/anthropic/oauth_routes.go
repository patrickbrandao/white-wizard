package anthropic

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// The OAuth and account routes of the subscription (spec 006 16.2.1), on
// the driver's mount; the real serves them on platform.claude.com,
// claude.com and api.anthropic.com. They exist only with oauth_key, share
// driver.KindUsage (the kind of the auxiliary routes the engine serves
// through Extra, which tells them apart by path) and, but for
// /api/claude_cli_profile (an x-api-key), are answered without the
// engine's credential check: the driver verifies its own tokens.
const (
	pathAuthorize     = "/oauth/authorize"
	pathCAIAuthorize  = "/cai/oauth/authorize"
	pathToken         = "/v1/oauth/token" //nolint:gosec // G101: a route, not a credential
	pathRevoke        = "/v1/oauth/token/revoke"
	pathProfile       = "/api/oauth/profile"
	pathCLIProfile    = "/api/claude_cli_profile"
	pathValidate      = "/api/oauth/validate"
	pathRoles         = "/api/oauth/claude_cli/roles"
	pathCreateAPIKey  = "/api/oauth/claude_cli/create_api_key" //nolint:gosec // G101: a route, not a credential
	pathBootstrap     = "/api/claude_cli/bootstrap"
	pathUsage         = "/api/oauth/usage"
	pathOAuthHello    = "/v1/oauth/hello"
	pathFirstTokenDay = "/api/organization/claude_code_first_token_date" //nolint:gosec // G101: a route, not a credential
)

// oauthHelloBody is /v1/oauth/hello's answer: compact, 19 bytes (unlike
// /api/hello; fixture routes/oauth_hello_get).
const oauthHelloBody = `{"message":"hello"}`

// identityDate is the created_at of every account and subscription.
const identityDate = "2026-01-01T00:00:00Z"

// The plans of oauth_plan: organization_type and rate_limit_tier (spec 006
// 16.2.1; the two Max tiers are the client's, the others not conferred).
var plans = map[string][2]string{
	"pro":        {"claude_pro", "default_claude_ai"},
	"max_5x":     {"claude_max", "default_claude_max_5x"},
	"max_20x":    {"claude_max", "default_claude_max_20x"},
	"team":       {"claude_team", "default_claude_ai"},
	"enterprise": {"claude_enterprise", "default_claude_ai"},
}

// planNames are the values of oauth_plan, the default first.
var planNames = []string{"max_5x", "max_20x", "pro", "team", "enterprise"}

// oauthRoutes are the subscription's routes (without oauth_key, none).
func oauthRoutes() []driver.Route {
	rs := []driver.Route{
		{Method: "GET", Path: pathAuthorize},
		{Method: "GET", Path: pathCAIAuthorize},
		{Method: "POST", Path: pathToken},
		{Method: "POST", Path: pathRevoke},
		{Method: "GET", Path: pathProfile},
		{Method: "GET", Path: pathCLIProfile},
		{Method: "POST", Path: pathValidate},
		{Method: "GET", Path: pathRoles},
		{Method: "POST", Path: pathCreateAPIKey},
		{Method: "GET", Path: pathBootstrap},
		{Method: "GET", Path: pathUsage},
	}
	for i := range rs {
		rs[i].Kind = driver.KindUsage
	}
	return rs
}

// bearerPaths are the account routes authenticated by a Bearer access
// token: without oauth_key they are unknown routes, but a Bearer
// sk-ant-oat… there gets the invalid token's 401 (fixture
// routes/oauth_profile_badkey).
var bearerPaths = map[string]bool{
	pathProfile: true, pathValidate: true, pathRoles: true, pathCreateAPIKey: true,
	pathBootstrap: true, pathUsage: true,
}

var _ driver.PublicRouter = (*Driver)(nil)

// PublicRoute: /api/hello (task 449) and every subscription route but
// /api/claude_cli_profile.
func (d *Driver) PublicRoute(route driver.Route) bool {
	return isHello(route) || d.oauthKey != "" && route.Kind == driver.KindUsage && route.Path != pathCLIProfile
}

// serveOAuth answers the subscription's routes (Extra).
func (d *Driver) serveOAuth(w core.ResponseWriter, h driver.Headers, body []byte, ctx driver.ExtraContext) {
	now := ctx.Now
	if now.IsZero() {
		now = time.Now()
	}
	_, path := driver.RouteOf(h)
	switch path {
	case pathAuthorize, pathCAIAuthorize:
		d.authorize(w, driver.Query(h), ctx, now)
	case pathToken:
		d.token(w, body, ctx, now)
	case pathRevoke:
		writeJSON(w, 200, oai.O())
	case pathCLIProfile:
		label, account := "anonymous", "acct-anonymous" // server.auth: none with an unknown key
		if ctx.Key != nil {
			label, account = ctx.Key.Label, ctx.Key.Account
		}
		writeJSON(w, 200, d.profile(label, account))
	case pathBootstrap:
		if bearerOf(h.Get) == "" {
			if ctx.Key == nil {
				d.writeAPIError(w, h, &core.Error{Kind: core.ErrInvalidAPIKey, Message: noKeyMessage(h)})
				return
			}
			writeJSON(w, 200, oai.O()) // x-api-key
			return
		}
		fallthrough
	default:
		d.serveAccount(w, h, path, ctx, now)
	}
}

// noKeyMessage is the engine's text for a request without a key (anth's
// "x-api-key header is required"), else the invalid key's default.
func noKeyMessage(h driver.Headers) string {
	if h.Get("x-api-key") == "" {
		return oai.NoKeyMessage
	}
	return ""
}

// serveAccount answers the account routes of a Bearer access token: an
// invalid or expired token is the inference's 401, a missing scope a 403
// permission_error.
func (d *Driver) serveAccount(w core.ResponseWriter, h driver.Headers, path string, ctx driver.ExtraContext, now time.Time) {
	t, ok, live := d.accessToken(bearerOf(h.Get), ctx.Keys, now)
	switch {
	case !ok:
		d.writeAPIError(w, h, &core.Error{Kind: core.ErrInvalidAPIKey, Message: msgTokenInvalid})
		return
	case !live:
		d.writeAPIError(w, h, &core.Error{Kind: core.ErrInvalidAPIKey, Message: msgTokenExpired})
		return
	}
	need := map[string]string{pathProfile: scopeProfile, pathCreateAPIKey: scopeCreateAPIKey}[path]
	if need != "" && !t.scopes.has(need) {
		d.writeAPIError(w, h, &core.Error{Kind: core.ErrPermission, Message: msgScopePrefix + need})
		return
	}
	k := d.oauthEntry(ctx.Keys) // the token verified: the entry exists
	id := d.identity(k.Label, k.Account)
	switch path {
	case pathProfile:
		writeJSON(w, 200, d.profile(k.Label, k.Account))
	case pathValidate:
		writeJSON(w, 200, oai.O("organization_uuid", id.org, "account_uuid", id.account))
	case pathRoles:
		writeJSON(w, 200, oai.O("organization_role", "admin", "workspace_role", nil, "organization_name", k.Account))
	case pathCreateAPIKey:
		writeJSON(w, 200, oai.O("raw_key", k.Key))
	case pathBootstrap:
		writeJSON(w, 200, oai.O("oauth_account", oai.O(
			"account_uuid", id.account, "account_email", id.email,
			"organization_uuid", id.org, "organization_name", k.Account,
			"organization_type", id.orgType, "organization_rate_limit_tier", id.tier,
			"user_rate_limit_tier", id.tier, "seat_tier", nil,
		)))
	default: // pathUsage
		writeJSON(w, 200, usageBody(driver.NewSubscriptionView(quotaOf(ctx), ctx.AccountBalance, now)))
	}
}

// quotaOf is the quota_usd of the request's account; nil without one.
func quotaOf(ctx driver.ExtraContext) *config.QuotaUSD {
	return ctx.Limits.QuotaUSD
}

// usageBody is /api/oauth/usage: the 5 h and weekly windows in percent
// (0–100, one decimal) with resets_at in ISO 8601 UTC, the same windows as
// the unified headers; no per-model weeks and no Extra Usage.
func usageBody(v driver.SubscriptionView) oai.Obj {
	window := func(q driver.QuotaWindow) oai.Obj {
		return oai.O("utilization", math.Round(q.Utilization()*1000)/10, "resets_at", q.Reset.UTC().Format(time.RFC3339))
	}
	return oai.O(
		"five_hour", window(v.FiveHour),
		"seven_day", window(v.Weekly),
		"seven_day_oauth_apps", nil,
		"seven_day_opus", nil,
		"seven_day_sonnet", nil,
		"extra_usage", oai.O("is_enabled", false, "monthly_limit", nil, "used_credits", nil, "utilization", nil),
	)
}

// identity is who a subscription is: the derived uuids, the email and the
// plan's organization_type and rate_limit_tier.
type identity struct {
	account, org, email, orgType, tier string
}

func (d *Driver) identity(label, account string) identity {
	p := plans[d.oauthPlan]
	return identity{
		account: shaUUID("account\x00" + label),
		org:     shaUUID("organization\x00" + account),
		email:   label + "@wwz.invalid",
		orgType: p[0],
		tier:    p[1],
	}
}

// profile is /api/oauth/profile (and /api/claude_cli_profile) of the key
// labelled label in account.
func (d *Driver) profile(label, account string) oai.Obj {
	id := d.identity(label, account)
	return oai.O(
		"account", oai.O("uuid", id.account, "email", id.email, "display_name", label, "full_name", label, "created_at", identityDate),
		"organization", oai.O(
			"uuid", id.org, "name", account, "organization_type", id.orgType, "rate_limit_tier", id.tier,
			"seat_tier", nil, "has_extra_usage_enabled", false, "billing_type", nil, "subscription_created_at", identityDate,
		),
	)
}

// shaUUID is a version 4 UUID from the SHA-256 of seed.
func shaUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	u := sum[:16]
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	const hex = "0123456789abcdef"
	b := make([]byte, 0, 36)
	for i, c := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			b = append(b, '-')
		}
		b = append(b, hex[c>>4], hex[c&0xF])
	}
	return string(b)
}

// onHold reports whether the account of the oauth_key entry k is blocked
// or banned (keys[].status).
func onHold(k *config.KeyConfig) bool {
	switch k.Status {
	case "banned", "revoked", "deactivated", "suspended":
		return true
	}
	return false
}

// authorize answers GET /oauth/authorize and /cai/oauth/authorize with an
// automatic consent: a code bound to the PKCE challenge, the redirect and
// the client, sent to the local callback (302) or shown as code#state for
// the manual redirect (200 text/plain). A request the real would refuse is
// a 400 text/plain with the reason (not conferred); an account on hold is
// access_denied.
func (d *Driver) authorize(w core.ResponseWriter, q url.Values, ctx driver.ExtraContext, now time.Time) {
	redirect, state := q.Get("redirect_uri"), q.Get("state")
	manual := redirect == manualRedirect
	s, scopesOK := parseScopes(q.Get("scope"))
	switch {
	case q.Get("client_id") != oauthClientID:
		writeText(w, 400, "Invalid client_id")
		return
	case q.Get("response_type") != "code":
		writeText(w, 400, "Unsupported response_type")
		return
	case q.Get("code_challenge") == "":
		writeText(w, 400, "Missing code_challenge")
		return
	case q.Get("code_challenge_method") != "S256":
		writeText(w, 400, "Unsupported code_challenge_method")
		return
	case !manual && !localCallback(redirect):
		writeText(w, 400, "Invalid redirect_uri")
		return
	case !scopesOK:
		writeText(w, 400, "Invalid scope")
		return
	}
	k := d.oauthEntry(ctx.Keys)
	if k == nil || onHold(k) {
		if manual {
			writeText(w, 400, "account_on_hold")
			return
		}
		redirectTo(w, redirect, url.Values{"error": {"access_denied"}, "error_description": {"account_on_hold"}, "state": {state}})
		return
	}
	t := newOAuthToken(s, now, codeTTL)
	t.binding = codeBinding(q.Get("code_challenge"), redirect, oauthClientID)
	code := t.sign(k.Key, "")
	if manual {
		writeText(w, 200, code+"#"+state)
		return
	}
	redirectTo(w, redirect, url.Values{"code": {code}, "state": {state}})
}

// localCallback reports whether redirect is Claude Code's local listener:
// http://localhost:<port>/callback or http://127.0.0.1:<port>/callback.
func localCallback(redirect string) bool {
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "http" || u.Path != "/callback" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return false
	}
	host, port := u.Hostname(), u.Port()
	n, err := strconv.Atoi(port)
	return (host == "localhost" || host == "127.0.0.1") && err == nil && n > 0 && n < 65536
}

// redirectTo is the 302 to redirect with the query q (code, state; or the
// error), in that order.
func redirectTo(w core.ResponseWriter, redirect string, q url.Values) {
	var b strings.Builder
	for _, k := range []string{"code", "error", "error_description", "state"} {
		if v, ok := q[k]; ok {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(k + "=" + url.QueryEscape(v[0]))
		}
	}
	w.Status(302)
	w.Header("Location", redirect+"?"+b.String())
	w.Header("Content-Length", "0")
	_ = w.Flush()
}

// maxTTLSeconds caps the expires_in a client asks for (about 31 years).
const maxTTLSeconds = 1e9

// bearerOf is the credential of an Authorization: Bearer header read with
// get; "" without one.
func bearerOf(get func(string) string) string {
	auth := get("authorization")
	if len(auth) < 7 || !strings.EqualFold(auth[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[7:])
}

// tokenRequest is the token route's JSON body.
type tokenRequest struct {
	GrantType    string   `json:"grant_type"`
	Code         string   `json:"code"`
	RedirectURI  string   `json:"redirect_uri"`
	ClientID     string   `json:"client_id"`
	CodeVerifier string   `json:"code_verifier"`
	RefreshToken string   `json:"refresh_token"`
	Scope        *string  `json:"scope"`
	ExpiresIn    *float64 `json:"expires_in"`
}

// token answers POST /v1/oauth/token (spec 006 16.2.1): the authorization
// code with PKCE S256 and the refresh token, as the OAuth errors of the
// client's parser.
func (d *Driver) token(w core.ResponseWriter, body []byte, ctx driver.ExtraContext, now time.Time) {
	var req tokenRequest
	if json.Unmarshal(body, &req) != nil {
		writeOAuthError(w, 400, "invalid_request", "Invalid request body")
		return
	}
	if req.ClientID != oauthClientID {
		writeOAuthError(w, 401, "invalid_client", "Invalid client_id")
		return
	}
	k := d.oauthEntry(ctx.Keys)
	secret := d.oauthSecret(ctx.Keys)
	var granted scopes
	switch req.GrantType {
	case "authorization_code":
		t, ok := verifyOAuth(secret, "", req.Code, true)
		if !ok || !now.Before(t.expires) || req.CodeVerifier == "" ||
			string(t.binding) != string(codeBinding(pkceChallenge(req.CodeVerifier), req.RedirectURI, req.ClientID)) {
			writeOAuthError(w, 400, "invalid_grant", "Invalid 'code' in request.")
			return
		}
		granted = t.scopes
	case "refresh_token":
		t, ok := verifyOAuth(secret, refreshPrefix, req.RefreshToken, false)
		switch {
		case !ok:
			writeOAuthError(w, 400, "invalid_grant", "Refresh token not found or invalid")
			return
		case !now.Before(t.expires):
			writeOAuthError(w, 400, "invalid_grant", "Refresh token expired")
			return
		}
		granted = t.scopes
		if req.Scope != nil && strings.TrimSpace(*req.Scope) != "" {
			asked, ok := parseScopes(*req.Scope)
			if !ok || asked&^t.scopes != 0 {
				writeOAuthError(w, 400, "invalid_scope", "The requested scope is invalid, unknown, malformed, or exceeds the scope granted by the resource owner.")
				return
			}
			granted = asked
		}
	default:
		writeOAuthError(w, 400, "unsupported_grant_type", "Unsupported grant_type")
		return
	}
	if onHold(k) { // a code or a refresh token verified: the entry exists
		writeOAuthError(w, 400, "invalid_grant", "account_on_hold")
		return
	}
	ttl := accessTTL
	if req.ExpiresIn != nil && *req.ExpiresIn >= 1 {
		// What the client asks for (claude setup-token: a year), up to
		// maxTTL so the expiry fits a Duration.
		ttl = time.Duration(min(*req.ExpiresIn, maxTTLSeconds)) * time.Second
	}
	id := d.identity(k.Label, k.Account)
	writeJSON(w, 200, oai.O(
		"token_type", "Bearer",
		"access_token", newOAuthToken(granted, now, ttl).sign(secret, accessPrefix),
		"expires_in", int64(ttl/time.Second),
		"refresh_token", newOAuthToken(granted, now, refreshTTL).sign(secret, refreshPrefix),
		"refresh_token_expires_in", int64(refreshTTL/time.Second),
		"scope", granted.String(),
		"organization", oai.O("uuid", id.org, "name", k.Account),
		"account", oai.O("uuid", id.account, "email_address", id.email),
	))
}

// writeOAuthError is the token route's {"error", "error_description"}.
func writeOAuthError(w core.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, oai.O("error", code, "error_description", desc))
}

// writeAPIError is an error of the account routes in the API's envelope,
// with request-id (anth.WriteError).
func (d *Driver) writeAPIError(w core.ResponseWriter, h driver.Headers, e *core.Error) {
	req := &core.Request{}
	if m, ok := h.(driver.MapHeaders); ok {
		req.Headers = m
	}
	anth.WriteError(&d.hooks, w, req, e, nil)
}

// writeJSON writes a JSON answer of the subscription's routes, with a
// request-id as every answer of the real's API hosts (the profile's must
// match ^req_[A-Za-z0-9_-]{1,36}$ for the client to trust the plan).
func writeJSON(w core.ResponseWriter, status int, v any) {
	b, _ := oai.Format{RawHTML: true}.Marshal(v)
	w.Status(status)
	w.Header("Content-Type", "application/json")
	w.Header("request-id", anth.NewRequestID())
	write(w, string(b))
	_ = w.Flush()
}

// writeText is a text/plain answer (the authorize page).
func writeText(w core.ResponseWriter, status int, text string) {
	w.Status(status)
	w.Header("Content-Type", "text/plain; charset=utf-8")
	write(w, text)
	_ = w.Flush()
}
