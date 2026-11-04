package limits

import (
	"context"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

func subj(l config.Limits) Subject {
	return Subject{Entity: Entity{Kind: KindKey, ID: "k1"}, Limits: l}
}

func TestRPMBucket(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	l := config.Limits{RPM: 60}
	allowed := 0
	for i := 0; i < 70; i++ {
		d, err := m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed {
			allowed++
		} else if d.Kind != core.ErrRateLimit || d.LimitName != "rpm" || d.RetryAfter <= 0 {
			t.Fatalf("bad decision: %+v", d)
		}
	}
	if allowed != 60 {
		t.Fatalf("expected 60 allowed, got %d", allowed)
	}
	// 30s later half the bucket is back.
	d, _ := m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}, Now: now.Add(30 * time.Second)})
	if !d.Allowed || d.State.RequestsRemaining < 28 || d.State.RequestsRemaining > 30 {
		t.Fatalf("refill: %+v", d.State)
	}
}

func TestTPMReserveAndFinish(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	l := config.Limits{TPM: 1000}
	d, _ := m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}, EstPromptTokens: 100, EstOutputTokens: 500, Now: now})
	if !d.Allowed || d.State.TokensRemaining != 400 {
		t.Fatalf("reserve: %+v", d)
	}
	// Actual output was 100 → 400 tokens returned.
	_ = m.Finish(ctx, FinishRequest{Subjects: []Subject{subj(l)}, EstOutputTokens: 500, ActualOutput: 100, Now: now})
	d, _ = m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}, EstPromptTokens: 700, Now: now})
	if !d.Allowed {
		t.Fatalf("should fit after refund: %+v", d)
	}
	d, _ = m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}, EstPromptTokens: 200, Now: now})
	if d.Allowed {
		t.Fatalf("should exceed: %+v", d)
	}
}

func TestBlockBanAndConcurrency(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	e := Entity{Kind: KindKey, ID: "k1"}
	l := config.Limits{Concurrency: 1}
	d, _ := m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}})
	if !d.Allowed {
		t.Fatal("first should pass")
	}
	d, _ = m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}})
	if d.Allowed || d.LimitName != "concurrency" {
		t.Fatalf("second should hit concurrency: %+v", d)
	}
	_ = m.Finish(ctx, FinishRequest{Subjects: []Subject{subj(l)}})
	d, _ = m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}})
	if !d.Allowed {
		t.Fatal("after finish should pass")
	}
	_ = m.Finish(ctx, FinishRequest{Subjects: []Subject{subj(l)}})
	_ = m.Block(ctx, e, "test", time.Hour, "tester")
	d, _ = m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}})
	if d.Allowed || d.Kind != core.ErrAccountSuspended || d.RetryAfter <= 0 {
		t.Fatalf("blocked: %+v", d)
	}
	_ = m.Unblock(ctx, e)
	_ = m.Ban(ctx, e, "abuse", "tester")
	d, _ = m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}})
	if d.Allowed || d.Kind != core.ErrAccountBanned {
		t.Fatalf("banned: %+v", d)
	}
	list, _ := m.List(ctx, StatusBanned, 10)
	if len(list) != 1 {
		t.Fatalf("list banned: %d", len(list))
	}
}

func TestCreditsAndQuota(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	c := 0.001
	l := config.Limits{CreditsUSD: &c}
	d, _ := m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}, Now: now})
	if !d.Allowed {
		t.Fatal("credits positive should pass")
	}
	_ = m.Finish(ctx, FinishRequest{Subjects: []Subject{subj(l)}, CostMicroUSD: 2000, Now: now})
	d, _ = m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}, Now: now})
	if d.Allowed || d.LimitName != "credits" {
		t.Fatalf("credits exhausted: %+v", d)
	}
	q := config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 10, Rolling5hPct: 20, WeeklyPct: 50}}
	s := Subject{Entity: Entity{Kind: KindAccount, ID: "a"}, Limits: q}
	_ = m.Finish(ctx, FinishRequest{Subjects: []Subject{s}, CostMicroUSD: 2_000_000, Now: now})
	d, _ = m.Check(ctx, CheckRequest{Subjects: []Subject{s}, Now: now})
	if d.Allowed || d.LimitName != "quota_5h" {
		t.Fatalf("quota 5h: %+v", d)
	}
	d, _ = m.Check(ctx, CheckRequest{Subjects: []Subject{s}, Now: now.Add(6 * time.Hour)})
	if !d.Allowed {
		t.Fatalf("quota 5h should reset: %+v", d)
	}
}

func TestAttempts(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		n, _ := m.Attempts(ctx, "conv", "step", time.Hour)
		if n != i {
			t.Fatalf("attempt %d got %d", i, n)
		}
	}
}

func TestTPDRefundInSameDay(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	l := config.Limits{TPD: 1000}
	d, _ := m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}, EstPromptTokens: 100, EstOutputTokens: 500, Now: now})
	if !d.Allowed {
		t.Fatalf("reserve: %+v", d)
	}
	_ = m.Finish(ctx, FinishRequest{Subjects: []Subject{subj(l)}, EstOutputTokens: 500, ActualOutput: 100, Now: now})
	d, _ = m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}, EstPromptTokens: 800, Now: now})
	if !d.Allowed {
		t.Fatalf("800 must fit after the 400 refund: %+v", d)
	}
	// A refund that arrives in the next day does not touch the old window.
	_ = m.Finish(ctx, FinishRequest{Subjects: []Subject{subj(l)}, EstOutputTokens: 1000, Now: now.Add(24 * time.Hour)})
	d, _ = m.Check(ctx, CheckRequest{Subjects: []Subject{subj(l)}, EstPromptTokens: 1, Now: now})
	if d.Allowed {
		t.Fatalf("day is full: %+v", d)
	}
}

func TestRampRolledBackWhenLaterLevelDenies(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	key := Subject{Entity: Entity{Kind: KindKey, ID: "k"}, Limits: config.Limits{Ramp: &config.Ramp{ThresholdTPM: 100, MaxIncreasePct: 50, Window: time.Minute}}}
	acct := Entity{Kind: KindAccount, ID: "a"}
	ss := []Subject{key, {Entity: acct}}
	if d, _ := m.Check(ctx, CheckRequest{Subjects: ss, EstPromptTokens: 100, Now: now}); !d.Allowed {
		t.Fatalf("baseline: %+v", d)
	}
	_ = m.Ban(ctx, acct, "x", "t")
	for range 3 {
		if d, _ := m.Check(ctx, CheckRequest{Subjects: ss, EstPromptTokens: 10, Now: now}); d.Allowed || d.LimitName != "ban" {
			t.Fatalf("banned: %+v", d)
		}
	}
	_ = m.Unban(ctx, acct)
	// Denied requests left no ramp samples: 100 → 140 is within +50%.
	if d, _ := m.Check(ctx, CheckRequest{Subjects: ss, EstPromptTokens: 40, Now: now}); !d.Allowed {
		t.Fatalf("ramp sample leaked from denied requests: %+v", d)
	}
}

func TestQuotaMonthlyCalendarMonth(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	s := Subject{Entity: Entity{Kind: KindAccount, ID: "a"}, Limits: config.Limits{QuotaUSD: &config.QuotaUSD{MonthlyUSD: 1}}}
	jan31 := time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC)
	_ = m.Finish(ctx, FinishRequest{Subjects: []Subject{s}, CostMicroUSD: 1_000_000, Now: jan31})
	d, _ := m.Check(ctx, CheckRequest{Subjects: []Subject{s}, Now: jan31.Add(30 * time.Minute)})
	if d.Allowed || d.LimitName != "quota_monthly" || d.RetryAfter != 30*time.Minute {
		t.Fatalf("january quota used: %+v", d)
	}
	if d, _ := m.Check(ctx, CheckRequest{Subjects: []Subject{s}, Now: jan31.Add(2 * time.Hour)}); !d.Allowed {
		t.Fatalf("february resets the quota: %+v", d)
	}
	// An out-of-order timestamp from January does not reopen January.
	_ = m.Finish(ctx, FinishRequest{Subjects: []Subject{s}, CostMicroUSD: 1_000_000, Now: jan31})
	if d, _ := m.Check(ctx, CheckRequest{Subjects: []Subject{s}, Now: jan31.Add(2 * time.Hour)}); d.Allowed {
		t.Fatalf("late January spend counts in the current window: %+v", d)
	}
}

func TestGetReturnsCreditsCopyAndSpendOfCurrentMonth(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	e := Entity{Kind: KindKey, ID: "k1"}
	_ = m.SetCredits(ctx, e, 2)
	st, _ := m.Get(ctx, e)
	*st.Credits = 99
	if st2, _ := m.Get(ctx, e); st2.Credits == nil || *st2.Credits != 2 {
		t.Fatalf("Get must return a copy of the balance: %+v", st2.Credits)
	}
	old := time.Now().UTC().AddDate(0, -2, 0)
	_ = m.Finish(ctx, FinishRequest{Subjects: []Subject{{Entity: e}}, CostMicroUSD: 1_000_000, Now: old})
	if st, _ := m.Get(ctx, e); st.SpendUSD != 0 {
		t.Fatalf("spend of a past month shown as current: %v", st.SpendUSD)
	}
}

func TestResetCountersKeepsBanAndBlock(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	e := Entity{Kind: KindKey, ID: "k1"}
	_ = m.Block(ctx, e, "b", time.Hour, "t")
	_ = m.Ban(ctx, e, "x", "t")
	_ = m.ResetCounters(ctx)
	_ = m.Unban(ctx, e)
	if st, _ := m.Get(ctx, e); st.Status != StatusSuspended || st.Reason != "b" {
		t.Fatalf("block lost on ResetCounters: %+v", st)
	}
}
