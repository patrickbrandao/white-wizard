package opencode_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Task 254: the usage route reports the account's quota windows, its 401
// is the gateway's AuthError and the list cost keeps the pico-dollars.

// TestRealUsageUnauthorized: GET /zen/go/v1/usage with an invalid key or
// without one answers AuthError/Unauthorized with the listing's headers,
// byte for byte (usage_zen_badkey.json; no key conferred on 2026-10-31).
func TestRealUsageUnauthorized(t *testing.T) {
	d := newDriver(t)
	fx := loadShape(t, "usage_zen_badkey")
	for _, e := range []core.Error{
		{Kind: core.ErrInvalidAPIKey},
		{Kind: core.ErrInvalidAPIKey, Message: "You didn't provide an API key."},
		{Kind: core.ErrAccountBanned},
	} {
		h := driver.MapHeaders{}
		driver.SetRoute(h, "GET", fx.Request.Path)
		w := core.NewMemWriter()
		d.WriteError(w, &core.Request{Headers: h}, &e, nil)
		if w.StatusCode() != fx.Response.Status || string(w.Body()) != fx.Response.Body {
			t.Errorf("%+v: %d %s, real %d %s", e, w.StatusCode(), w.Body(), fx.Response.Status, fx.Response.Body)
		}
		checkHeaders(t, fx, w.Headers())
	}
}

type usageBody struct {
	Usage map[string]struct {
		Status   string `json:"status"`
		Percent  int    `json:"percent"`
		ResetsAt string `json:"resetsAt"`
	} `json:"usage"`
}

func usageOf(t *testing.T, xc driver.ExtraContext) usageBody {
	t.Helper()
	w := core.NewMemWriter()
	newDriver(t).Extra(driver.KindUsage, w, driver.MapHeaders{}, nil, xc)
	var u usageBody
	if err := json.Unmarshal(w.Body(), &u); err != nil {
		t.Fatalf("%s: %v", w.Body(), err)
	}
	return u
}

// TestUsageQuotaPercent: weekly and monthly come from the account's spend
// over its quota_usd windows, rounded and capped at 100; without quota,
// without an account or without the store's view every window reads 0.
func TestUsageQuotaPercent(t *testing.T) {
	lim := config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 60, Rolling5hPct: 20, WeeklyPct: 50}}
	cases := []struct {
		name                     string
		xc                       driver.ExtraContext
		rolling, weekly, monthly int
	}{
		{"spent", driver.ExtraContext{Limits: lim, AccountBalance: &driver.Balance{SpendUSD: 12, SpendWeeklyUSD: 7.5}}, 0, 25, 20},
		{"rounded", driver.ExtraContext{Limits: lim, AccountBalance: &driver.Balance{SpendUSD: 1.25, SpendWeeklyUSD: 0.14}}, 0, 0, 2},
		{"capped", driver.ExtraContext{Limits: lim, AccountBalance: &driver.Balance{SpendUSD: 90, SpendWeeklyUSD: 45}}, 0, 100, 100},
		{"no weekly window", driver.ExtraContext{Limits: config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 60}}, AccountBalance: &driver.Balance{SpendUSD: 30, SpendWeeklyUSD: 30}}, 0, 0, 50},
		{"no quota", driver.ExtraContext{AccountBalance: &driver.Balance{SpendUSD: 30}}, 0, 0, 0},
		{"no account", driver.ExtraContext{Limits: lim}, 0, 0, 0},
	}
	for _, c := range cases {
		u := usageOf(t, c.xc)
		got := [3]int{u.Usage["rolling"].Percent, u.Usage["weekly"].Percent, u.Usage["monthly"].Percent}
		if want := [3]int{c.rolling, c.weekly, c.monthly}; got != want {
			t.Errorf("%s: rolling, weekly, monthly %v, want %v", c.name, got, want)
		}
		for name, w := range u.Usage {
			if w.Status != "ok" {
				t.Errorf("%s: %s status %q", c.name, name, w.Status)
			}
		}
	}
	at, err := time.Parse(time.RFC3339, usageOf(t, driver.ExtraContext{}).Usage["monthly"].ResetsAt)
	now := time.Now().UTC()
	if err != nil || at.Day() != 1 || at.Hour() != 0 || !at.After(now) || at.After(now.AddDate(0, 1, 1)) {
		t.Errorf("monthly resetsAt %v (%v), want the start of the next UTC month", at, err)
	}
}

// TestListCostPico: the list cost and the allowance come from the engine's
// pico-dollars, not the whole micro-dollars (task 246): the chat fixture's
// tokens of the chat fixture, at $0.15/M in and $0.60/M out, give its costs
// (tokens and costs are read from the fixture).
func TestListCostPico(t *testing.T) {
	fx := loadShape(t, "chat")
	var body struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			Details          struct {
				Reasoning int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(fx.Response.Body), &body); err != nil {
		t.Fatal(err)
	}
	want := regexp.MustCompile(`"cost":0,"opencode":\{"billing_source":"go","list_cost":[0-9.e-]+,"allowance":[0-9.e-]+,`).FindString(fx.Response.Body)
	if want == "" {
		t.Fatalf("no opencode usage in the fixture: %s", fx.Response.Body)
	}
	listCost := regexp.MustCompile(`"list_cost":([0-9.e-]+)`).FindStringSubmatch(want)[1]
	in, out := int64(body.Usage.PromptTokens), int64(body.Usage.CompletionTokens)
	u := core.Usage{
		PromptTokens: int(in), CompletionTokens: int(out), ReasoningTokens: body.Usage.Details.Reasoning,
		CostMicroUSD:      (in*150_000 + out*600_000) / 1_000_000,
		PromptCostPicoUSD: in * 150_000, CompletionCostPicoUSD: out * 600_000,
	}
	w := core.NewMemWriter()
	resp := &core.Response{ID: gwID, Model: "deepseek-v4.1-flash", Content: "ok", FinishReason: core.FinishStop, Usage: u}
	if err := newDriver(t).WriteResponse(w, chatReq(false), resp, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(w.Body()), want) {
		t.Errorf("usage %s\nwant …%s", w.Body(), want)
	}
	req := chatReq(false)
	req.Headers["x-wwz-path"] = "/inference/openai/v1/chat/completions"
	w = core.NewMemWriter()
	if err := newDriver(t).WriteResponse(w, req, resp, nil); err != nil {
		t.Fatal(err)
	}
	if want := `"cost":` + listCost + `,"opencode":{"billing_source":"balance","list_cost":` + listCost + `,`; !strings.Contains(string(w.Body()), want) {
		t.Errorf("balance usage %s\nwant …%s", w.Body(), want)
	}
}
