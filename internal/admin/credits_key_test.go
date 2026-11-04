package admin

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// TestCreditsKeyResolvesAccount covers task 248 (ADR 0058): add_usd on a key
// credits its account; usd stays on the key; unknown keys are 400.
func TestCreditsKeyResolvesAccount(t *testing.T) {
	pol := &limits.PolicyApplier{Log: quiet}
	s := newServer(t, promoteCfg, func(o *Options) { o.Override = pol.Override })
	s.OnReload(pol.Committed)
	pol.Current, pol.Apply = s.Config, s.Apply
	s.eng.EnablePromotion(pol)

	st := buy2(t, s, "/admin/entities/key/p1/credits", `{"add_usd": 12}`)
	if st.Entity != (limits.Entity{Kind: limits.KindAccount, ID: "acme"}) || st.PurchasedUSD != 12 {
		t.Fatalf("add_usd on key: %+v", st)
	}
	// promote_when of the account was evaluated (free -> pro by purchase of
	// the next tier is not reached here; the account state is what matters).
	if st := buy2(t, s, "/admin/entities/key/p1/credits", `{"usd": 2}`); st.Entity.Kind != limits.KindKey || *st.Credits != 2 {
		t.Fatalf("usd on key: %+v", st)
	}
	if r := do(t, s, "POST", "/admin/entities/key/ghost/credits", "application/json", `{"add_usd": 1}`); r.code != 400 {
		t.Fatalf("unknown key: %d", r.code)
	}
}

const creditsKeyPromoteCfg = fastCfg + `
keys:
  - {key: wwz-key-01, label: p1, account: acme}
accounts:
  - {id: acme, tier: free}
tiers:
  free: {limits: {rpm: 2}, promote_when: {purchased_usd: 10}, next: pro}
  pro: {limits: {rpm: 1000}}
`

// TestCreditsKeyRaisesAccountBalanceAndPromotes (spec 010): add_usd on a key
// raises the balance of its account and a purchase that crosses
// promote_when promotes the account through the key path.
func TestCreditsKeyRaisesAccountBalanceAndPromotes(t *testing.T) {
	pol := &limits.PolicyApplier{Log: quiet}
	s := newServer(t, creditsKeyPromoteCfg, func(o *Options) { o.Override = pol.Override })
	s.OnReload(pol.Committed)
	pol.Current, pol.Apply = s.Config, s.Apply
	p := s.eng.EnablePromotion(pol)
	s.eng.Store.(limits.PolicySource).WatchPolicies(pol.Offer)

	if st := buy2(t, s, "/admin/entities/key/p1/credits", `{"add_usd": 4}`); st.Entity.Kind != limits.KindAccount || st.Credits == nil || *st.Credits != 4 {
		t.Fatalf("first purchase: %+v", st)
	}
	p.Wait()
	if pol.AccountTier("acme") != "" {
		t.Fatalf("promoted below purchased_usd: %q", pol.AccountTier("acme"))
	}
	if st := buy2(t, s, "/admin/entities/key/p1/credits", `{"add_usd": 7}`); st.Credits == nil || *st.Credits != 11 || st.PurchasedUSD != 11 {
		t.Fatalf("second purchase: %+v", st)
	}
	p.Wait()
	if pol.AccountTier("acme") != "pro" {
		t.Fatalf("account tier = %q, want pro", pol.AccountTier("acme"))
	}
	// The balance is the account's, readable on the account entity.
	var acct limits.EntityState
	r := do(t, s, "GET", "/admin/entities/account/acme", "", "")
	want(t, r, 200)
	r.json(t, &acct)
	if acct.Credits == nil || *acct.Credits != 11 || acct.Tier != "pro" {
		t.Fatalf("account: %+v", acct)
	}
}

func TestCreditsKeyWithoutAccount(t *testing.T) {
	s := newServer(t, fastCfg+"\nkeys:\n  - {key: wwz-key-01, label: loose}\n")
	// Parsing gives every key an account (acct-<label>): credited there.
	if st := buy2(t, s, "/admin/entities/key/loose/credits", `{"add_usd": 1}`); st.Entity.ID != "acct-loose" {
		t.Fatalf("default account: %+v", st)
	}
	// A configuration swapped in without one is refused.
	c := *s.eng.Config()
	c.Keys = []config.KeyConfig{{Key: "wwz-key-01", Label: "loose"}}
	s.eng.SetConfig(&c)
	if r := do(t, s, "POST", "/admin/entities/key/loose/credits", "application/json", `{"add_usd": 1}`); r.code != 400 {
		t.Fatalf("key without account: %d", r.code)
	}
}
