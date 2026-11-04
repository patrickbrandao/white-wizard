// Package jsonscan walks well-formed JSON without decoding it, keeping the
// document order of object members: the order some providers report
// validation errors in. The request validators of the drivers read the body
// once with json.Valid and then walk it with these helpers (spec 004).
package jsonscan

import (
	"encoding/json"
	"strconv"
)

// Field is one member of a JSON object, kept in document order.
type Field struct {
	Key string
	Val []byte
}

// Kind is the JSON type of a value.
type Kind int

const (
	Invalid Kind = iota
	Null
	Bool
	Int   // a number written without fraction or exponent
	Float // a number with fraction or exponent
	String
	Array
	Object
)

// KindOf returns the type of a well-formed JSON value.
func KindOf(v []byte) Kind {
	v = TrimSpace(v)
	if len(v) == 0 {
		return Invalid
	}
	switch v[0] {
	case '{':
		return Object
	case '[':
		return Array
	case '"':
		return String
	case 't', 'f':
		return Bool
	case 'n':
		return Null
	}
	for _, c := range v {
		if c == '.' || c == 'e' || c == 'E' {
			return Float
		}
	}
	return Int
}

// IsNumber reports whether k is Int or Float.
func IsNumber(k Kind) bool { return k == Int || k == Float }

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// TrimSpace drops the JSON whitespace around v.
func TrimSpace(v []byte) []byte {
	for len(v) > 0 && isSpace(v[0]) {
		v = v[1:]
	}
	for len(v) > 0 && isSpace(v[len(v)-1]) {
		v = v[:len(v)-1]
	}
	return v
}

// SkipSpace returns the index of the first non-space byte of b at or after i.
func SkipSpace(b []byte, i int) int {
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return i
}

// SkipString returns the index after the string starting at b[i] == '"'.
func SkipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(b)
}

// SkipValue returns the index after the value starting at b[i].
func SkipValue(b []byte, i int) int {
	if i >= len(b) {
		return i
	}
	switch b[i] {
	case '"':
		return SkipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = SkipString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	}
	for i < len(b) && b[i] != ',' && b[i] != '}' && b[i] != ']' && !isSpace(b[i]) {
		i++
	}
	return i
}

// Members splits a well-formed JSON object into its members in document
// order. It reports false when v is not an object.
func Members(v []byte) ([]Field, bool) {
	v = TrimSpace(v)
	if len(v) == 0 || v[0] != '{' {
		return nil, false
	}
	var out []Field
	i := SkipSpace(v, 1)
	for i < len(v) && v[i] == '"' {
		end := SkipString(v, i)
		key := string(v[i+1 : end-1])
		for _, c := range v[i+1 : end-1] {
			if c == '\\' {
				_ = json.Unmarshal(v[i:end], &key)
				break
			}
		}
		i = SkipSpace(v, end) + 1 // ':'
		i = SkipSpace(v, i)
		vend := SkipValue(v, i)
		out = append(out, Field{key, v[i:vend]})
		i = SkipSpace(v, vend)
		if i < len(v) && v[i] == ',' {
			i = SkipSpace(v, i+1)
		}
	}
	return out, true
}

// Elements splits a well-formed JSON array into its elements.
func Elements(v []byte) [][]byte {
	v = TrimSpace(v)
	if len(v) == 0 || v[0] != '[' {
		return nil
	}
	var out [][]byte
	i := SkipSpace(v, 1)
	for i < len(v) && v[i] != ']' {
		end := SkipValue(v, i)
		out = append(out, v[i:end])
		i = SkipSpace(v, end)
		if i < len(v) && v[i] == ',' {
			i = SkipSpace(v, i+1)
		}
	}
	return out
}

// Lookup returns the value of key in fs.
func Lookup(fs []Field, key string) ([]byte, bool) {
	for _, f := range fs {
		if f.Key == key {
			return f.Val, true
		}
	}
	return nil, false
}

// StringValue decodes a JSON string value.
func StringValue(v []byte) (string, bool) {
	if KindOf(v) != String {
		return "", false
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

// NumberValue decodes a JSON number value.
func NumberValue(v []byte) (float64, bool) {
	if !IsNumber(KindOf(v)) {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(TrimSpace(v)), 64)
	return f, err == nil
}

// Present reports whether fs sets key to something other than null.
func Present(fs []Field, key string) ([]byte, bool) {
	v, ok := Lookup(fs, key)
	if !ok || KindOf(v) == Null {
		return nil, false
	}
	return v, true
}

// Quote is s as a JSON string literal.
func Quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Set is a set of names.
func Set(names ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}
