package oai

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"unicode/utf8"
)

// Obj is a JSON object that keeps its keys in insertion order. The real
// providers never sort keys (spec 006 16.7), so every object the dialect
// writes is an Obj, never a map[string]any marshalled by encoding/json.
type Obj []Field

// Field is one key and value of an Obj.
type Field struct {
	Key string
	Val any
}

// O builds an Obj from alternating keys and values: O("a", 1, "b", 2).
func O(kv ...any) Obj {
	o := make(Obj, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		o = append(o, Field{Key: kv[i].(string), Val: kv[i+1]})
	}
	return o
}

// Get returns the value of k.
func (o Obj) Get(k string) (any, bool) {
	for _, f := range o {
		if f.Key == k {
			return f.Val, true
		}
	}
	return nil, false
}

// Set replaces the value of k in place, or appends k at the end.
func (o *Obj) Set(k string, v any) {
	for i := range *o {
		if (*o)[i].Key == k {
			(*o)[i].Val = v
			return
		}
	}
	*o = append(*o, Field{Key: k, Val: v})
}

// Delete removes k (a no-op when absent).
func (o *Obj) Delete(k string) {
	for i := range *o {
		if (*o)[i].Key == k {
			*o = append((*o)[:i], (*o)[i+1:]...)
			return
		}
	}
}

// Merge sets every key of m, in sorted key order so the output is stable;
// keys already present keep their position.
func (o *Obj) Merge(m map[string]any) {
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o.Set(k, m[k])
	}
}

// Reorder returns o with the listed keys first, in that order, followed by
// the remaining keys in their current order. Listed keys that are absent
// are skipped. It is the building block of Hooks.Shape.
func (o Obj) Reorder(keys ...string) Obj {
	out := make(Obj, 0, len(o))
	used := make([]bool, len(o))
	for _, k := range keys {
		for i, f := range o {
			if !used[i] && f.Key == k {
				out = append(out, f)
				used[i] = true
				break
			}
		}
	}
	for i, f := range o {
		if !used[i] {
			out = append(out, f)
		}
	}
	return out
}

// MarshalJSON writes the object compactly, keys in order, escaping HTML
// characters like encoding/json.
func (o Obj) MarshalJSON() ([]byte, error) { return o.AppendJSON(nil) }

// AppendJSON appends the compact JSON of o to dst, escaping HTML
// characters like encoding/json.
func (o Obj) AppendJSON(dst []byte) ([]byte, error) { return encoder{html: true}.obj(dst, o) }

// Format is the byte layout of a whole body: Indent "" is compact; Newline
// appends "\n" after the closing brace; RawHTML leaves <, > and & as they
// are (OpenAI: "Expected a value <= 2") instead of encoding/json's <.
type Format struct {
	Indent  string
	Newline bool
	RawHTML bool
	// Prefix is written before the JSON (OpenRouter's keep-alive
	// whitespace before a non-streaming body).
	Prefix string
}

// Marshal renders v (usually an Obj) in the format f.
func (f Format) Marshal(v any) ([]byte, error) { return f.Append(nil, v) }

// Append appends v rendered in the format f to dst.
func (f Format) Append(dst []byte, v any) ([]byte, error) {
	dst = append(dst, f.Prefix...)
	if f.Indent == "" {
		b, err := encoder{html: !f.RawHTML}.value(dst, v)
		if err == nil && f.Newline {
			b = append(b, '\n')
		}
		return b, err
	}
	b, err := encoder{html: !f.RawHTML}.value(nil, v)
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(dst)
	buf.Grow(len(b) * 2)
	_ = json.Indent(buf, b, "", f.Indent) // b is valid JSON: the encoder built it
	if f.Newline {
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// encoder writes JSON values; html escapes <, > and & as <… .
type encoder struct{ html bool }

func (e encoder) obj(dst []byte, o Obj) ([]byte, error) {
	dst = append(dst, '{')
	var err error
	for i, f := range o {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = e.str(dst, f.Key)
		dst = append(dst, ':')
		if dst, err = e.value(dst, f.Val); err != nil {
			return nil, err
		}
	}
	return append(dst, '}'), nil
}

func (e encoder) value(dst []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(dst, "null"...), nil
	case string:
		return e.str(dst, x), nil
	case bool:
		return strconv.AppendBool(dst, x), nil
	case int:
		return strconv.AppendInt(dst, int64(x), 10), nil
	case int64:
		return strconv.AppendInt(dst, x, 10), nil
	case Obj:
		return e.obj(dst, x)
	case []Obj:
		dst = append(dst, '[')
		var err error
		for i, o := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			if dst, err = e.obj(dst, o); err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case []any:
		dst = append(dst, '[')
		var err error
		for i, el := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			if dst, err = e.value(dst, el); err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(e.html)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return append(dst, bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})...), nil
}

const hexDigits = "0123456789abcdef"

// str quotes s exactly like encoding/json (control characters, U+2028 and
// U+2029 escaped, invalid UTF-8 replaced by U+FFFD, HTML characters when
// e.html), without the reflection and allocation of json.Marshal.
func (e encoder) str(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' && (!e.html || c != '<' && c != '>' && c != '&') {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, `�`...)
			i += size
			start = i
			continue
		}
		if r == ' ' || r == ' ' {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}
