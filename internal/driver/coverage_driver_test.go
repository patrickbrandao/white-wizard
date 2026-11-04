package driver_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

func TestQueryHeader(t *testing.T) {
	if q := driver.Query(nil); len(q) != 0 {
		t.Errorf("nil headers: %v", q)
	}
	h := driver.MapHeaders{driver.QueryHeader: "alt=sse&key=k"}
	q := driver.Query(h)
	if q.Get("alt") != "sse" || q.Get("key") != "k" {
		t.Errorf("query: %v", q)
	}
}

func TestExtraMarkers(t *testing.T) {
	if _, ok := driver.AfterModel(nil); ok {
		t.Error("AfterModel(nil)")
	}
	if driver.BeforeAuth(nil) {
		t.Error("BeforeAuth(nil)")
	}
	e := &core.Error{Extra: map[string]any{
		driver.ExtraAfterModel: "gemini-x",
		driver.ExtraBeforeAuth: true,
	}}
	if n, ok := driver.AfterModel(e); !ok || n != "gemini-x" {
		t.Errorf("AfterModel = %q %v", n, ok)
	}
	if !driver.BeforeAuth(e) {
		t.Error("BeforeAuth false")
	}
	if _, ok := driver.AfterModel(&core.Error{}); ok {
		t.Error("AfterModel without extra")
	}
	if driver.BeforeAuth(&core.Error{}) {
		t.Error("BeforeAuth without extra")
	}
}

func TestRouteSlots(t *testing.T) {
	driver.SetRoute(nil, "GET", "/x") // no panic
	if m, p := driver.RouteOf(nil); m != "" || p != "" {
		t.Errorf("RouteOf(nil) = %q %q", m, p)
	}
	if m, p := driver.RouteOfRequest(nil); m != "" || p != "" {
		t.Errorf("RouteOfRequest(nil) = %q %q", m, p)
	}
	h := driver.MapHeaders{}
	driver.SetRoute(h, "POST", "/v1/responses")
	if m, p := driver.RouteOf(h); m != "POST" || p != "/v1/responses" {
		t.Errorf("RouteOf = %q %q", m, p)
	}
	if m, p := driver.RouteOfRequest(&core.Request{Headers: h}); m != "POST" || p != "/v1/responses" {
		t.Errorf("RouteOfRequest = %q %q", m, p)
	}
}

type missCapable struct{ driver.Driver }

func (missCapable) WriteRouteMiss(core.ResponseWriter, *core.Request, driver.RouteMiss, *core.Error) {
}

type wrapping struct {
	driver.Driver
	inner driver.Driver
}

func (w wrapping) Unwrap() driver.Driver { return w.inner }

func TestAsUnwrapsOutermostFirst(t *testing.T) {
	if _, ok := driver.As[driver.RouteMisser](nil); ok {
		t.Error("As(nil)")
	}
	if _, ok := driver.As[driver.RouteMisser](missCapable{}.Driver); ok {
		t.Error("nil inner driver")
	}
	var plain driver.Driver = wrapping{}
	if _, ok := driver.As[driver.RouteMisser](plain); ok {
		t.Error("wrapper over nil is no RouteMisser")
	}
	var lone driver.Driver = struct{ driver.Driver }{}
	if _, ok := driver.As[driver.RouteMisser](lone); ok {
		t.Error("plain driver is no RouteMisser")
	}
	w := wrapping{inner: missCapable{}}
	got, ok := driver.As[driver.RouteMisser](w)
	if !ok {
		t.Fatal("As did not find the wrapped RouteMisser")
	}
	if _, ok := got.(missCapable); !ok {
		t.Errorf("found %T", got)
	}
	if _, ok := driver.As[driver.Unwrapper](w); !ok {
		t.Error("outermost Unwrapper not found")
	}
}

func TestDefaultConventionsGemini(t *testing.T) {
	if c := driver.DefaultConventions("gemini"); c != (driver.Conventions{AuthFirst: true}) {
		t.Errorf("gemini: %+v", c)
	}
}

func TestNewUnknownAndNames(t *testing.T) {
	_, err := driver.New("nope", nil)
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("err = %v", err)
	}
	names := driver.Names()
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("Names not sorted: %v", names)
		}
	}
}
