package structured

import "testing"

const (
	apMsg  = "'additionalProperties' is required to be supplied and to be false."
	reqMsg = "'required' is required to be supplied and to be an array including every key in properties. Missing '"
)

func TestSchemaViolation(t *testing.T) {
	const ok = `"additionalProperties":false`
	cases := []struct{ schema, want string }{
		{`true`, ""},
		{`{"type":"string"}`, ""},
		{`{"type":"object",` + ok + `}`, ""},
		{`{"type":"object","additionalProperties":true}`, "In context=(), " + apMsg},
		{`{"type":"object","additionalProperties":{}}`, "In context=(), " + apMsg},
		{`{"type":["object","null"]}`, "In context=(), " + apMsg},
		{`{"type":["string","null"]}`, ""},
		{`{"type":7}`, ""},
		{`{"type":"object",` + ok + `,"properties":{"a":{},"b":{}},"required":"a"}`, "In context=(), " + reqMsg + "a'."},
		{`{"type":"object",` + ok + `,"properties":{"b":{},"a":{}},"required":["b",1]}`, "In context=(), " + reqMsg + "a'."},
		{`{"type":"object",` + ok + `,"properties":{"x":{"type":"array","items":{"type":"object"}}},"required":["x"]}`, "In context=('properties', 'x', 'items'), " + apMsg},
		{`{"type":"array","items":{"type":"object"}}`, "In context=('items',), " + apMsg},
		{`{"anyOf":[{"type":"string"},{"type":"object",` + ok + `,"properties":{"k":{}}}]}`, "In context=('anyOf', '1'), " + reqMsg + "k'."},
		{`{"anyOf":{}}`, ""},
		{`{"$defs":{"A":{"type":"object",` + ok + `},"B":{"type":"object"}}}`, "In context=('$defs', 'B'), " + apMsg},
		{`{"definitions":{"C":{"type":"object"}}}`, "In context=('definitions', 'C'), " + apMsg},
		{`{"properties":{"it's":{"type":"object"}}}`, `In context=('properties', "it's"), ` + apMsg},
		{`{"properties":7}`, ""},
	}
	for _, c := range cases {
		if got := Violation([]byte(c.schema)); got != c.want {
			t.Errorf("%s:\ngot  %q\nwant %q", c.schema, got, c.want)
		}
	}
}

func TestPyRepr(t *testing.T) {
	cases := map[string]string{
		`a`:       `'a'`,
		`it's`:    `"it's"`,
		`a'b"c`:   `'a\'b"c'`,
		`say "x"`: `'say "x"'`,
		`a\b`:     `'a\\b'`,
	}
	for in, want := range cases {
		if got := pyRepr(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
	if got := pyTuple(nil); got != "()" {
		t.Errorf("empty tuple %s", got)
	}
}

// TestMalformed: a schema that is not well-formed JSON has no violation.
func TestMalformed(t *testing.T) {
	for _, s := range []string{`{"type":"object"`, `{"a" 1}`, `{"a":}`, `{1:2}`} {
		if got := Violation([]byte(s)); got != "" {
			t.Errorf("%s: %q", s, got)
		}
	}
}
