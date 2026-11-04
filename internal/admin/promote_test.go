package admin

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

const promoteCfg = fastCfg + `
keys:
  - {key: wwz-key-01, label: p1, account: acme}
accounts:
  - {id: acme, tier: free}
tiers:
  free: {limits: {rpm: 2}, promote_when: {requests: 2}, next: pro}
  pro: {limits: {rpm: 1000}, promote_when: {purchased_usd: 10}, next: max}
  max: {limits: {rpm: 5000}}
`

// TestPromotion covers ADR 0051 §6 on one instance: the account climbs from
// free to pro after its requests (FinishRequest.OnUsage, off the request
// path) and from pro to max after add_usd purchases, one step each, with
// wwz_tier_promotions_total and the limits of the new tier in effect.
func TestPromotion(t *testing.T) {
	pol := &limits.PolicyApplier{Log: quiet}
	s := newServer(t, promoteCfg, func(o *Options) { o.Override = pol.Override })
	s.OnReload(pol.Committed)
	pol.Current, pol.Apply = s.Config, s.Apply
	p := s.eng.EnablePromotion(pol)
	s.eng.Store.(limits.PolicySource).WatchPolicies(pol.Offer)

	for i := range 2 {
		if w := chat(t, s.eng, nil); w.StatusCode() != 200 {
			t.Fatalf("request %d: %d %s", i, w.StatusCode(), w.Body())
		}
	}
	p.Wait()
	if got := pol.AccountTier("acme"); got != "pro" {
		t.Fatalf("after 2 requests the account tier is %q, want pro", got)
	}
	// The limits of pro count from the next request on.
	if w := chat(t, s.eng, nil); w.Headers()["x-ratelimit-limit-requests"] != "1000" {
		t.Fatalf("pro tier not in effect: %d %v", w.StatusCode(), w.Headers())
	}

	buy := func(body string) limits.EntityState {
		t.Helper()
		r := do(t, s, "POST", "/admin/entities/account/acme/credits", "application/json", body)
		want(t, r, 200)
		var st limits.EntityState
		r.json(t, &st)
		return st
	}
	if st := buy(`{"add_usd": 4}`); st.PurchasedUSD != 4 || st.Credits == nil || *st.Credits != 4 || st.Tier != "pro" {
		t.Fatalf("first purchase: %+v", st)
	}
	if pol.AccountTier("acme") != "pro" {
		t.Fatal("promoted below purchased_usd")
	}
	st := buy(`{"add_usd": 6}`)
	if st.PurchasedUSD != 10 || st.Tier != "max" || pol.AccountTier("acme") != "max" {
		t.Fatalf("second purchase: %+v (tier %q)", st, pol.AccountTier("acme"))
	}
	// usd sets the balance without counting as a purchase.
	if st := buy(`{"usd": 1}`); st.PurchasedUSD != 10 || *st.Credits != 1 {
		t.Fatalf("usd: %+v", st)
	}
	// A purchase on a key is credited to its account (ADR 0058).
	if st := buy2(t, s, "/admin/entities/key/p1/credits", `{"add_usd": 3}`); st.PurchasedUSD != 13 || st.Entity.Kind != limits.KindAccount {
		t.Fatalf("key purchase: %+v", st)
	}

	m := string(do(t, s, "GET", "/metrics", "", "", "").body)
	for _, line := range []string{`wwz_tier_promotions_total{from="free",to="pro"} 1`, `wwz_tier_promotions_total{from="pro",to="max"} 1`} {
		if !strings.Contains(m, line) {
			t.Errorf("metrics lack %s", line)
		}
	}
}

func buy2(t *testing.T, s *Server, path, body string) limits.EntityState {
	t.Helper()
	r := do(t, s, "POST", path, "application/json", body)
	want(t, r, 200)
	var st limits.EntityState
	r.json(t, &st)
	return st
}

// TestCreditsValidation: exactly one of usd and add_usd, add_usd > 0, and
// a store without purchases answers 503 (cmd/wwz/openapi.yaml).
func TestCreditsValidation(t *testing.T) {
	s := newServer(t, fastCfg)
	for _, body := range []string{``, `{}`, `{"usd":1,"add_usd":1}`, `{"add_usd":0}`, `{"add_usd":-2}`, `{"add_usd":"x"}`, `{"add":1}`} {
		if r := do(t, s, "POST", "/admin/entities/account/acct-test/credits", "application/json", body); r.code != 400 {
			t.Errorf("%s = %d %s, want 400", body, r.code, r.body)
		}
	}
	want(t, do(t, s, "POST", "/admin/entities/bogus/x/credits", "application/json", `{"add_usd":1}`), 400)
	// Without EnablePromotion a purchase still works.
	if st := buy2(t, s, "/admin/entities/account/acct-test/credits", `{"add_usd":2.5}`); st.PurchasedUSD != 2.5 {
		t.Fatalf("purchase: %+v", st)
	}
	s.eng.Store = failStore{Store: limits.NewMemory()}
	r := do(t, s, "POST", "/admin/entities/account/acct-test/credits", "application/json", `{"add_usd":1}`)
	want(t, r, 503)
	var p Problem
	r.json(t, &p)
	if p.Error != "store_unavailable" {
		t.Fatalf("unsupported: %+v", p)
	}
}
