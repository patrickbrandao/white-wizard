package claudecode

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// Header is one header of a raw request head, with the name spelled as
// received and the value without the surrounding optional whitespace.
type Header struct {
	Name  string
	Value string
}

// Head is the raw head of an HTTP/1.1 request: the request line as received
// and the headers in order, with repetitions.
type Head struct {
	Method  string
	Target  string // request target with the query, e.g. /v1/messages?beta=true
	Proto   string // e.g. HTTP/1.1
	Headers []Header
}

// ErrNoHeadEnd is returned by [ParseHead] when raw has no "\r\n\r\n".
var ErrNoHeadEnd = errors.New("raw request has no end of head (CRLF CRLF)")

// ParseHead parses the head of a raw HTTP/1.1 request (lines ended by CRLF,
// head ended by an empty line) and returns it with the bytes after it.
func ParseHead(raw []byte) (Head, []byte, error) {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 {
		return Head{}, nil, ErrNoHeadEnd
	}
	lines := strings.Split(string(raw[:end]), "\r\n")
	parts := strings.Split(lines[0], " ")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Head{}, nil, fmt.Errorf("malformed request line %q", lines[0])
	}
	h := Head{Method: parts[0], Target: parts[1], Proto: parts[2], Headers: make([]Header, 0, len(lines)-1)}
	for _, l := range lines[1:] {
		name, value, ok := strings.Cut(l, ":")
		if !ok || name == "" || strings.TrimRight(name, " \t") != name {
			return Head{}, nil, fmt.Errorf("malformed header line %q", l)
		}
		h.Headers = append(h.Headers, Header{Name: name, Value: strings.Trim(value, " \t")})
	}
	return h, raw[end+4:], nil
}

// Get returns the value of the first header whose name matches name
// without regard to case.
func (h Head) Get(name string) (string, bool) {
	for _, hd := range h.Headers {
		if strings.EqualFold(hd.Name, name) {
			return hd.Value, true
		}
	}
	return "", false
}

// Append appends the head as sent on the wire: the request line, each
// header as "Name: value" and the empty line, all ended by CRLF.
func (h Head) Append(dst []byte) []byte {
	dst = append(dst, h.Method...)
	dst = append(dst, ' ')
	dst = append(dst, h.Target...)
	dst = append(dst, ' ')
	dst = append(dst, h.Proto...)
	dst = append(dst, '\r', '\n')
	for _, hd := range h.Headers {
		dst = append(dst, hd.Name...)
		dst = append(dst, ':', ' ')
		dst = append(dst, hd.Value...)
		dst = append(dst, '\r', '\n')
	}
	return append(dst, '\r', '\n')
}
