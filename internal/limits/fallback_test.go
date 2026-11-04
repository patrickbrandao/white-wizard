package limits_test

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// closedAddr returns a local address nobody listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

type transitions struct {
	mu  sync.Mutex
	got []bool
}

func (tr *transitions) add(open bool) {
	tr.mu.Lock()
	tr.got = append(tr.got, open)
	tr.mu.Unlock()
}

func (tr *transitions) list() []bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]bool(nil), tr.got...)
}

func downStore(t *testing.T, mode string) (*limits.Fallback, *transitions) {
	t.Helper()
	cfg := config.StoreConfig{Kind: "valkey", Addr: []string{closedAddr(t)}, ReadTimeout: 5 * time.Millisecond, OnDown: mode, Prefix: "wwz"}
	tr := &transitions{}
	f := limits.OpenValkey(context.Background(), cfg, limits.ValkeyOptions{Heartbeat: -1}, limits.FallbackOptions{OnChange: tr.add, ProbeInterval: 20 * time.Millisecond})
	t.Cleanup(func() { _ = f.Close() })
	return f, tr
}

func TestFallbackValkeyDownAtBoot(t *testing.T) {
	ctx := context.Background()
	sub := []limits.Subject{{Entity: keyA, Limits: config.Limits{RPM: 2}}}
	req := limits.CheckRequest{Subjects: sub, Now: time.Now()}
	for _, mode := range []string{"allow", "deny", "local"} {
		t.Run(mode, func(t *testing.T) {
			f, tr := downStore(t, mode)
			if !f.CircuitOpen() || f.Kind() != "valkey" {
				t.Fatalf("circuit open = %v, kind %s", f.CircuitOpen(), f.Kind())
			}
			if got := tr.list(); len(got) != 1 || !got[0] {
				t.Fatalf("transitions = %v, want [true]", got)
			}
			allowed := 0
			for range 4 {
				d, err := f.Check(ctx, req)
				if err != nil {
					t.Fatalf("Check returned an error with Valkey down: %v", err)
				}
				if d.Allowed {
					allowed++
				} else if mode == "deny" && (d.Kind != core.ErrOverloaded || d.LimitName != "store_down" || d.RetryAfter != time.Second) {
					t.Fatalf("deny decision: %+v", d)
				}
			}
			want := map[string]int{"allow": 4, "deny": 0, "local": 2}[mode]
			if allowed != want {
				t.Fatalf("on_down %s allowed %d of 4, want %d", mode, allowed, want)
			}
			if err := f.Finish(ctx, limits.FinishRequest{Subjects: sub, Now: time.Now(), Status: 200}); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if n, err := f.Attempts(ctx, "c", "s", time.Hour); err != nil || n != 1 {
				t.Fatalf("Attempts = %d, %v (internal memory)", n, err)
			}
			if n, err := f.Strike(ctx, keyA, "r", time.Hour); err != nil || n != 1 {
				t.Fatalf("Strike = %d, %v (internal memory)", n, err)
			}
			if err := f.Ban(ctx, keyA, "r", "x"); !errors.Is(err, limits.ErrStoreDown) {
				t.Fatalf("Ban with Valkey down = %v, want ErrStoreDown", err)
			}
			if _, err := f.Get(ctx, keyA); !errors.Is(err, limits.ErrStoreDown) {
				t.Fatalf("Get = %v", err)
			}
			if _, err := f.List(ctx, "", 0); !errors.Is(err, limits.ErrStoreDown) {
				t.Fatalf("List = %v", err)
			}
			if _, err := f.Events(ctx, 0); !errors.Is(err, limits.ErrStoreDown) {
				t.Fatalf("Events = %v", err)
			}
			for name, err := range map[string]error{
				"Block": f.Block(ctx, keyA, "r", 0, "x"), "Unblock": f.Unblock(ctx, keyA), "Unban": f.Unban(ctx, keyA),
				"SetCredits": f.SetCredits(ctx, keyA, 1), "Reset": f.Reset(ctx), "ResetCounters": f.ResetCounters(ctx),
			} {
				if !errors.Is(err, limits.ErrStoreDown) {
					t.Fatalf("%s = %v, want ErrStoreDown", name, err)
				}
			}
			if _, err := f.ResetConversations(ctx, ""); !errors.Is(err, limits.ErrStoreDown) {
				t.Fatalf("ResetConversations = %v", err)
			}
		})
	}
}

// proxy is a TCP forwarder that can drop and restore the path to Valkey.
type proxy struct {
	t      *testing.T
	target string
	addr   string
	mu     sync.Mutex
	ln     net.Listener
	conns  []net.Conn
}

func newProxy(t *testing.T, target string) *proxy {
	p := &proxy{t: t, target: target}
	p.start("127.0.0.1:0")
	t.Cleanup(p.stop)
	return p
}

func (p *proxy) start(addr string) {
	var ln net.Listener
	var err error
	for range 50 { // the port may linger briefly after stop
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		p.t.Fatal(err)
	}
	p.mu.Lock()
	p.ln, p.addr = ln, ln.Addr().String()
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", p.target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, u)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(u, c); _ = u.Close() }()
			go func() { _, _ = io.Copy(c, u); _ = c.Close() }()
		}
	}()
}

func (p *proxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Valkey goes away and comes back: Check never errors, the circuit opens
// after the failures and closes on the first successful PING.
func TestFallbackOutageAndRecovery(t *testing.T) {
	addr, db := valkeyEnv(t)
	for _, mode := range []string{"allow", "deny", "local"} {
		t.Run(mode, func(t *testing.T) {
			p := newProxy(t, addr)
			cfg := testStoreConfig(p.addr)
			cfg.ReadTimeout = 200 * time.Millisecond
			cfg.OnDown = mode
			tr := &transitions{}
			f := limits.OpenValkey(context.Background(), cfg, limits.ValkeyOptions{DB: db, Heartbeat: -1}, limits.FallbackOptions{OnChange: tr.add, ProbeInterval: 20 * time.Millisecond})
			t.Cleanup(func() { _ = f.Close() })
			if f.CircuitOpen() {
				t.Fatal("circuit open with Valkey up")
			}
			flush(t, f.Valkey())
			ctx := context.Background()
			req := limits.CheckRequest{Subjects: []limits.Subject{{Entity: keyA, Limits: config.Limits{RPM: 1000}}}}
			check := func() limits.Decision {
				t.Helper()
				req.Now = time.Now()
				d, err := f.Check(ctx, req)
				if err != nil {
					t.Fatalf("Check error: %v", err)
				}
				return d
			}
			if d := check(); !d.Allowed || !d.State.Present {
				t.Fatalf("Valkey up: %+v", d)
			}

			p.stop()
			for range 5 {
				d := check()
				if mode == "deny" && f.CircuitOpen() && d.Allowed {
					t.Fatalf("deny mode admitted with the circuit open: %+v", d)
				}
				if err := f.Finish(ctx, limits.FinishRequest{Subjects: req.Subjects, Now: time.Now(), Status: 200}); err != nil {
					t.Fatalf("Finish error: %v", err)
				}
			}
			if !f.CircuitOpen() {
				t.Fatal("circuit still closed after 5 failures")
			}
			if _, err := f.Get(ctx, keyA); !errors.Is(err, limits.ErrStoreDown) {
				t.Fatalf("admin with circuit open = %v", err)
			}

			p.start(p.addr)
			waitFor(t, "circuit to close", func() bool { return !f.CircuitOpen() })
			if d := check(); !d.Allowed || !d.State.Present {
				t.Fatalf("after recovery: %+v", d)
			}
			st, err := f.Get(ctx, keyA)
			if err != nil || st.Requests < 2 {
				t.Fatalf("Valkey state after recovery = %+v, %v", st, err)
			}
			if got := tr.list(); len(got) != 3 || got[0] || !got[1] || got[2] {
				t.Fatalf("transitions = %v, want [false true false]", got)
			}
		})
	}
}
