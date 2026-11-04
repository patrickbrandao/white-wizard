package engine_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Task 258: GET /api/v1/key reports the key's spend of the day, the week,
// the month and in total, and the :free requests of the account's day;
// GET /api/v1/credits the account's accumulated spend.

type keyUsage struct {
	Data struct {
		Usage        float64 `json:"usage"`
		UsageDaily   float64 `json:"usage_daily"`
		UsageWeekly  float64 `json:"usage_weekly"`
		UsageMonthly float64 `json:"usage_monthly"`
		Free         struct {
			Used      int `json:"used"`
			Limit     int `json:"limit"`
			Remaining int `json:"remaining"`
		} `json:"free_model_daily_requests"`
	} `json:"data"`
}

func getKeyUsage(t *testing.T, e *engine.Engine, key string) keyUsage {
	t.Helper()
	w := call(t, e, "openrouter", "/api/v1/key", map[string]string{"Authorization": "Bearer " + key}, "")
	if w.StatusCode() != 200 {
		t.Fatalf("key: status %d: %s", w.StatusCode(), w.Body())
	}
	var out keyUsage
	if err := json.Unmarshal(w.Body(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestKeyInfoSpendWindowsAndFreeRequests(t *testing.T) {
	e := newEngine(t, keyInfoCfg)
	const key = "sk-or-v1-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ctx := context.Background()
	keyEnt := limits.Entity{Kind: limits.KindKey, ID: "capped"}
	acct := limits.Entity{Kind: limits.KindAccount, ID: "acme"}

	// Spend of yesterday counts in the total (and in the week and month
	// when they hold yesterday), never in the day.
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yesterday := today.Add(-time.Hour)
	if err := e.Store.Finish(ctx, limits.FinishRequest{Subjects: []limits.Subject{{Entity: keyEnt}, {Entity: acct}}, Now: yesterday, CostMicroUSD: 1_000_000, Status: 200}); err != nil {
		t.Fatal(err)
	}
	w := call(t, e, "openrouter", "/api/v1/chat/completions", map[string]string{"Authorization": "Bearer " + key},
		`{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	if w.StatusCode() != 200 {
		t.Fatalf("chat: %d %s", w.StatusCode(), w.Body())
	}
	d := getKeyUsage(t, e, key).Data
	day := d.UsageDaily
	if day <= 0 || day >= 1 || !near(d.Usage, day+1) {
		t.Fatalf("usage %v daily %v, want the call today and 1 more in total", d.Usage, day)
	}
	weekly, monthly := day, day
	if monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7)); !yesterday.Before(monday) {
		weekly++
	}
	if yesterday.Month() == today.Month() {
		monthly++
	}
	if !near(d.UsageWeekly, weekly) || !near(d.UsageMonthly, monthly) {
		t.Fatalf("weekly %v monthly %v, want %v %v", d.UsageWeekly, d.UsageMonthly, weekly, monthly)
	}
	if total, used := credits(t, e, key); total != 28 || !near(used, d.Usage) {
		t.Fatalf("credits: total %v used %v, want 28 and the account's spent %v", total, used, d.Usage)
	}

	// :free requests of the day count against the account's cap.
	if d.Free.Used != 0 || d.Free.Limit != 1000 || d.Free.Remaining != 1000 {
		t.Fatalf("free before: %+v", d.Free)
	}
	for range 3 {
		if err := e.Store.Finish(ctx, limits.FinishRequest{Subjects: []limits.Subject{{Entity: keyEnt, Limits: config.Limits{}}, {Entity: acct}}, Now: now, Model: "meta-llama/llama-3.3-70b-instruct:free", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	if f := getKeyUsage(t, e, key).Data.Free; f.Used != 3 || f.Limit != 1000 || f.Remaining != 997 {
		t.Fatalf("free after: %+v, want 3 used of 1000", f)
	}
	// The count is the account's: the key's own does not show.
	poor := limits.Entity{Kind: limits.KindAccount, ID: "poor"}
	if err := e.Store.Finish(ctx, limits.FinishRequest{Subjects: []limits.Subject{{Entity: limits.Entity{Kind: limits.KindKey, ID: "free"}}}, Now: now, Model: "x:free", Status: 200}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Finish(ctx, limits.FinishRequest{Subjects: []limits.Subject{{Entity: poor}}, Now: now, Model: "x:free", Status: 200}); err != nil {
		t.Fatal(err)
	}
	if f := getKeyUsage(t, e, "k-free").Data.Free; f.Used != 1 || f.Limit != 50 || f.Remaining != 49 {
		t.Fatalf("free account: %+v, want 1 used of 50", f)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
