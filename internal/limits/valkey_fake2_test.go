package limits_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	fv "github.com/patrickbrandao/white-wizard/internal/limits/storetest/fakevalkey"
)

var fkeyA = limits.Entity{Kind: limits.KindKey, ID: "a"}

// byName answers commands from a table; anything else is "-ERR".
func byName(m map[string]string) fv.Handler {
	return func(r fv.Request) string {
		if r.Name == "SCRIPT LOAD" {
			return ""
		}
		return m[r.Name]
	}
}

func TestFakeFinishEdges(t *testing.T) {
	ctx := context.Background()
	fin := fv.Ints(0, 0, 0, 1, 0, 0, 0)
	xadd := fv.Bulk("1-0")
	v, srv := fakeStore(t, func(r fv.Request) string {
		switch r.Name {
		case evn(sFinish):
			return fin
		case "XADD":
			return xadd
		}
		return ""
	})
	sub := []limits.Subject{{Entity: fkeyA, Limits: config.Limits{TPM: 100, TPD: 1000}}}

	// A reply with fewer than 7 items is skipped, not an error.
	fin = fv.Ints(1)
	if err := v.Finish(ctx, limits.FinishRequest{Subjects: sub}); err != nil {
		t.Fatalf("short finish reply: %v", err)
	}
	// The event write fails after the script answered.
	fin, xadd = fv.Ints(0, 0, 0, 1, 0, 0, 0), fv.Err("ERR xadd")
	if err := v.Finish(ctx, limits.FinishRequest{Subjects: sub, Now: time.Now()}); err == nil {
		t.Fatal("event failure must surface")
	}
	// An event that cannot be encoded (year > 9999) is dropped.
	before := len(srv.Calls())
	if err := v.Finish(ctx, limits.FinishRequest{Now: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("unencodable event: %v", err)
	}
	if len(srv.Calls()) != before {
		t.Fatal("nothing must be sent for an unencodable event without subjects")
	}
	// Zero Now falls back to the wall clock.
	xadd = fv.Bulk("1-1")
	if err := v.Finish(ctx, limits.FinishRequest{Subjects: sub}); err != nil {
		t.Fatalf("zero now: %v", err)
	}
}

func TestFakeEvents(t *testing.T) {
	ctx := context.Background()
	reply := ""
	v, _ := fakeStore(t, byName(map[string]string{}))
	_ = reply
	if _, err := v.Events(ctx, 10); err == nil {
		t.Fatal("XREVRANGE error must surface")
	}
	v2, srv := fakeStore(t, nil)
	srv.SetHandler(func(r fv.Request) string {
		if r.Name == "XREVRANGE" {
			if r.Args[3] != "COUNT" {
				t.Errorf("args %v", r.Args)
			}
			return fv.Arr(
				fv.Arr(fv.Bulk("1-0"), fv.Strs("d", "not json")),
				fv.Arr(fv.Bulk("2-0"), fv.Strs("d", `{"type":"request"}`)),
			)
		}
		return fv.OK()
	})
	evs, err := v2.Events(ctx, 20000)
	if err != nil || len(evs) != 1 || evs[0].Type != "request" {
		t.Fatalf("events %v %v", evs, err)
	}
	if got := srv.Calls()[len(srv.Calls())-1].Args[4]; got != "10000" {
		t.Fatalf("limit not capped: %v", srv.Calls())
	}
	if _, err := v2.Events(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := srv.Calls()[len(srv.Calls())-1].Args[4]; got != "1000" {
		t.Fatalf("default limit: %v", got)
	}
}

func TestFakeEventRingTrim(t *testing.T) {
	v, _ := fakeStore(t, nil, func(c *config.StoreConfig, _ *limits.ValkeyOptions) { c.EventStream = false })
	for i := range 10001 {
		if err := v.Finish(context.Background(), limits.FinishRequest{RequestID: "r" + time.Duration(i).String()}); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := v.Events(context.Background(), 20000)
	if err != nil || len(evs) != 5001 {
		t.Fatalf("ring after trim: %d %v", len(evs), err)
	}
}

func TestFakeStatusAndStrike(t *testing.T) {
	ctx := context.Background()
	status := fv.Err("ERR status")
	strike := fv.Err("ERR strike")
	zadd := fv.Int(1)
	v, _ := fakeStore(t, func(r fv.Request) string {
		switch r.Name {
		case evn(sSetStatus):
			return status
		case evn(sStrike):
			return strike
		case "ZADD", "ZREM", "XADD":
			return zadd
		}
		return ""
	})
	if err := v.Block(ctx, fkeyA, "r", time.Minute, "t"); err == nil {
		t.Fatal("set_status error must surface")
	}
	status = fv.Ints(1)
	zadd = fv.Err("ERR zadd")
	if err := v.Ban(ctx, fkeyA, "r", "t"); err == nil {
		t.Fatal("index error must surface")
	}

	if _, err := v.Strike(ctx, fkeyA, "r", time.Minute); err == nil {
		t.Fatal("strike error must surface")
	}
	strike = fv.Ints(1)
	if _, err := v.Strike(ctx, fkeyA, "r", time.Minute); err == nil || !strings.Contains(err.Error(), "strike reply") {
		t.Fatalf("short strike reply: %v", err)
	}
	strike = fv.Ints(2, 1)
	if n, err := v.Strike(ctx, fkeyA, "r", time.Minute); err == nil || n != 2 {
		t.Fatalf("follow-up failure: %d %v", n, err)
	}
}

func TestFakeReads(t *testing.T) {
	ctx := context.Background()
	h := map[string]string{"HGETALL": fv.Err("ERR hgetall"), "HMGET": fv.Arr(fv.Null(), fv.Null())}
	v, srv := fakeStore(t, func(r fv.Request) string {
		if r.Name == "SCRIPT LOAD" {
			return ""
		}
		return h[r.Name]
	})
	if _, err := v.Get(ctx, fkeyA); err == nil {
		t.Fatal("HGETALL error must surface")
	}
	h["HGETALL"], h["HMGET"] = fv.Map(), fv.Err("ERR hmget")
	if _, err := v.Get(ctx, fkeyA); err == nil {
		t.Fatal("HMGET error must surface")
	}

	// List: index read fails; entity read fails.
	if _, err := v.List(ctx, "", 10); err == nil {
		t.Fatal("ZREVRANGE error must surface")
	}
	h["ZREVRANGE"], h["HGETALL"] = fv.Strs("key:a"), fv.Err("ERR hgetall")
	if _, err := v.List(ctx, limits.StatusBanned, 10); err == nil {
		t.Fatal("entity read error inside List must surface")
	}
	_ = srv

	// Float-looking numbers in the hash are truncated.
	h["HGETALL"] = fv.Map("requests", "1.5", "strikes", "x", "since", "1000")
	h["HMGET"] = fv.Arr(fv.Null(), fv.Null())
	st, err := v.Get(ctx, fkeyA)
	if err != nil || st.Requests != 1 || st.Strikes != 0 {
		t.Fatalf("lenient parse: %+v %v", st, err)
	}
}

func TestFakeAttempts(t *testing.T) {
	ctx := context.Background()
	exec := fv.Err("ERR exec")
	v, _ := fakeStore(t, func(r fv.Request) string {
		switch r.Name {
		case "MULTI":
			return fv.OK()
		case "HINCRBY", "PEXPIRE":
			return "+QUEUED\r\n"
		case "EXEC":
			return exec
		}
		return ""
	})
	if _, err := v.Attempts(ctx, "c", "s", time.Minute); err == nil {
		t.Fatal("EXEC error must surface")
	}
	exec = fv.Arr()
	if _, err := v.Attempts(ctx, "c", "s", time.Minute); err == nil || !strings.Contains(err.Error(), "attempts reply") {
		t.Fatalf("empty EXEC: %v", err)
	}
}

func TestFakeAddAndSetCredits(t *testing.T) {
	ctx := context.Background()
	h := map[string]string{"HINCRBY": fv.Err("ERR hincrby"), "HSET": fv.Int(1)}
	v, _ := fakeStore(t, func(r fv.Request) string {
		if r.Name == "SCRIPT LOAD" {
			return ""
		}
		return h[r.Name]
	})
	if _, err := v.AddCredits(ctx, fkeyA, 1); err == nil {
		t.Fatal("HINCRBY error must surface")
	}
	h["HINCRBY"], h["HSETNX"], h["PERSIST"] = fv.Int(1), fv.Int(1), fv.Int(1)
	h["HGETALL"] = fv.Err("ERR hgetall")
	if _, err := v.AddCredits(ctx, fkeyA, 1); err == nil {
		t.Fatal("HGETALL error must surface")
	}
	if err := v.SetCredits(ctx, fkeyA, 1); err == nil {
		t.Fatal("index error must surface")
	}
}

func TestFakeResets(t *testing.T) {
	ctx := context.Background()
	h := map[string]string{}
	v, _ := fakeStore(t, func(r fv.Request) string {
		if r.Name == "SCRIPT LOAD" {
			return ""
		}
		if r.Name == evn(sResetCount) {
			return h["RESETCOUNT"]
		}
		return h[r.Name]
	})
	if err := v.Reset(ctx); err == nil {
		t.Fatal("SCAN error must surface")
	}
	if _, err := v.ResetConversations(ctx, ""); err == nil {
		t.Fatal("SCAN error must surface")
	}
	h["SCAN"] = fv.Arr(fv.Bulk("0"), fv.Strs("wwz:ent:{key:a}", "wwz:cfg:policies"))
	if err := v.Reset(ctx); err == nil {
		t.Fatal("UNLINK error must surface")
	}
	if _, err := v.ResetConversations(ctx, "c"); err == nil {
		t.Fatal("UNLINK error must surface")
	}

	if err := v.ResetCounters(ctx); err == nil {
		t.Fatal("ZSCAN error must surface")
	}
	h["ZSCAN"] = fv.Arr(fv.Bulk("0"), fv.Strs("key:a", "1"))
	if err := v.ResetCounters(ctx); err == nil {
		t.Fatal("reset_counters error must surface")
	}
}
