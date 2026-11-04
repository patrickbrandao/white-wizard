package claudecode

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// Kind is the type of a JSON [Value].
type Kind uint8

// JSON value kinds.
const (
	KindNull Kind = iota
	KindBool
	KindNumber
	KindString
	KindArray
	KindObject
)

// Member is one key/value pair of a JSON object, in insertion order.
type Member struct {
	Key   string
	Value Value
}

// Value is a JSON value that keeps the order of object keys, like an
// ECMAScript object built by JSON.parse or by an object literal. The zero
// Value is null.
//
// Strings hold UTF-8; a lone UTF-16 surrogate (from a "\udxxx" escape
// without its pair) is kept as its 3-byte WTF-8 form, so it round-trips to
// the same escape, as JSON.stringify does.
type Value struct {
	kind Kind
	b    bool
	n    float64
	s    string
	arr  []Value
	obj  []Member
}

// Null returns the JSON null.
func Null() Value { return Value{} }

// Bool returns a JSON boolean.
func Bool(b bool) Value { return Value{kind: KindBool, b: b} }

// Number returns a JSON number (an IEEE 754 double, like an ECMAScript Number).
func Number(f float64) Value { return Value{kind: KindNumber, n: f} }

// Int returns a JSON number from an integer.
func Int(i int64) Value { return Number(float64(i)) }

// String returns a JSON string.
func String(s string) Value { return Value{kind: KindString, s: s} }

// Array returns a JSON array with the given elements. Array() is [].
func Array(vs ...Value) Value {
	if vs == nil {
		vs = []Value{}
	}
	return Value{kind: KindArray, arr: vs}
}

// Object returns a JSON object with the given members, in order.
// Object() is {}.
func Object(ms ...Member) Value {
	if ms == nil {
		ms = []Member{}
	}
	return Value{kind: KindObject, obj: ms}
}

// M builds an object [Member].
func M(key string, v Value) Member { return Member{Key: key, Value: v} }

// Kind returns the kind of v.
func (v Value) Kind() Kind { return v.kind }

// Truth returns the boolean of a KindBool value (false otherwise).
func (v Value) Truth() bool { return v.b }

// Num returns the number of a KindNumber value (0 otherwise).
func (v Value) Num() float64 { return v.n }

// Str returns the string of a KindString value ("" otherwise).
func (v Value) Str() string { return v.s }

// Elems returns the elements of a KindArray value (nil otherwise).
func (v Value) Elems() []Value { return v.arr }

// Members returns the members of a KindObject value, in order (nil otherwise).
func (v Value) Members() []Member { return v.obj }

// Keys returns the keys of a KindObject value, in order.
func (v Value) Keys() []string {
	keys := make([]string, len(v.obj))
	for i, m := range v.obj {
		keys[i] = m.Key
	}
	return keys
}

// Get returns the member named key of a KindObject value.
func (v Value) Get(key string) (Value, bool) {
	for _, m := range v.obj {
		if m.Key == key {
			return m.Value, true
		}
	}
	return Value{}, false
}

// Marshal serializes v exactly as ECMAScript JSON.stringify(v) does with no
// replacer and no indentation.
func Marshal(v Value) []byte { return AppendJS(nil, v) }

// AppendJS appends the JSON.stringify serialization of v to dst.
func AppendJS(dst []byte, v Value) []byte {
	switch v.kind {
	case KindBool:
		if v.b {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case KindNumber:
		return AppendNumber(dst, v.n)
	case KindString:
		return AppendString(dst, v.s)
	case KindArray:
		dst = append(dst, '[')
		for i, e := range v.arr {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendJS(dst, e)
		}
		return append(dst, ']')
	case KindObject:
		dst = append(dst, '{')
		for i, m := range v.obj {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendString(dst, m.Key)
			dst = append(dst, ':')
			dst = AppendJS(dst, m.Value)
		}
		return append(dst, '}')
	default:
		return append(dst, "null"...)
	}
}

const hexDigits = "0123456789abcdef"

// AppendString appends s quoted as JSON.stringify quotes a string: only
// '"', '\\' and the controls below U+0020 are escaped (\b \t \n \f \r short,
// the others \u00xx in lowercase hex) and a lone surrogate becomes \udxxx;
// '<', '>', '&', '/', U+2028, U+2029 and all other non-ASCII stay raw.
// Bytes that are not UTF-8 (and not a WTF-8 surrogate) become U+FFFD.
func AppendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c < utf8.RuneSelf {
			i++
			continue
		}
		if c < utf8.RuneSelf {
			dst = append(dst, s[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\r':
				dst = append(dst, '\\', 'r')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError || size != 1 {
			i += size
			continue
		}
		dst = append(dst, s[start:i]...)
		if u, ok := wtf8Surrogate(s[i:]); ok {
			dst = append(dst, '\\', 'u',
				hexDigits[u>>12], hexDigits[(u>>8)&0xf], hexDigits[(u>>4)&0xf], hexDigits[u&0xf])
			i += 3
		} else {
			dst = append(dst, "�"...)
			i++
		}
		start = i
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// wtf8Surrogate decodes a surrogate code unit encoded as 3-byte WTF-8
// (ED A0–BF 80–BF) at the start of s.
func wtf8Surrogate(s string) (uint16, bool) {
	if len(s) < 3 || s[0] != 0xED || s[1] < 0xA0 || s[1] > 0xBF || s[2] < 0x80 || s[2] > 0xBF {
		return 0, false
	}
	return 0xD000 | uint16(s[1]&0x3F)<<6 | uint16(s[2]&0x3F), true
}

// AppendNumber appends f formatted as ECMAScript Number.prototype.toString
// (shortest round-trip digits; integers without ".0"; exponent form from
// 1e21 and below 1e-6; -0 as "0"); NaN and ±Infinity become null, as in
// JSON.stringify.
func AppendNumber(dst []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(dst, "null"...)
	}
	if f == 0 {
		return append(dst, '0')
	}
	if f < 0 {
		dst = append(dst, '-')
		f = -f
	}
	var buf [32]byte
	e := strconv.AppendFloat(buf[:0], f, 'e', -1, 64) // d.ddde±xx
	var digits [24]byte
	k := 0
	j := 0
	for ; e[j] != 'e'; j++ {
		if e[j] != '.' {
			digits[k] = e[j]
			k++
		}
	}
	exp := 0
	neg := e[j+1] == '-'
	for _, c := range e[j+2:] {
		exp = exp*10 + int(c-'0')
	}
	if neg {
		exp = -exp
	}
	n := exp + 1
	d := digits[:k]
	switch {
	case k <= n && n <= 21:
		dst = append(dst, d...)
		for range n - k {
			dst = append(dst, '0')
		}
	case 0 < n && n <= 21:
		dst = append(dst, d[:n]...)
		dst = append(dst, '.')
		dst = append(dst, d[n:]...)
	case -6 < n && n <= 0:
		dst = append(dst, '0', '.')
		for range -n {
			dst = append(dst, '0')
		}
		dst = append(dst, d...)
	default:
		dst = append(dst, d[0])
		if k > 1 {
			dst = append(dst, '.')
			dst = append(dst, d[1:]...)
		}
		dst = append(dst, 'e')
		if n-1 < 0 {
			dst = append(dst, '-')
			dst = strconv.AppendInt(dst, int64(1-n), 10)
		} else {
			dst = append(dst, '+')
			dst = strconv.AppendInt(dst, int64(n-1), 10)
		}
	}
	return dst
}

// ErrDuplicateKey is wrapped by [Parse] when an object repeats a key.
var ErrDuplicateKey = errors.New("duplicate key")

const maxDepth = 512

// Parse decodes strict JSON (RFC 8259) in UTF-8 into an ordered [Value].
// Unlike JSON.parse it refuses an object that repeats a key
// ([ErrDuplicateKey]), since such a body cannot be the output of
// JSON.stringify. Numbers become doubles, as in JSON.parse.
func Parse(data []byte) (Value, error) {
	p := parser{data: data}
	p.ws()
	v, err := p.value(0)
	if err != nil {
		return Value{}, err
	}
	p.ws()
	if p.i != len(p.data) {
		return Value{}, p.errorf("unexpected data after the value")
	}
	return v, nil
}

type parser struct {
	data []byte
	i    int
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("json at byte %d: %s", p.i, fmt.Sprintf(format, args...))
}

func (p *parser) ws() {
	for p.i < len(p.data) {
		switch p.data[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (Value, error) {
	if depth > maxDepth {
		return Value{}, p.errorf("nesting deeper than %d", maxDepth)
	}
	if p.i >= len(p.data) {
		return Value{}, p.errorf("unexpected end of input")
	}
	switch c := p.data[p.i]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		s, err := p.str()
		return String(s), err
	case c == 't':
		return Bool(true), p.literal("true")
	case c == 'f':
		return Bool(false), p.literal("false")
	case c == 'n':
		return Null(), p.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		return Value{}, p.errorf("unexpected character %q", c)
	}
}

func (p *parser) literal(lit string) error {
	if len(p.data)-p.i < len(lit) || string(p.data[p.i:p.i+len(lit)]) != lit {
		return p.errorf("invalid literal")
	}
	p.i += len(lit)
	return nil
}

func (p *parser) object(depth int) (Value, error) {
	p.i++ // {
	ms := []Member{}
	p.ws()
	if p.i < len(p.data) && p.data[p.i] == '}' {
		p.i++
		return Object(ms...), nil
	}
	for {
		p.ws()
		if p.i >= len(p.data) || p.data[p.i] != '"' {
			return Value{}, p.errorf("expected an object key")
		}
		at := p.i
		key, err := p.str()
		if err != nil {
			return Value{}, err
		}
		for _, m := range ms {
			if m.Key == key {
				p.i = at
				return Value{}, fmt.Errorf("%w %q", ErrDuplicateKey, key)
			}
		}
		p.ws()
		if p.i >= len(p.data) || p.data[p.i] != ':' {
			return Value{}, p.errorf("expected ':'")
		}
		p.i++
		p.ws()
		v, err := p.value(depth + 1)
		if err != nil {
			return Value{}, err
		}
		ms = append(ms, Member{Key: key, Value: v})
		p.ws()
		if p.i < len(p.data) && p.data[p.i] == ',' {
			p.i++
			continue
		}
		if p.i < len(p.data) && p.data[p.i] == '}' {
			p.i++
			return Object(ms...), nil
		}
		return Value{}, p.errorf("expected ',' or '}'")
	}
}

func (p *parser) array(depth int) (Value, error) {
	p.i++ // [
	vs := []Value{}
	p.ws()
	if p.i < len(p.data) && p.data[p.i] == ']' {
		p.i++
		return Array(vs...), nil
	}
	for {
		p.ws()
		v, err := p.value(depth + 1)
		if err != nil {
			return Value{}, err
		}
		vs = append(vs, v)
		p.ws()
		if p.i < len(p.data) && p.data[p.i] == ',' {
			p.i++
			continue
		}
		if p.i < len(p.data) && p.data[p.i] == ']' {
			p.i++
			return Array(vs...), nil
		}
		return Value{}, p.errorf("expected ',' or ']'")
	}
}

func (p *parser) str() (string, error) {
	p.i++ // opening quote
	start := p.i
	var out []byte
	for p.i < len(p.data) {
		c := p.data[p.i]
		switch {
		case c == '"':
			s := string(p.data[start:p.i])
			if out != nil {
				s = string(append(out, p.data[start:p.i]...))
			}
			p.i++
			return s, nil
		case c < 0x20:
			return "", p.errorf("control character in string")
		case c == '\\':
			out = append(out, p.data[start:p.i]...)
			var err error
			if out, err = p.escape(out); err != nil {
				return "", err
			}
			start = p.i
		case c < utf8.RuneSelf:
			p.i++
		default:
			r, size := utf8.DecodeRune(p.data[p.i:])
			if r == utf8.RuneError && size == 1 {
				return "", p.errorf("invalid UTF-8")
			}
			p.i += size
		}
	}
	return "", p.errorf("unterminated string")
}

// escape decodes the escape at p.i (a backslash) and appends it to out.
func (p *parser) escape(out []byte) ([]byte, error) {
	if p.i+1 >= len(p.data) {
		return nil, p.errorf("unterminated escape")
	}
	c := p.data[p.i+1]
	p.i += 2
	switch c {
	case '"', '\\', '/':
		return append(out, c), nil
	case 'b':
		return append(out, '\b'), nil
	case 'f':
		return append(out, '\f'), nil
	case 'n':
		return append(out, '\n'), nil
	case 'r':
		return append(out, '\r'), nil
	case 't':
		return append(out, '\t'), nil
	case 'u':
		u, err := p.hex4()
		if err != nil {
			return nil, err
		}
		if u >= 0xD800 && u <= 0xDBFF && p.i+1 < len(p.data) && p.data[p.i] == '\\' && p.data[p.i+1] == 'u' {
			save := p.i
			p.i += 2
			lo, err := p.hex4()
			if err != nil {
				return nil, err
			}
			if lo >= 0xDC00 && lo <= 0xDFFF {
				r := 0x10000 + (rune(u)-0xD800)<<10 + (rune(lo) - 0xDC00)
				return utf8.AppendRune(out, r), nil
			}
			p.i = save
		}
		if u >= 0xD800 && u <= 0xDFFF {
			// Lone surrogate: keep its WTF-8 form.
			return append(out, 0xED, byte(0x80|(u>>6)&0x3F), byte(0x80|u&0x3F)), nil //nolint:gosec // both masked to 6 bits
		}
		return utf8.AppendRune(out, rune(u)), nil
	default:
		p.i -= 2
		return nil, p.errorf("invalid escape")
	}
}

func (p *parser) hex4() (uint16, error) {
	if len(p.data)-p.i < 4 {
		return 0, p.errorf("short \\u escape")
	}
	var u uint16
	for _, c := range p.data[p.i : p.i+4] {
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, p.errorf("invalid \\u escape")
		}
		u = u<<4 | uint16(d)
	}
	p.i += 4
	return u, nil
}

func (p *parser) number() (Value, error) {
	start := p.i
	if p.data[p.i] == '-' {
		p.i++
	}
	switch {
	case p.i < len(p.data) && p.data[p.i] == '0':
		p.i++
	case p.i < len(p.data) && p.data[p.i] >= '1' && p.data[p.i] <= '9':
		p.digits()
	default:
		return Value{}, p.errorf("invalid number")
	}
	if p.i < len(p.data) && p.data[p.i] == '.' {
		p.i++
		if p.digits() == 0 {
			return Value{}, p.errorf("invalid number")
		}
	}
	if p.i < len(p.data) && (p.data[p.i] == 'e' || p.data[p.i] == 'E') {
		p.i++
		if p.i < len(p.data) && (p.data[p.i] == '+' || p.data[p.i] == '-') {
			p.i++
		}
		if p.digits() == 0 {
			return Value{}, p.errorf("invalid number")
		}
	}
	// The grammar is already checked; out of range gives ±Inf, like JSON.parse.
	f, _ := strconv.ParseFloat(string(p.data[start:p.i]), 64)
	return Number(f), nil
}

func (p *parser) digits() int {
	n := 0
	for p.i < len(p.data) && p.data[p.i] >= '0' && p.data[p.i] <= '9' {
		p.i++
		n++
	}
	return n
}
