package storetest

import (
	"context"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// usageQuirk is limits.Memory with a misbehaving OnUsage contract, to make
// the usage assertions of the suite fail (ADR 0013: the suite's own
// assertions are exercised by its own tests).
type usageQuirk struct {
	*limits.Memory
	skewFirst   bool // the first usage reports one request too many
	onDenial    bool // a denial reports usage
	usageCalled int
}

func (q *usageQuirk) Finish(ctx context.Context, req limits.FinishRequest) error {
	if fn := req.OnUsage; fn != nil {
		req.OnUsage = func(e limits.Entity, u config.PromotionUsage) {
			q.usageCalled++
			if q.skewFirst && q.usageCalled == 1 {
				u.Requests++
			}
			fn(e, u)
		}
		if q.onDenial && req.Denial != nil {
			fn(accountEnt, config.PromotionUsage{})
		}
	}
	return q.Memory.Finish(ctx, req)
}

func TestFinishUsageAssertionsFail(t *testing.T) {
	for _, c := range []struct {
		name string
		q    *usageQuirk
		msg  string
	}{
		{"skewed first usage", &usageQuirk{Memory: limits.NewMemory(), skewFirst: true}, "usage after one request"},
		{"usage on denial", &usageQuirk{Memory: limits.NewMemory(), onDenial: true}, "called for a denial"},
	} {
		ft := runCase(testFinishUsage, c.q)
		if !ft.failed() || !strings.Contains(strings.Join(ft.msgs, "\n"), c.msg) {
			t.Errorf("%s: want a failure mentioning %q, got %v", c.name, c.msg, ft.msgs)
		}
	}
}

// noCounterReset keeps credits and per-entity resets but hides
// limits.CounterResetter.
type noCounterReset struct {
	limits.Store
	limits.CreditAdder
	limits.EntityResetter
}

func TestResetKeepsPurchasedSkipsWithoutCounterResetter(t *testing.T) {
	m := limits.NewMemory()
	ft := runCase(testResetKeepsPurchased, noCounterReset{Store: m, CreditAdder: m, EntityResetter: m})
	if !ft.skipped || ft.failed() {
		t.Fatalf("want a skip, got skipped=%v msgs=%v", ft.skipped, ft.msgs)
	}
}
