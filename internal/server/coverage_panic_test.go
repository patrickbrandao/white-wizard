package server

import (
	"context"
	"testing"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// panicDriver is the OpenAI driver whose request parser blows up: the data
// plane must turn the panic into the provider's 500, not kill the process.
type panicDriver struct{ driver.Driver }

func (panicDriver) ParseRequest(driver.Kind, []byte, driver.Headers) (*core.Request, *core.Error) {
	panic("boom")
}

func init() {
	driver.Register("fake-panic", func(opts map[string]string) (driver.Driver, error) {
		d, err := driver.New("openai", opts)
		if err != nil {
			return nil, err
		}
		return panicDriver{d}, nil
	})
}

func TestHandleRecoversFromDriverPanic(t *testing.T) {
	h := start(t, "drivers: [{name: fake-panic}]\n"+fastCfg, nil)
	resp := h.do("POST", "/v1/chat/completions", oaiAuth, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode() != 500 {
		t.Fatalf("status %d: %s", resp.StatusCode(), resp.Body())
	}
	// The server keeps serving afterwards.
	resp = h.do("POST", "/v1/chat/completions", oaiAuth, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode() != 500 {
		t.Fatalf("second status %d", resp.StatusCode())
	}
}

func TestQueryStringReachesTheDriver(t *testing.T) {
	h := start(t, fastCfg, nil)
	resp := h.do("GET", "/v1/models?limit=1", oaiAuth, "")
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode(), resp.Body())
	}
}

func TestFastWriterUnhandledFaults(t *testing.T) {
	w := newFastWriter(&fasthttp.RequestCtx{}, func() {}, 0, false)
	if w.Fault(core.TransportSlowHeaders) {
		t.Fatal("slow headers is written as the normal error")
	}
	w = newFastWriter(&fasthttp.RequestCtx{}, func() {}, 0, false)
	if w.Fault(core.TransportFault("unknown")) {
		t.Fatal("an unknown fault is not handled")
	}
}

func TestListenDefaultsListenersToGOMAXPROCS(t *testing.T) {
	s, err := New(newEngine(t, "server: {reuseport: true}\n"+latCfg), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	ls, err := s.Listen("127.0.0.1:0")
	if err != nil || len(ls) == 0 {
		t.Fatalf("listen: %v %d", err, len(ls))
	}
	for _, l := range ls {
		_ = l.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}
