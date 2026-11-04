package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Task 258: the escalation record of the store outlives the strike window,
// so the ladder keeps climbing after a block longer than the window, and
// ban_after_strikes counts the strikes of the record (30 days), not of the
// window.

const shortWindowCfg = `
server: {seed: 42, auth: keys}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: gpt-4o-mini, latency: fast}
keys:
  - {key: k-abuse, label: abuse, account: a, limits: {rpm: 1}}
abuse:
  enabled: true
  strike_429: {count: 1, window: 20ms}
  ladder:
    - {action: suspend, duration: 150ms}
    - {action: suspend, duration: 10m}
`

var abuseEnt = limits.Entity{Kind: limits.KindKey, ID: "abuse"}

func abuseStatus(t *testing.T, e *engine.Engine) int {
	t.Helper()
	return call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer k-abuse"}, chatBody).StatusCode()
}

func wantStatuses(t *testing.T, e *engine.Engine, want ...int) {
	t.Helper()
	for i, w := range want {
		if got := abuseStatus(t, e); got != w {
			t.Fatalf("request %d: status %d, want %d", i+1, got, w)
		}
	}
}

// blockLength is how long the current block of the entity lasts.
func blockLength(t *testing.T, e *engine.Engine) time.Duration {
	t.Helper()
	st, err := e.Store.Get(context.Background(), abuseEnt)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != limits.StatusSuspended {
		t.Fatalf("state %+v, want suspended", st)
	}
	return st.Until.Sub(st.Since)
}

func TestAbuseLadderOutlivesStrikeWindow(t *testing.T) {
	e := newEngine(t, shortWindowCfg)
	// 200, then a 429 whose strike escalates to the first rung (150 ms).
	wantStatuses(t, e, 200, 429, 403)
	if d := blockLength(t, e); d > time.Second {
		t.Fatalf("first block lasts %v, want the first rung", d)
	}
	// The block and the strike window end; the next 429 opens a new window
	// but the record remembers the first escalation: second rung.
	time.Sleep(200 * time.Millisecond)
	wantStatuses(t, e, 429, 403)
	if d := blockLength(t, e); d < 9*time.Minute {
		t.Fatalf("second block lasts %v, want the second rung (10m)", d)
	}
}

// noEscalator hides limits.Escalator from the engine.
type noEscalator struct{ limits.Store }

// failingEscalator answers every Escalate with an error.
type failingEscalator struct{ limits.Store }

func (failingEscalator) Escalate(context.Context, limits.Entity, bool, time.Duration) (limits.Escalation, error) {
	return limits.Escalation{}, errors.New("escalate down")
}

func TestAbuseLadderWithoutEscalationRecord(t *testing.T) {
	for name, wrap := range map[string]func(limits.Store) limits.Store{
		"no escalator":      func(s limits.Store) limits.Store { return noEscalator{s} },
		"failing escalator": func(s limits.Store) limits.Store { return failingEscalator{s} },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEngine(t, shortWindowCfg)
			e.Store = wrap(e.Store)
			wantStatuses(t, e, 200, 429, 403)
			// Without the record the window decides: a new window starts
			// over at the first rung.
			time.Sleep(200 * time.Millisecond)
			wantStatuses(t, e, 429, 403)
			if d := blockLength(t, e); d > time.Second {
				t.Fatalf("block lasts %v, want the first rung again", d)
			}
		})
	}
}

func TestAbuseBanAfterStrikesAcrossWindows(t *testing.T) {
	e := newEngine(t, `
server: {seed: 42, auth: keys}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: gpt-4o-mini, latency: fast}
keys:
  - {key: k-abuse, label: abuse, account: a, limits: {rpm: 1}}
abuse:
  enabled: true
  strike_429: {count: 100, window: 5ms}
  ban_after_strikes: 3
`)
	wantStatuses(t, e, 200)
	for i := range 3 {
		// Each strike falls in a window of its own.
		time.Sleep(10 * time.Millisecond)
		if got := abuseStatus(t, e); got != 429 {
			t.Fatalf("strike %d: status %d, want 429", i+1, got)
		}
	}
	st, err := e.Store.Get(context.Background(), abuseEnt)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != limits.StatusBanned || st.Reason != "abuse ladder: 3 strikes" || st.By != "abuse-ladder" {
		t.Fatalf("state %+v, want banned after 3 strikes in 30 days", st)
	}
}

// ladderEngine is an engine with rpm 1 on k-abuse, strike_429 count 1 and
// the given ladder (YAML flow list, may be empty).
func ladderEngine(t *testing.T, ladder string) *engine.Engine {
	t.Helper()
	return newEngine(t, `
server: {seed: 42, auth: keys}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: gpt-4o-mini, latency: fast}
keys:
  - {key: k-abuse, label: abuse, account: a, limits: {rpm: 1}}
abuse:
  enabled: true
  strike_429: {count: 1, window: 1m}
  ladder: `+ladder+`
`)
}

func TestAbuseLadderRungs(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, ladder string
		status       limits.Status
		reason       string
		length       time.Duration
	}{
		{"no ladder blocks a minute", "[]", limits.StatusSuspended, "abuse ladder: repeated rate limiting", time.Minute},
		{"suspend without duration", "[{action: suspend}]", limits.StatusSuspended, "abuse ladder: suspend", time.Minute},
		{"ban rung", "[{action: ban}]", limits.StatusBanned, "abuse ladder: ban", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := ladderEngine(t, tc.ladder)
			wantStatuses(t, e, 200, 429)
			st, err := e.Store.Get(ctx, abuseEnt)
			if err != nil {
				t.Fatal(err)
			}
			if st.Status != tc.status || st.Reason != tc.reason || st.By != "abuse-ladder" {
				t.Fatalf("state %+v, want %s %q", st, tc.status, tc.reason)
			}
			if tc.length > 0 {
				if d := st.Until.Sub(st.Since); d < tc.length-time.Second || d > tc.length+time.Second {
					t.Fatalf("block lasts %v, want %v", d, tc.length)
				}
			}
		})
	}
}

// recordingEscalator records the TTL the engine passes to Escalate.
type recordingEscalator struct {
	limits.Store
	ttl time.Duration
}

func (r *recordingEscalator) Escalate(ctx context.Context, e limits.Entity, escalated bool, ttl time.Duration) (limits.Escalation, error) {
	r.ttl = ttl
	return r.Store.(limits.Escalator).Escalate(ctx, e, escalated, ttl)
}

func TestAbuseEscalationTTL(t *testing.T) {
	e := ladderEngine(t, "[{action: warn}]")
	rec := &recordingEscalator{Store: e.Store}
	e.Store = rec
	wantStatuses(t, e, 200, 429)
	if rec.ttl != 30*24*time.Hour {
		t.Fatalf("ttl %v, want the default strike_ttl of 30 days", rec.ttl)
	}
	// A configuration built without config.Load (strike_ttl 0) still keeps
	// the record for 30 days.
	cfg := *e.Config()
	cfg.Abuse.StrikeTTL = 0
	e.SetConfig(&cfg)
	rec.ttl = 0
	wantStatuses(t, e, 429)
	if rec.ttl != 30*24*time.Hour {
		t.Fatalf("ttl %v with strike_ttl 0, want 30 days", rec.ttl)
	}
}
