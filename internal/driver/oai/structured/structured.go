// Package structured holds the Structured Outputs rules of the OpenAI API
// (ADR 0029): a function or json_schema with strict: true must be a schema
// whose every object sets additionalProperties to false and lists every
// property in required. The walk order and the texts were conferred against
// the real OpenAI API (tools nested, items, anyOf, second tool,
// response_format; testdata/real/openai/validation/). The request
// validation of the server (openai, Responses API) and the azure driver
// share them (spec 004; not conferred on Azure).
package structured

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Violation walks a JSON Schema depth first (the node, then its
// properties, items, anyOf, $defs and definitions) and returns the message
// tail of the first Structured Outputs violation ("In context=(...), ..."),
// or "" when the schema follows the rules or is not an object.
func Violation(schema []byte) string { return violation(schema, nil) }

func violation(raw []byte, path []string) string {
	fs, ok := members(raw)
	if !ok {
		return ""
	}
	props, _ := members(lookup(fs, "properties"))
	if isObjectSchema(lookup(fs, "type")) {
		ctx := "In context=" + pyTuple(path) + ", "
		if ap := lookup(fs, "additionalProperties"); string(bytes.TrimSpace(ap)) != "false" {
			return ctx + "'additionalProperties' is required to be supplied and to be false."
		}
		have := map[string]bool{}
		for _, r := range elements(lookup(fs, "required")) {
			var s string
			if json.Unmarshal(r, &s) == nil {
				have[s] = true
			}
		}
		for _, p := range props {
			if !have[p.key] {
				return ctx + "'required' is required to be supplied and to be an array including every key in properties. Missing '" + p.key + "'."
			}
		}
	}
	for _, p := range props {
		if msg := violation(p.val, appendPath(path, "properties", p.key)); msg != "" {
			return msg
		}
	}
	if msg := violation(lookup(fs, "items"), appendPath(path, "items")); msg != "" {
		return msg
	}
	for i, s := range elements(lookup(fs, "anyOf")) {
		if msg := violation(s, appendPath(path, "anyOf", strconv.Itoa(i))); msg != "" {
			return msg
		}
	}
	for _, k := range []string{"$defs", "definitions"} {
		defs, _ := members(lookup(fs, k))
		for _, d := range defs {
			if msg := violation(d.val, appendPath(path, k, d.key)); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// member is one key of a JSON object, in document order.
type member struct {
	key string
	val []byte
}

// members decodes a JSON object keeping the key order; false when raw is
// not a well-formed object.
func members(raw []byte) ([]member, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	var out []member
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		k, _ := t.(string)
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			return nil, false
		}
		out = append(out, member{k, v})
	}
	return out, true
}

// elements are the values of a JSON array; nil when raw is not one.
func elements(raw []byte) []json.RawMessage {
	var out []json.RawMessage
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func lookup(ms []member, k string) []byte {
	for _, m := range ms {
		if m.key == k {
			return m.val
		}
	}
	return nil
}

// isObjectSchema reports whether a schema's type is "object" or a type list
// containing it.
func isObjectSchema(raw []byte) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == "object"
	}
	for _, t := range elements(raw) {
		if json.Unmarshal(t, &s) == nil && s == "object" {
			return true
		}
	}
	return false
}

// appendPath returns path plus segs without aliasing path's backing array.
func appendPath(path []string, segs ...string) []string {
	out := make([]string, 0, len(path)+len(segs))
	return append(append(out, path...), segs...)
}

// pyTuple renders path as Python's repr of a tuple of str, the form the
// OpenAI validator prints: (), ('a',), ('a', 'b').
func pyTuple(path []string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = pyRepr(p)
	}
	s := strings.Join(parts, ", ")
	if len(path) == 1 {
		s += ","
	}
	return "(" + s + ")"
}

// pyRepr renders s as Python's repr of a str (printable ASCII): single
// quotes unless s holds a single quote and no double quote.
func pyRepr(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}
