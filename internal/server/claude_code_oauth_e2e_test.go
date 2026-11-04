package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/admin"
)

// ccQuotaCfg is the strict mount of spec 020 at the root with the
// subscription of the key "assinante" (spec 006 16.2.1), whose account
// has a quota_usd of $5 in 5 h and $30 a week; every answer costs $1 (the
// 20 output tokens Claude counts for the 8 generated, at $50 000 per
// million). The API key of the captures is on an account of its own.
const ccQuotaCfg = `
server: {auth: keys, seed: 7}
store: {kind: memory}
drivers:
  - {name: anthropic, options: {oauth_key: assinante, client_verification: claude-code}}
accounts:
  - {id: acct-sub, limits: {quota_usd: {monthly_usd: 100, rolling_5h_pct: 5, weekly_pct: 30}}}
  - {id: acct-cc}
keys:
  - {key: wwz-oauth-secret-0001, label: assinante, account: acct-sub}
  - {key: CCKEY, label: cc, account: acct-cc}
profiles:
  latency: {zero: {}}
  generation:
    text: {mode: lorem, output_tokens: {dist: fixed, value: 8}}
models:
  - {id: claude-sonnet-5-5, latency: zero, generation: text, pricing: {input_per_m: 0, output_per_m: 50000}}
  - {id: claude-opus-5-5, latency: zero, generation: text, pricing: {input_per_m: 0, output_per_m: 50000}}
`

const ccAdminToken = "wwz-admin-e2e"

// adminGet returns the body of GET path on the control plane.
func adminGet(t *testing.T, base, path string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("Authorization", "Bearer "+ccAdminToken)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", path, r.StatusCode, b)
	}
	return string(b)
}

// TestClaudeCodeStrictSubscriptionEndToEnd (spec 020, "Verificação
// estrita"; spec 006, 16.2.1; ADR 0074 items 7 and 10): on the strict
// mount with a subscription, every mutation of the spec 020 table sent
// with a token wwz issued gets its 403 and leaves the account and the key
// as they were (GET /admin/entities/account/<id> and key/<label> equal
// before and after) while wwz_client_verification_failures_total{reason}
// counts it; the capture itself is served with the
// anthropic-ratelimit-unified-* headers (and no anthropic-ratelimit-
// requests/tokens), the API key capture and count_tokens without them;
// once the 5 h quota is spent the capture gets the 429 with status
// rejected, and a forged request still gets the 403 first.
func TestClaudeCodeStrictSubscriptionEndToEnd(t *testing.T) {
	fx := strictFixtures(t)
	f002, f004 := parseCC(t, fx["002-sdk-cli-oauth-turn.raw"]), parseCC(t, fx["004-sdk-cli-apikey-turn.raw"])
	f013 := parseCC(t, fx["013-sdk-cli-oauth-count-tokens.raw"])
	ccKey := strings.TrimPrefix(f004.lines[f004.index("x-api-key")], "x-api-key: ")
	eng := newEngine(t, strings.Replace(ccQuotaCfg, "CCKEY", ccKey, 1))
	s, err := New(eng, Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	addr := ln.Addr().String()
	ad, err := admin.New(admin.Options{Engine: eng, Token: ccAdminToken, Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	adm := httptest.NewServer(ad.Handler())
	t.Cleanup(adm.Close)

	token := strictLogin(t, addr)
	tok := f002.withToken(t, token)
	betas := strings.TrimPrefix(f002.lines[f002.index("anthropic-beta")], "anthropic-beta: ")
	const device = "68159782ff84a80342f7e86ed7cddfaaf32e783f7a2a3e29e09814638ebb1f79"
	const session = "ce90e450-43fc-4765-8110-6aed6d650ca1"
	chunked := tok.clone()
	chunked.lines[chunked.index("Content-Length")] = "Transfer-Encoding: chunked"
	chunked.body = strconv.FormatInt(int64(len(f002.body)), 16) + "\r\n" + f002.body + "\r\n0\r\n\r\n"
	swapped := tok.clone()
	i, j := swapped.index("X-Stainless-Arch"), swapped.index("X-Stainless-Lang")
	swapped.lines[i], swapped.lines[j] = swapped.lines[j], swapped.lines[i]
	apiBeta := tok.remove(t, "Authorization")
	apiBeta = apiBeta.insert(apiBeta.index("anthropic-version")+1, "x-api-key: "+ccKey)
	forced := parseCC(t, sdkStrictHeads["python"][0]+sdkStrictHeads["python"][1]).set(t, "Authorization", "Bearer "+token).
		line("/v1/messages ", "/v1/messages?beta=true ")

	// The table "Mutações de teste" of spec 020, on the OAuth capture with
	// the token of the subscription.
	mutations := []struct{ name, raw, reason string }{
		{"without ?beta=true", tok.line("?beta=true", "").String(), "query"},
		{"Transfer-Encoding", strings.Join(chunked.lines, "\r\n") + "\r\n\r\n" + chunked.body, "transfer_encoding"},
		{"without X-Stainless-Timeout", tok.remove(t, "X-Stainless-Timeout").String(), "header_missing"},
		{"Arch swapped with Lang", swapped.String(), "header_order"},
		{"user-agent lowercase", strings.Replace(tok.String(), "\r\nUser-Agent: ", "\r\nuser-agent: ", 1), "header_order"},
		{"X-Forwarded-For", tok.insert(tok.index("Connection"), "X-Forwarded-For: 10.0.0.1").String(), "header_order"},
		{"sdk-py", tok.set(t, "User-Agent", "claude-cli/2.1.308 (external, sdk-py)").String(), "user_agent"},
		{"2.1.309", tok.set(t, "User-Agent", "claude-cli/2.1.309 (external, sdk-cli)").String(), "unknown_version"},
		{"package 0.129.0", tok.set(t, "X-Stainless-Package-Version", "0.129.0").String(), "header_value"},
		{"betas out of order", tok.set(t, "anthropic-beta", strings.Replace(betas, "claude-code-20250219,oauth-2025-04-20", "oauth-2025-04-20,claude-code-20250219", 1)).String(), "beta"},
		{"invented beta", tok.set(t, "anthropic-beta", betas+",wwz-invented-2026-01-01").String(), "beta"},
		{"oauth beta with x-api-key", apiBeta.String(), "beta"},
		{"space after :", tok.edit(t, `"model":`, `"model": `).String(), "body_encoding"},
		{"< escaped", tok.edit(t, "<total_tokens>", "\\"+"u003ctotal_tokens>").String(), "body_encoding"},
		{"stream before model", tok.edit(t, `,"stream":true}`, `}`).edit(t, `{"model":`, `{"stream":true,"model":`).String(), "body_key_order"},
		{"device_id 63 hex", tok.edit(t, device, device[:63]).String(), "metadata"},
		{"session mismatch", tok.edit(t, `\"session_id\":\"`+session, `\"session_id\":\"`+session[:35]+"2").String(), "session_mismatch"},
		{"cch", tok.edit(t, "cc_entrypoint=sdk-cli;", "cc_entrypoint=sdk-cli; cch=00000;").String(), "billing_header"},
		{"cc_version 2.1.307", tok.edit(t, "cc_version=2.1.308.9fe", "cc_version=2.1.307.9fe").String(), "billing_header"},
		{"fingerprint 000", tok.edit(t, "cc_version=2.1.308.9fe", "cc_version=2.1.308.000").String(), "fingerprint"},
		{"cli identity", tok.edit(t, "You are a Claude agent, built on Anthropic's Claude Agent SDK.", "You are Claude Code, Anthropic's official CLI for Claude.").String(), "identity"},
		{"SDK messages.create with the token", parseCC(t, sdkStrictHeads["node"][0]+sdkStrictHeads["node"][1]).set(t, "authorization", "Bearer "+token).String(), "query"},
		{"SDK with ?beta=true forced", forced.String(), "header_missing"},
	}

	account := func() string { return adminGet(t, adm.URL, "/admin/entities/account/acct-sub") }
	key := func() string { return adminGet(t, adm.URL, "/admin/entities/key/assinante") }
	acct0, key0 := account(), key()
	if !strings.Contains(acct0, "acct-sub") {
		t.Fatalf("account entity %s", acct0)
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			before := eng.M.ClientVerificationFailures.Value("anthropic", m.reason)
			checkForged(t, strictDo(t, addr, m.raw), m.reason)
			if got := eng.M.ClientVerificationFailures.Value("anthropic", m.reason); got != before+1 {
				t.Errorf("wwz_client_verification_failures_total{reason=%q} %d -> %d", m.reason, before, got)
			}
			if a, k := account(), key(); a != acct0 || k != key0 {
				t.Errorf("the refusal moved the store:\naccount %s\n     -> %s\nkey %s\n -> %s", acct0, a, key0, k)
			}
		})
	}
	if got := eng.M.ClientVerificationFailures.Total(); got != int64(len(mutations)) {
		t.Errorf("failures total %d, want %d", got, len(mutations))
	}

	// The capture is served with the subscription headers, and spends the
	// quota (the account moves now).
	unifiedOK := func(t *testing.T, a strictAnswer, util string) {
		t.Helper()
		h := a.header
		if a.status != 200 || h["anthropic-ratelimit-unified-status"] != "allowed" || h["anthropic-ratelimit-unified-5h-utilization"] != util ||
			h["anthropic-ratelimit-unified-representative-claim"] != "five_hour" || h["anthropic-ratelimit-unified-5h-reset"] == "" ||
			h["anthropic-ratelimit-unified-7d-utilization"] == "" || h["anthropic-ratelimit-unified-7d-reset"] == "" ||
			h["anthropic-ratelimit-unified-reset"] != h["anthropic-ratelimit-unified-5h-reset"] ||
			h["anthropic-ratelimit-unified-overage-status"] != "rejected" ||
			h["anthropic-ratelimit-unified-overage-disabled-reason"] != "org_level_disabled" {
			t.Errorf("subscriber answer: %d %v %.200s", a.status, h, a.body)
		}
		for name := range h {
			if strings.HasPrefix(name, "anthropic-ratelimit-") && !strings.HasPrefix(name, "anthropic-ratelimit-unified-") {
				t.Errorf("subscriber answer with %s", name)
			}
		}
		if _, ok := h["anthropic-ratelimit-unified-fallback"]; ok {
			t.Error("anthropic-ratelimit-unified-fallback present")
		}
	}
	// The utilization reported is the one before the answer is debited:
	// the first answer sees 0.
	unifiedOK(t, strictDo(t, addr, tok.String()), "0")
	if a := account(); a == acct0 {
		t.Errorf("the served request did not move the account: %s", a)
	}

	// The API key capture (not a token) and count_tokens: no subscription
	// headers.
	for name, raw := range map[string]string{"API key capture": f004.String(), "count_tokens": f013.withToken(t, token).String()} {
		a := strictDo(t, addr, raw)
		if a.status != 200 {
			t.Errorf("%s: %d %s", name, a.status, a.body)
		}
		for h := range a.header {
			if strings.HasPrefix(h, "anthropic-ratelimit-unified-") || (name == "count_tokens" && strings.HasPrefix(h, "anthropic-ratelimit-")) {
				t.Errorf("%s with %s", name, h)
			}
		}
	}

	// More answers spend the $5 of the 5 h window (each capture answer
	// costs more than $1: Claude counts its own output tokens); the first
	// request once the window is spent is the 429 with status rejected.
	var a strictAnswer
	for n := 2; ; n++ {
		a = strictDo(t, addr, tok.String())
		if a.status != 200 || n > 10 {
			break
		}
		if a.header["anthropic-ratelimit-unified-status"] == "" {
			t.Fatalf("answer %d without the subscription headers: %v", n, a.header)
		}
	}
	util, _ := strconv.ParseFloat(a.header["anthropic-ratelimit-unified-5h-utilization"], 64)
	retry, _ := strconv.Atoi(a.header["retry-after"])
	if a.status != 429 || !strings.Contains(a.body, `"type":"rate_limit_error"`) ||
		!strings.Contains(a.body, "This request would exceed your account's rate limit. Please try again later.") ||
		a.header["anthropic-ratelimit-unified-status"] != "rejected" || a.header["anthropic-ratelimit-unified-representative-claim"] != "five_hour" ||
		util < 1 || !strictReqID.MatchString(a.header["request-id"]) || retry < 1 || retry > 5*3600 {
		t.Errorf("over the quota: %d %v %s", a.status, a.header, a.body)
	}
	// The verification still comes first: a forged request is a 403, not
	// the 429, and the spent account is not touched.
	spent := account()
	checkForged(t, strictDo(t, addr, tok.edit(t, "cc_version=2.1.308.9fe", "cc_version=2.1.308.000").String()), "fingerprint")
	if got := account(); got != spent {
		t.Errorf("the refusal moved the spent account:\n%s\n%s", spent, got)
	}
}
