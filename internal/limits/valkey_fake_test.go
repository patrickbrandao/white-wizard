package limits_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	fv "github.com/patrickbrandao/white-wizard/internal/limits/storetest/fakevalkey"
)

// Script load order (allScripts in valkey.go).
const (
	sCheck = iota
	sCheckEntity
	sRelease
	sFinish
	sSetStatus
	sStrike
	sResetCount
	sSyncLocal
	sPeek
	sResetEntity
	sSetPolicy
	sSetTier
	sPCCommit
)

// evn is the fakevalkey request name of script i.
func evn(i int) string {
	s := "EVALSHA#"
	if i >= 10 {
		s += "1"
	}
	return s + string(rune('0'+i%10))
}

// fakeStore opens a Valkey store against a scripted server. Commands not
// handled by h answer "-ERR".
func fakeStore(t *testing.T, h fv.Handler, tweaks ...valkeyTweak) (*limits.Valkey, *fv.Server) {
	t.Helper()
	srv := fv.New(t, h)
	cfg := testStoreConfig(srv.Addr)
	cfg.ReadTimeout = 2 * time.Second
	o := limits.ValkeyOptions{DB: 1, Instance: "fake-1", Version: "test", Heartbeat: -1}
	for _, tw := range tweaks {
		tw(&cfg, &o)
	}
	v, err := limits.NewValkey(context.Background(), cfg, o)
	if err != nil {
		t.Fatalf("NewValkey: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v, srv
}

// checkReply is a well formed (29 items) reply of the check scripts.
func checkReply(allowed, idx int64, limit string, retry, created int64) string {
	items := make([]string, 29)
	for i := range items {
		items[i] = fv.Int(0)
	}
	items[0], items[1], items[2], items[3] = fv.Int(allowed), fv.Int(idx), fv.Bulk(limit), fv.Int(retry)
	items[15] = fv.Bulk("why")
	items[16] = fv.Int(created)
	return fv.Arr(items...)
}

func oneSubject(l config.Limits) []limits.Subject {
	return []limits.Subject{{Entity: limits.Entity{Kind: limits.KindKey, ID: "k"}, Limits: l}}
}

func TestFakeNewValkeyErrors(t *testing.T) {
	cfg := testStoreConfig("127.0.0.1:1")
	cfg.Addr = nil
	if _, err := limits.NewValkey(context.Background(), cfg, limits.ValkeyOptions{}); err == nil {
		t.Fatal("empty addr must fail")
	}

	bad := fv.New(t, nil)
	bad.SetHandler(func(r fv.Request) string {
		if r.Name == "SCRIPT LOAD" {
			return fv.Err("ERR load denied")
		}
		return ""
	})
	cfg = testStoreConfig(bad.Addr)
	if _, err := limits.NewValkey(context.Background(), cfg, limits.ValkeyOptions{Heartbeat: -1}); err == nil || !strings.Contains(err.Error(), "SCRIPT LOAD") {
		t.Fatalf("script load error: %v", err)
	}
	wrong := fv.New(t, nil)
	wrong.SetHandler(func(r fv.Request) string {
		if r.Name == "SCRIPT LOAD" {
			return fv.Bulk("deadbeef")
		}
		return ""
	})
	cfg = testStoreConfig(wrong.Addr)
	if _, err := limits.NewValkey(context.Background(), cfg, limits.ValkeyOptions{Heartbeat: -1}); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("sha mismatch: %v", err)
	}
}

func TestFakeDefaultsAndHeartbeatFailure(t *testing.T) {
	t.Setenv("POD_NAME", "pod-x")
	srv := fv.New(t, func(r fv.Request) string {
		if r.Name == "SCRIPT LOAD" {
			return ""
		}
		return fv.Err("ERR boom")
	})
	cfg := testStoreConfig(srv.Addr)
	cfg.Prefix, cfg.EventMaxLen, cfg.EventSample, cfg.ReadTimeout = "", 0, 5, 0
	// Heartbeat 0 = 5 s: the first beat runs inside NewValkey and fails.
	v, err := limits.NewValkey(context.Background(), cfg, limits.ValkeyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	found := false
	for _, c := range srv.Calls() {
		if c.Name == "HSET" && strings.HasPrefix(c.Args[0], "wwz:inst:") && strings.Contains(c.Args[0], "pod-x") {
			found = true
		}
	}
	if !found {
		t.Fatalf("heartbeat not sent with POD_NAME / default prefix: %v", srv.Calls())
	}
}

func TestFakeCheckReplies(t *testing.T) {
	ctx := context.Background()
	reply := ""
	v, _ := fakeStore(t, func(r fv.Request) string {
		if r.Name == evn(sCheck) {
			return reply
		}
		return ""
	})
	l := config.Limits{RPM: 10, TPM: 100, Algorithm: "fixed_window"}
	req := limits.CheckRequest{Subjects: oneSubject(l), Now: time.Now()}

	// Short reply.
	reply = fv.Ints(1, 2, 3)
	if _, err := v.Check(ctx, req); err == nil || !strings.Contains(err.Error(), "check reply has 3 items") {
		t.Fatalf("short reply: %v", err)
	}
	// Error reply.
	reply = fv.Err("ERR script")
	if _, err := v.Check(ctx, req); err == nil {
		t.Fatal("error reply must fail")
	}

	n := 0
	deny := func(limit string, idx, retry int64) limits.Decision {
		t.Helper()
		reply = checkReply(0, idx, limit, retry, 0)
		n++
		r := req
		r.Subjects = []limits.Subject{{Entity: limits.Entity{Kind: limits.KindKey, ID: "k" + string(rune('a'+n))}, Limits: l}}
		d, err := v.Check(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := deny("rpm", 9, 0); d.Allowed || d.Kind != core.ErrRateLimit || d.LimitName != "rpm" || d.Entity.ID != "" {
		t.Fatalf("idx out of range: %+v", d)
	}
	if d := deny("block", 1, 5000); d.Kind != core.ErrAccountSuspended || d.RetryAfter != 5*time.Second {
		t.Fatalf("block with retry: %+v", d)
	}
	// The block is now cached: the next check does not reach the script.
	reply = checkReply(1, 0, "", 0, 0)
	if d, err := v.Check(ctx, limits.CheckRequest{Subjects: []limits.Subject{{Entity: limits.Entity{Kind: limits.KindKey, ID: "kc"}, Limits: l}}, Now: time.Now()}); err != nil || d.Kind != core.ErrAccountSuspended {
		t.Fatalf("cached block: %+v %v", d, err)
	}
	if d := deny("tpm", 1, 1000); !strings.Contains(d.Message, "tokens per minute") || strings.Contains(d.Message, "remaining") {
		t.Fatalf("fixed window tpm message: %q", d.Message)
	}
	if d := deny("weird", 1, 0); d.Kind != core.ErrRateLimit {
		t.Fatalf("unknown limit: %+v", d)
	}

	// An integer-looking double in a numeric position is accepted.
	items := make([]string, 29)
	for i := range items {
		items[i] = fv.Int(0)
	}
	items[0] = fv.Bulk("1.0")
	reply = fv.Arr(items...)
	if d, err := v.Check(ctx, req); err != nil || !d.Allowed {
		t.Fatalf("float allowed flag: %+v %v", d, err)
	}

	// A check without subjects is allowed without touching the server.
	if d, err := v.Check(ctx, limits.CheckRequest{}); err != nil || !d.Allowed {
		t.Fatalf("empty check: %+v %v", d, err)
	}
	// Zero Now falls back to the wall clock.
	reply = checkReply(1, 0, "", 0, 0)
	if d, err := v.Check(ctx, limits.CheckRequest{Subjects: oneSubject(config.Limits{RPM: 10})}); err != nil || !d.Allowed {
		t.Fatalf("zero now: %+v %v", d, err)
	}
}

func TestFakeCheckPerEntity(t *testing.T) {
	ctx := context.Background()
	var entity func(r fv.Request) string
	var release string
	v, srv := fakeStore(t, func(r fv.Request) string {
		switch r.Name {
		case evn(sCheckEntity):
			return entity(r)
		case evn(sRelease):
			return release
		}
		return ""
	}, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.PerEntity = true })
	subs := []limits.Subject{
		{Entity: limits.Entity{Kind: limits.KindKey, ID: "a"}, Limits: config.Limits{RPM: 10}},
		{Entity: limits.Entity{Kind: limits.KindIP, ID: "b"}, Limits: config.Limits{RPM: 10}},
	}
	req := limits.CheckRequest{Subjects: subs, Now: time.Now()}

	entity = func(fv.Request) string { return fv.Ints(1) }
	if _, err := v.Check(ctx, req); err == nil {
		t.Fatal("short per-entity reply must fail")
	}

	// First entity allowed (and created: index write fails), second denied;
	// the release of the first fails (logged only).
	entity = func(r fv.Request) string {
		if strings.Contains(r.Args[0], "{key:a}") {
			return checkReply(1, 0, "", 0, 1)
		}
		return checkReply(0, 2, "rpm", 1000, 0)
	}
	release = fv.Err("ERR release")
	d, err := v.Check(ctx, req)
	if err != nil || d.Allowed || d.LimitName != "rpm" || d.Entity.ID != "b" {
		t.Fatalf("denied: %+v %v", d, err)
	}
	released := false
	for _, c := range srv.Calls() {
		if c.Name == evn(sRelease) {
			released = true
		}
	}
	if !released {
		t.Fatal("release not called")
	}
}
