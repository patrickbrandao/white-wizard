package limits_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	fv "github.com/patrickbrandao/white-wizard/internal/limits/storetest/fakevalkey"
)

// sEscalate is the load index of the escalate script (task 258), after
// sPCCommit in allScripts.
const sEscalate = sPCCommit + 1

func TestFakeEscalateReplies(t *testing.T) {
	ctx := context.Background()
	reply := fv.Err("ERR escalate")
	v, srv := fakeStore(t, func(r fv.Request) string {
		if r.Name == evn(sEscalate) {
			return reply
		}
		return ""
	})
	if _, err := v.Escalate(ctx, fkeyA, true, time.Minute); err == nil {
		t.Fatal("escalate error must surface")
	}
	reply = fv.Ints(1)
	if _, err := v.Escalate(ctx, fkeyA, true, time.Minute); err == nil || !strings.Contains(err.Error(), "escalate reply") {
		t.Fatalf("short escalate reply: %v", err)
	}
	reply = fv.Ints(3, 7)
	if es, err := v.Escalate(ctx, fkeyA, false, 0); err != nil || es != (limits.Escalation{Level: 3, Strikes: 7}) {
		t.Fatalf("escalate = %+v, %v", es, err)
	}
	// The call names the esc key of the entity and passes the flag and a
	// TTL of at least 1 ms.
	var last fv.Request
	for _, c := range srv.Calls() {
		if c.Name == evn(sEscalate) {
			last = c
		}
	}
	if got := strings.Join(last.Args, " "); !strings.Contains(got, ":esc:{key:") || !strings.HasSuffix(got, " 0 1") {
		t.Fatalf("escalate call: %q", got)
	}
}

func TestFakeFallbackEscalate(t *testing.T) {
	v, _ := fakeStore(t, nil)
	cfg := testStoreConfig("unused")
	cfg.OnDown = ""
	f := limits.NewFallback(context.Background(), cfg, func(context.Context) (*limits.Valkey, error) { return v, nil }, limits.FallbackOptions{ProbeInterval: time.Hour})
	t.Cleanup(func() { _ = f.Close() })

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Escalate(gone, fkeyA, true, time.Minute); err == nil {
		t.Fatal("canceled caller must see the error")
	}
	if f.CircuitOpen() {
		t.Fatal("a caller that went away is not a Valkey failure")
	}
	// A real failure falls back to the local memory store.
	if es, err := f.Escalate(context.Background(), fkeyA, true, time.Minute); err != nil || es != (limits.Escalation{Level: 1, Strikes: 1}) {
		t.Fatalf("escalate fallback: %+v %v", es, err)
	}
}
