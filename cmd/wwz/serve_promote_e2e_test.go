package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// TestServeValkeyPromotion: two `wwz serve` on one Valkey (ADR 0051 §6,
// task 191). A purchase (add_usd) on one instance promotes the account and
// the other applies the tier through cfg:changed; requests served by the
// second instance promote it one more step, seen by the first; each
// promotion is counted once, on the instance that wrote it; tipo key is
// resolved to the account.
func TestServeValkeyPromotion(t *testing.T) {
	addr, _ := testValkeyDB(t)
	cleanEnv(t)
	t.Setenv("WWZ_VALKEY_ADDR", addr)
	path := filepath.Join(t.TempDir(), "wwz.yaml")
	writeFile(t, path, `
server: {auth: keys, seed: 5}
store: {read_timeout: 1s}
keys:
  - {key: sk-pr-1, label: pr-1, account: acme}
  - {key: sk-pr-2, label: pr-2, account: acme}
accounts:
  - {id: acme, tier: free}
tiers:
  free: {limits: {rpm: 1}, promote_when: {purchased_usd: 10}, next: pro}
  pro: {limits: {rpm: 1000}, promote_when: {requests: 4}, next: max}
  max: {limits: {rpm: 5000}}
`)
	a := startServe(t, "-config", path)
	b := startServe(t, "-config", path)
	json1 := map[string]string{"Content-Type": "application/json"}
	req := `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`
	call := func(in *instance, key string) (int, string) {
		t.Helper()
		code, h, body := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"}, req)
		if code != 200 && code != 429 {
			t.Fatalf("%s: %d %s", key, code, body)
		}
		return code, h.Get("x-ratelimit-limit-requests")
	}
	entity := func(in *instance, path string) limits.EntityState {
		t.Helper()
		code, _, body := do(t, "GET", url(in.a.Admin, path), nil, "")
		if code != 200 {
			t.Fatalf("GET %s: %d %s", path, code, body)
		}
		var st limits.EntityState
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	// free: rpm 1 for the whole account, on both instances.
	if c1, _ := call(b, "sk-pr-1"); c1 != 200 {
		t.Fatalf("first request: %d", c1)
	}
	if c2, lim := call(a, "sk-pr-2"); c2 != 429 || lim != "1" {
		t.Fatalf("free tier on the account: %d (limit %s)", c2, lim)
	}

	// A purchase on a promotes the account; b applies it.
	code, _, body := do(t, "POST", url(a.a.Admin, "/admin/entities/account/acme/credits"), json1, `{"add_usd": 10}`)
	if code != 200 || !strings.Contains(body, `"tier":"pro"`) || !strings.Contains(body, `"purchased_usd":10`) {
		t.Fatalf("purchase: %d %s", code, body)
	}
	waitMetric(t, b.a.Admin, "wwz_policies_version", "1")
	if _, lim := call(b, "sk-pr-1"); lim != "1000" {
		t.Fatalf("pro tier on the other instance: limit %q", lim)
	}
	waitMetric(t, a.a.Admin, `wwz_tier_promotions_total{from="free",to="pro"}`, "1")

	// Requests on b (the account has 2 or 3 so far; pro wants 4) promote it
	// to max; a sees it.
	deadline := time.Now().Add(5 * time.Second)
	for metric(t, a.a.Admin, "wwz_policies_version") != "2" {
		if time.Now().After(deadline) {
			t.Fatalf("no promotion to max; account %+v", entity(b, "/admin/entities/account/acme"))
		}
		call(b, "sk-pr-2")
		time.Sleep(20 * time.Millisecond)
	}
	if st := entity(a, "/admin/entities/account/acme"); st.Tier != "max" {
		t.Fatalf("account on a: %+v", st)
	}
	if _, lim := call(a, "sk-pr-1"); lim != "5000" {
		t.Fatalf("max tier on a: limit %q", lim)
	}
	// The counter moves after the new version is published, so a sees the
	// version before b counts the promotion: wait for it.
	waitMetric(t, b.a.Admin, `wwz_tier_promotions_total{from="pro",to="max"}`, "1")
	if v := metric(t, a.a.Admin, `wwz_tier_promotions_total{from="pro",to="max"}`); v != "" {
		t.Fatalf("a counted b's promotion: %q", v)
	}

	// tipo key: resolved to the key's account; unknown key → 400.
	code, _, body = do(t, "POST", url(b.a.Admin, "/admin/entities/key/pr-2/tier"), json1, `{"tier":"max"}`)
	if code != 200 || !strings.Contains(body, `"kind":"account","id":"acme"`) || !strings.Contains(body, `"tier":"max"`) {
		t.Fatalf("tier by key: %d %s", code, body)
	}
	if code, _, body := do(t, "POST", url(b.a.Admin, "/admin/entities/key/nobody/tier"), json1, `{"tier":"max"}`); code != 400 {
		t.Fatalf("unknown key: %d %s", code, body)
	}
	if !strings.Contains(a.logs.String(), "tier promoted") || !strings.Contains(b.logs.String(), "tier promoted") {
		t.Fatalf("promotion logs missing:\n%s\n%s", a.logs, b.logs)
	}
}
