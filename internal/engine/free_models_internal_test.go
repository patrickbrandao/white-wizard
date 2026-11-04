package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// failingGet is a store whose Get fails.
type failingGet struct{ limits.Store }

func (failingGet) Get(context.Context, limits.Entity) (limits.EntityState, error) {
	return limits.EntityState{}, errors.New("down")
}

// Task 262 (ADR 0060): the free-models unit of the account caps :free
// requests at 20 a minute and 50 a day, 1000 once the account got US$ 10
// (configured credits_usd plus purchases).
func TestFreeModelsSubjectCaps(t *testing.T) {
	cfg, err := config.Parse([]byte(`
accounts:
  - {id: rich, limits: {credits_usd: 10}}
  - {id: some, limits: {credits_usd: 4}}
keys:
  - {key: k-poor, label: poor, account: poor}
  - {key: k-rich, label: rich, account: rich}
  - {key: k-some, label: some, account: some}
`))
	if err != nil {
		t.Fatal(err)
	}
	or, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	oa, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	free := &config.ModelConfig{ID: "meta-llama/llama-3.3-70b-instruct:free"}
	paid := &config.ModelConfig{ID: "meta-llama/llama-3.3-70b-instruct"}
	ctx := context.Background()
	newE := func(st limits.Store) *Engine {
		return &Engine{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	}
	cases := []struct {
		name     string
		key      string
		purchase float64
		rpd      int
	}{
		{"no credits", "k-poor", 0, 50},
		{"under US$ 10 purchased", "k-poor", 9.99, 50},
		{"US$ 10 purchased", "k-poor", 10, 1000},
		{"configured credits", "k-rich", 0, 1000},
		{"configured plus purchased", "k-some", 6, 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newE(limits.NewMemory())
			if c.purchase > 0 {
				acct := cfg.FindKey(c.key).Account
				if _, err := e.Store.(limits.CreditAdder).AddCredits(ctx, limits.Entity{Kind: limits.KindAccount, ID: acct}, c.purchase); err != nil {
					t.Fatal(err)
				}
			}
			s, ok := e.freeModelsSubject(ctx, cfg, or, cfg.FindKey(c.key), free)
			if !ok {
				t.Fatal("no free-models subject")
			}
			want := limits.Entity{Kind: limits.KindFreeModels, ID: cfg.FindKey(c.key).Account}
			if s.Entity != want || s.Limits.RPM != 20 || s.Limits.RPD != c.rpd {
				t.Errorf("subject %+v rpm %d rpd %d, want %+v 20 %d", s.Entity, s.Limits.RPM, s.Limits.RPD, want, c.rpd)
			}
		})
	}
	e := newE(limits.NewMemory())
	for _, c := range []struct {
		name  string
		drv   driver.Driver
		key   *config.KeyConfig
		model *config.ModelConfig
	}{
		{"paid model", or, cfg.FindKey("k-poor"), paid},
		{"no model", or, cfg.FindKey("k-poor"), nil},
		{"no key", or, nil, free},
		{"other driver", oa, cfg.FindKey("k-poor"), free},
	} {
		if _, ok := e.freeModelsSubject(ctx, cfg, c.drv, c.key, c.model); ok {
			t.Errorf("%s: unexpected free-models subject", c.name)
		}
	}
	// A store that cannot be read keeps the cap of the configured credits.
	s, ok := newE(failingGet{limits.NewMemory()}).freeModelsSubject(ctx, cfg, or, cfg.FindKey("k-poor"), free)
	if !ok || s.Limits.RPD != 50 {
		t.Errorf("store down: %v %+v", ok, s.Limits)
	}
}
