package limits_test

import (
	"context"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Task 189 (ADR 0051, spec 010): per unit the reported state is that of the
// level with the smallest remaining, whatever the order of the levels.
func checkMostRestrictive(t *testing.T, s limits.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	key := limits.Entity{Kind: limits.KindKey, ID: "mr-key"}
	acct := limits.Entity{Kind: limits.KindAccount, ID: "mr-acct"}
	req := func(kl, al config.Limits) limits.CheckRequest {
		return limits.CheckRequest{Subjects: []limits.Subject{{Entity: key, Limits: kl}, {Entity: acct, Limits: al}}, EstPromptTokens: 10, Now: now}
	}

	// Key ceiling tighter on requests, account tighter on tokens.
	d, err := s.Check(ctx, req(config.Limits{RPM: 3, TPM: 1000}, config.Limits{RPM: 100, TPM: 500}))
	if err != nil || !d.Allowed {
		t.Fatalf("check: %+v %v", d, err)
	}
	if d.State.RequestsLimit != 3 || d.State.RequestsRemaining != 2 {
		t.Errorf("requests = %d/%d, want the key's 3/2", d.State.RequestsLimit, d.State.RequestsRemaining)
	}
	if d.State.TokensLimit != 500 || d.State.TokensRemaining != 490 {
		t.Errorf("tokens = %d/%d, want the account's 500/490", d.State.TokensLimit, d.State.TokensRemaining)
	}

	// The account alone limits requests: its state wins although the key
	// (evaluated first) also reports requests.
	key2 := limits.Entity{Kind: limits.KindKey, ID: "mr-key2"}
	acct2 := limits.Entity{Kind: limits.KindAccount, ID: "mr-acct2"}
	d, err = s.Check(ctx, limits.CheckRequest{Subjects: []limits.Subject{{Entity: key2, Limits: config.Limits{RPM: 50}}, {Entity: acct2, Limits: config.Limits{RPM: 2}}}, Now: now})
	if err != nil || !d.Allowed {
		t.Fatalf("check 2: %+v %v", d, err)
	}
	if d.State.RequestsLimit != 2 || d.State.RequestsRemaining != 1 {
		t.Errorf("requests = %d/%d, want the account's 2/1", d.State.RequestsLimit, d.State.RequestsRemaining)
	}
	// A later level with more room never hides a tighter earlier one.
	d, err = s.Check(ctx, limits.CheckRequest{Subjects: []limits.Subject{{Entity: acct2, Limits: config.Limits{RPM: 2}}, {Entity: key2, Limits: config.Limits{RPM: 50}}}, Now: now})
	if err != nil || !d.Allowed {
		t.Fatalf("check 3: %+v %v", d, err)
	}
	if d.State.RequestsLimit != 2 || d.State.RequestsRemaining != 0 {
		t.Errorf("requests = %d/%d, want the account's 2/0", d.State.RequestsLimit, d.State.RequestsRemaining)
	}
}

func TestMostRestrictiveStateMemory(t *testing.T) {
	checkMostRestrictive(t, limits.NewMemory())
}

func TestMostRestrictiveStateValkey(t *testing.T) {
	t.Run("script", func(t *testing.T) { checkMostRestrictive(t, newValkeyStore(t)) })
	t.Run("per_entity", func(t *testing.T) {
		checkMostRestrictive(t, newValkeyStore(t, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.PerEntity = true }))
	})
	t.Run("local_first", func(t *testing.T) {
		checkMostRestrictive(t, newValkeyStore(t, func(c *config.StoreConfig, o *limits.ValkeyOptions) {
			c.LocalFirst = true
			o.SyncInterval = time.Hour
		}))
	})
}
