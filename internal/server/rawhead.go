package server

import (
	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// storeRawHead puts the head of the request, as it arrived, in the raw
// head slot of h when drv asks for it (driver.RawHeadWanter, spec 004
// "Cabeça crua"); otherwise it copies nothing. The header section is
// fasthttp's own copy of the bytes, set aside before it parses (and may
// normalise) the names, so spelling, order and repeats survive; the
// request line is rebuilt from the method, request target and protocol
// fasthttp kept as sent.
func storeRawHead(rh *fasthttp.RequestHeader, h driver.MapHeaders, drv driver.Driver) {
	if !driver.WantsRawHead(drv) {
		return
	}
	driver.SetRawHead(h, rh.Method(), rh.RequestURI(), rh.Protocol(), rh.RawHeaders())
}
