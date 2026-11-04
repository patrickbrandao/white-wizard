package driver

import (
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// RawHeadWanter is implemented by a driver that needs the head of the
// request exactly as it arrived (spec 004 "Cabeça crua", ADR 0074): the
// request line and the header lines in their order, spelling and
// repetitions, which the lower-cased Headers lose. The answer is per
// mount (the driver instance New built from drivers[].options), and the
// server copies the head only when it is true: without it the hot path
// pays nothing (spec 014). The server finds it with As, so a wrapper
// keeps it.
type RawHeadWanter interface {
	WantsRawHead() bool
}

// WantsRawHead reports whether d (or a driver it wraps) asks for the raw
// head.
func WantsRawHead(d Driver) bool {
	w, ok := As[RawHeadWanter](d)
	return ok && w.WantsRawHead()
}

// RawHead is the head of a request as it arrived on the connection.
type RawHead struct {
	// Line is the request line without its line ending: the method, the
	// request target as sent (path and query, not decoded) and the
	// protocol, separated by one space ("POST /v1/messages?beta=true
	// HTTP/1.1"). Empty lines before it, which HTTP/1.1 lets a server
	// skip, are not part of it.
	Line string
	// Block is the header section byte for byte: every header line in
	// arrival order with its own line ending, then the empty line that
	// ends the head. Transfer-Encoding, Host, Content-Length and the like
	// are in it as they were sent.
	Block string
}

// RawHeader is one header line of a RawHead.
type RawHeader struct {
	// Name is the field name as sent, without normalisation.
	Name string
	// Value is what follows the colon, without the optional whitespace
	// (SP and HTAB) around it.
	Value string
}

// Headers splits Block into its header lines, in arrival order and with
// repeats kept. A line without a colon is a header with that whole line as
// Name and an empty Value. It allocates: call it only where the raw head
// is needed.
func (r RawHead) Headers() []RawHeader {
	out := make([]RawHeader, 0, strings.Count(r.Block, "\n"))
	for rest := r.Block; rest != ""; {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			break // the empty line that ends the head
		}
		name, value, _ := strings.Cut(line, ":")
		out = append(out, RawHeader{Name: name, Value: strings.Trim(value, " \t")})
	}
	return out
}

// rawHeadHeader is the request-header slot that carries the raw head, as
// Line + "\n" + Block. Like the account slots, its NUL prefix is not a
// valid header name, so a client cannot send it.
const rawHeadHeader = "\x00wwz-raw-head"

// SetRawHead stores the head of a request in h: the method, the request
// target and the protocol of its request line, and its header section
// byte for byte (see RawHead.Block). It copies the bytes, in one
// allocation, so they may be reused once it returns.
func SetRawHead(h MapHeaders, method, target, proto, block []byte) {
	if h == nil {
		return
	}
	var b strings.Builder
	b.Grow(len(method) + len(target) + len(proto) + len(block) + 3)
	b.Write(method)
	b.WriteByte(' ')
	b.Write(target)
	b.WriteByte(' ')
	b.Write(proto)
	b.WriteByte('\n')
	b.Write(block)
	h[rawHeadHeader] = b.String()
}

// RawHeadOf reads the raw head SetRawHead stored in h; false when the
// server stored none (the driver did not ask for it).
func RawHeadOf(h Headers) (RawHead, bool) {
	if h == nil {
		return RawHead{}, false
	}
	return parseRawHead(h.Get(rawHeadHeader))
}

// RawHeadOfRequest is RawHeadOf over the headers of req (nil-safe).
func RawHeadOfRequest(req *core.Request) (RawHead, bool) {
	if req == nil {
		return RawHead{}, false
	}
	return parseRawHead(req.Header(rawHeadHeader))
}

func parseRawHead(s string) (RawHead, bool) {
	line, block, ok := strings.Cut(s, "\n")
	if !ok {
		return RawHead{}, false
	}
	return RawHead{Line: line, Block: block}, true
}
