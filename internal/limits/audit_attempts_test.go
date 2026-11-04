package limits_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/toolargs"
)

// TestValkeyFailFirstAttemptsKey (I7, R24): a scenario with fail_first
// served against the Valkey store keeps its counter in the only scenario
// key, the hash P:att:{conv:<conv>} (field = step id) with a 1 h TTL, and
// nothing else of the scenario engine lands in Valkey (cache/valkey/README.md).
func TestValkeyFailFirstAttemptsKey(t *testing.T) {
	v := newValkeyStore(t)
	ctx := context.Background()
	s, err := scenario.ParseYAML([]byte("name: ff\nsteps:\n  - id: s1\n    fail_first: {times: 1, error: rate_limit}\n    content: ok\n"))
	if err != nil {
		t.Fatal(err)
	}
	rv := &scenario.Resolver{
		Tokenizer: generate.Simulated{}, ArgOptions: toolargs.DefaultOptions(), GlobalSeed: 1,
		Attempts: func(conv, step string) int {
			n, _ := v.Attempts(ctx, conv, step, time.Hour) // as internal/engine does
			return n
		},
	}
	req := &core.Request{Model: "m", Messages: []core.Message{{Role: "user", Content: "hi"}}}
	res, err := rv.Resolve(scenario.Selection{Scenario: s}, req, "conv-1")
	if err != nil || res.Error == nil || res.Attempt != 1 {
		t.Fatalf("first attempt should fail: %+v %v", res, err)
	}

	keys := keysMatching(t, v, "*")
	const key = "wwz:att:{conv:conv-1}"
	if len(keys) != 1 || keys[0] != key {
		t.Fatalf("keys after fail_first = %v, want only %s", keys, key)
	}
	c := v.Client()
	if typ, err := c.Do(ctx, c.B().Arbitrary("TYPE").Keys(key).Build()).ToString(); err != nil || typ != "hash" {
		t.Fatalf("type = %q, %v", typ, err)
	}
	if got, err := c.Do(ctx, c.B().Arbitrary("HGET").Keys(key).Args("s1").Build()).ToString(); err != nil || got != "1" {
		t.Fatalf("HGET s1 = %q, %v", got, err)
	}
	pttl, err := c.Do(ctx, c.B().Arbitrary("PTTL").Keys(key).Build()).AsInt64()
	if err != nil || pttl < 3_590_000 || pttl > 3_600_000 {
		t.Fatalf("PTTL = %d, %v; want 3590-3600 s", pttl, err)
	}

	// The second attempt succeeds, increments the same field and renews the TTL.
	res, err = rv.Resolve(scenario.Selection{Scenario: s}, req, "conv-1")
	if err != nil || res.Error != nil || res.Attempt != 2 {
		t.Fatalf("second attempt should succeed: %+v %v", res, err)
	}
	if got, _ := c.Do(ctx, c.B().Arbitrary("HGET").Keys(key).Args("s1").Build()).ToString(); got != strconv.Itoa(2) {
		t.Fatalf("HGET s1 after retry = %q", got)
	}
	if keys := keysMatching(t, v, "*"); len(keys) != 1 {
		t.Fatalf("keys after retry = %v", keys)
	}
}
