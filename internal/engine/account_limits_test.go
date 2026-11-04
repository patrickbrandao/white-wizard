package engine_test

import (
	"testing"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// Task 189 (ADR 0051, spec 010): every limit of the tier counts on the
// account; projects[].limits and keys[].limits are extra ceilings; the
// limit headers report the level with the smallest remaining.

const acctLimitsBody = `{"model":"wwz-fast","max_tokens":16,"messages":[{"role":"user","content":"x"}]}`

func oaiStatus(t *testing.T, e *engine.Engine, key string) (int, map[string]string) {
	t.Helper()
	w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + key}, acctLimitsBody)
	return w.StatusCode(), w.Headers()
}

func TestAccountRPMSharedByKeys(t *testing.T) {
	e := newEngine(t, fastCfg+`
tiers:
  small: {limits: {rpm: 2}}
keys:
  - {key: k-a1, label: a1, account: acme, tier: small}
  - {key: k-a2, label: a2, account: acme}
  - {key: k-b1, label: b1, account: other, tier: small}
`)
	for i, k := range []string{"k-a1", "k-a2"} {
		if st, h := oaiStatus(t, e, k); st != 200 {
			t.Fatalf("request %d (%s): status %d", i, k, st)
		} else if h["x-ratelimit-limit-requests"] != "2" {
			t.Errorf("%s: x-ratelimit-limit-requests = %q, want the account's 2", k, h["x-ratelimit-limit-requests"])
		}
	}
	st, h := oaiStatus(t, e, "k-a1")
	if st != 429 {
		t.Fatalf("third request on the account: status %d, want 429 (the two keys share the rpm)", st)
	}
	if h["x-ratelimit-remaining-requests"] != "0" {
		t.Errorf("429: x-ratelimit-remaining-requests = %q, want 0", h["x-ratelimit-remaining-requests"])
	}
	if st, _ := oaiStatus(t, e, "k-b1"); st != 200 {
		t.Errorf("another account: status %d, want 200", st)
	}
}

func TestAccountLimitsOverTier(t *testing.T) {
	e := newEngine(t, fastCfg+`
tiers:
  big: {limits: {rpm: 100}}
accounts:
  - {id: acme, tier: big, limits: {rpm: 1}}
keys:
  - {key: k-a1, label: a1, account: acme}
`)
	if st, h := oaiStatus(t, e, "k-a1"); st != 200 || h["x-ratelimit-limit-requests"] != "1" {
		t.Fatalf("first: status %d, limit %q; want 200 and accounts[].limits rpm 1", st, h["x-ratelimit-limit-requests"])
	}
	if st, _ := oaiStatus(t, e, "k-a1"); st != 429 {
		t.Errorf("second: status %d, want 429", st)
	}
}

func TestKeyLimitsAreACeiling(t *testing.T) {
	e := newEngine(t, fastCfg+`
tiers:
  big: {limits: {rpm: 100}}
keys:
  - {key: k-a1, label: a1, account: acme, tier: big, limits: {rpm: 1}}
  - {key: k-a2, label: a2, account: acme}
`)
	st, h := oaiStatus(t, e, "k-a1")
	if st != 200 {
		t.Fatalf("first: status %d", st)
	}
	// The key has 0 left of 1, the account 99 of 100: the key is reported.
	if h["x-ratelimit-limit-requests"] != "1" || h["x-ratelimit-remaining-requests"] != "0" {
		t.Errorf("key ceiling: requests %q/%q, want 1/0", h["x-ratelimit-limit-requests"], h["x-ratelimit-remaining-requests"])
	}
	if st, _ := oaiStatus(t, e, "k-a1"); st != 429 {
		t.Errorf("second on the key: status %d, want 429 by the key's ceiling", st)
	}
	st, h = oaiStatus(t, e, "k-a2")
	if st != 200 {
		t.Fatalf("other key of the account: status %d", st)
	}
	// The ceiling never raises nor lowers the account: a2 sees the account,
	// which counted only the admitted request of a1.
	if h["x-ratelimit-limit-requests"] != "100" || h["x-ratelimit-remaining-requests"] != "98" {
		t.Errorf("account: requests %q/%q, want 100/98", h["x-ratelimit-limit-requests"], h["x-ratelimit-remaining-requests"])
	}
}

func TestProjectLimitsCountOnTheProject(t *testing.T) {
	e := newEngine(t, fastCfg+`
tiers:
  big: {limits: {rpm: 100}}
projects:
  - {id: web, limits: {rpm: 1}}
keys:
  - {key: k-w1, label: w1, account: acme, project: web, tier: big}
  - {key: k-w2, label: w2, account: acme, project: web}
  - {key: k-b1, label: b1, account: acme, project: batch}
`)
	if st, _ := oaiStatus(t, e, "k-w1"); st != 200 {
		t.Fatalf("first on web: status %d", st)
	}
	if st, _ := oaiStatus(t, e, "k-w2"); st != 429 {
		t.Errorf("second on web (other key): status %d, want 429 by the project", st)
	}
	st, h := oaiStatus(t, e, "k-b1")
	if st != 200 {
		t.Fatalf("project without limits: status %d", st)
	}
	if h["x-ratelimit-limit-requests"] != "100" {
		t.Errorf("batch: x-ratelimit-limit-requests = %q, want the account's 100", h["x-ratelimit-limit-requests"])
	}
}

func TestAnthropicHeadersMostRestrictive(t *testing.T) {
	e := newEngine(t, fastCfg+`
tiers:
  big: {limits: {rpm: 50, itpm: 100000, otpm: 1000}}
keys:
  - {key: k-an, label: an, account: acme, tier: big, limits: {rpm: 2}}
`)
	w := call(t, e, "anthropic", "/v1/messages", map[string]string{"x-api-key": "k-an", "anthropic-version": "2023-06-01"}, acctLimitsBody)
	if w.StatusCode() != 200 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	h := w.Headers()
	if h["anthropic-ratelimit-requests-limit"] != "2" || h["anthropic-ratelimit-requests-remaining"] != "1" {
		t.Errorf("requests %q/%q, want the key's 2/1", h["anthropic-ratelimit-requests-limit"], h["anthropic-ratelimit-requests-remaining"])
	}
	if h["anthropic-ratelimit-output-tokens-limit"] != "1000" {
		t.Errorf("output tokens limit %q, want the account's 1000", h["anthropic-ratelimit-output-tokens-limit"])
	}
}
