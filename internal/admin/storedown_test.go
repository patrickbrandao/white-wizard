package admin

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// downStore behaves like limits.Fallback with the circuit open: the data
// plane keeps working, admin operations fail with limits.ErrStoreDown.
type downStore struct {
	limits.Store
}

func downErr() error { return fmt.Errorf("op: %w", limits.ErrStoreDown) }

func (downStore) Block(context.Context, limits.Entity, string, time.Duration, string) error {
	return downErr()
}
func (downStore) Unblock(context.Context, limits.Entity) error             { return downErr() }
func (downStore) Ban(context.Context, limits.Entity, string, string) error { return downErr() }
func (downStore) Unban(context.Context, limits.Entity) error               { return downErr() }
func (downStore) SetCredits(context.Context, limits.Entity, float64) error { return downErr() }
func (downStore) Get(context.Context, limits.Entity) (limits.EntityState, error) {
	return limits.EntityState{}, downErr()
}

func (downStore) List(context.Context, limits.Status, int) ([]limits.EntityState, error) {
	return nil, downErr()
}
func (downStore) Reset(context.Context) error                          { return downErr() }
func (downStore) ResetCounters(context.Context) error                  { return downErr() }
func (downStore) SetTier(context.Context, limits.Entity, string) error { return downErr() }
func (downStore) ResetEntity(context.Context, limits.Entity, bool, bool) error {
	return downErr()
}

func (downStore) ResetConversations(context.Context, string) (int, error) {
	return 0, downErr()
}

// failStore fails admin operations with an ordinary error (still 500).
type failStore struct {
	limits.Store
}

func (failStore) List(context.Context, limits.Status, int) ([]limits.EntityState, error) {
	return nil, errors.New("boom")
}

// TestStoreDownAnswers503 covers cache/valkey/README.md §6: with Valkey
// unreachable, every admin operation that touches the store answers 503.
func TestStoreDownAnswers503(t *testing.T) {
	s := newServer(t, fastCfg+"keys:\n  - {key: sk-a, label: a, account: acct-a}\n")
	s.eng.Store = downStore{Store: s.eng.Store}
	cases := []struct{ method, target, body string }{
		{"GET", "/admin/entities", ""},
		{"GET", "/admin/entities/key/a", ""},
		{"POST", "/admin/entities/ip/10.0.0.1/block", `{"reason":"x","duration":"1m"}`},
		{"POST", "/admin/entities/ip/10.0.0.1/unblock", ""},
		{"POST", "/admin/entities/key/a/ban", `{"reason":"x"}`},
		{"POST", "/admin/entities/key/a/unban", ""},
		{"POST", "/admin/entities/account/acct-a/credits", `{"usd":1}`},
		{"POST", "/admin/entities/key/a/tier", `{"tier":"default"}`},
		{"POST", "/admin/entities/key/a/reset", `{"scope":["counters"]}`},
		{"POST", "/admin/leak", `{"label":"a"}`},
		{"POST", "/admin/conversations/reset", ""},
		{"POST", "/admin/reset", `{"scope":["limits"]}`},
		{"POST", "/admin/reset", `{"scope":["conversations"]}`},
	}
	for _, c := range cases {
		ctype := ""
		if c.body != "" {
			ctype = "application/json"
		}
		r := do(t, s, c.method, c.target, ctype, c.body)
		if r.code != 503 {
			t.Errorf("%s %s = %d %s, want 503", c.method, c.target, r.code, r.body)
			continue
		}
		var p Problem
		r.json(t, &p)
		if p.Error != "store_unavailable" || p.Message == "" {
			t.Errorf("%s %s problem = %+v", c.method, c.target, p)
		}
		if got := r.hdr.Get("Retry-After"); got != "1" {
			t.Errorf("%s %s Retry-After = %q", c.method, c.target, got)
		}
	}

	// Other store errors stay 500 internal.
	s.eng.Store = failStore{Store: limits.NewMemory()}
	r := do(t, s, "GET", "/admin/entities", "", "")
	want(t, r, 500)
	var p Problem
	r.json(t, &p)
	if p.Error != "internal" || r.hdr.Get("Retry-After") != "" {
		t.Fatalf("generic failure: %+v %v", p, r.hdr)
	}
}
