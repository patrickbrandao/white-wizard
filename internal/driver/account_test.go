package driver_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

func TestAccountRoundTrip(t *testing.T) {
	if got := driver.AccountOf(nil); got != (driver.Account{}) {
		t.Errorf("nil request: %+v", got)
	}
	if got := driver.AccountOf(&core.Request{}); got != (driver.Account{}) {
		t.Errorf("no headers: %+v", got)
	}
	driver.SetAccount(nil, driver.Account{Key: "k"}) // no panic
	h := driver.MapHeaders{"authorization": "Bearer k"}
	want := driver.Account{Key: "k", Account: "acme", Project: "web"}
	driver.SetAccount(h, want)
	if got := driver.AccountOf(&core.Request{Headers: h}); got != want {
		t.Errorf("AccountOf = %+v, want %+v", got, want)
	}
	driver.SetAccount(h, driver.Account{Key: "k2"})
	if got := driver.AccountOf(&core.Request{Headers: h}); got != (driver.Account{Key: "k2"}) {
		t.Errorf("SetAccount does not replace: %+v", got)
	}
	if h.Get("authorization") != "Bearer k" {
		t.Error("SetAccount touched a real header")
	}
}
