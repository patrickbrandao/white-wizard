package claudecode

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestMarshalString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", `"plain"`},
		{`<a href="x">&amp;</a> / \`, `"<a href=\"x\">&amp;</a> / \\"`},
		{"\b\t\n\f\r", `"\b\t\n\f\r"`},
		{"\x00\x01\x1f\x7f", `"\u0000\u0001\u001f` + "\x7f" + `"`},
		{"  ", "\"  \""},
		{"ação 日本 \U0001F600", "\"ação 日本 \U0001F600\""},
		// Lone surrogates kept as WTF-8 come back as escapes.
		{"a\xed\xa0\x80b\xed\xbf\xbf", `"a\ud800b\udfff"`},
		// Bytes that are not UTF-8 become U+FFFD.
		{"a\xffb\xed\x9f", "\"a�b��\""},
	}
	for _, c := range cases {
		if got := string(Marshal(String(c.in))); got != c.want {
			t.Errorf("Marshal(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestMarshalNumber(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{math.Copysign(0, -1), "0"},
		{1, "1"},
		{-1, "-1"},
		{128000, "128000"},
		{0.1, "0.1"},
		{-0.5, "-0.5"},
		{123.456, "123.456"},
		{1e20, "100000000000000000000"},
		{1e21, "1e+21"},
		{1.5e21, "1.5e+21"},
		{0.000001, "0.000001"},
		{0.0000015, "0.0000015"},
		{1e-7, "1e-7"},
		{1.5e-7, "1.5e-7"},
		{5e-324, "5e-324"},
		{math.MaxFloat64, "1.7976931348623157e+308"},
		{math.NaN(), "null"},
		{math.Inf(1), "null"},
		{math.Inf(-1), "null"},
	}
	for _, c := range cases {
		if got := string(Marshal(Number(c.in))); got != c.want {
			t.Errorf("Marshal(%v) = %s, want %s", c.in, got, c.want)
		}
	}
	if got := string(Marshal(Int(-42))); got != "-42" {
		t.Errorf("Int(-42) = %s", got)
	}
}

func TestMarshalStructures(t *testing.T) {
	v := Object(
		M("z", Int(1)),
		M("a", Array(Bool(true), Bool(false), Null(), String("x"))),
		M("empty", Object()),
		M("list", Array()),
	)
	want := `{"z":1,"a":[true,false,null,"x"],"empty":{},"list":[]}`
	if got := string(Marshal(v)); got != want {
		t.Fatalf("Marshal = %s, want %s", got, want)
	}
	if v.Kind() != KindObject || len(v.Members()) != 4 || strings.Join(v.Keys(), ",") != "z,a,empty,list" {
		t.Fatalf("accessors: %v %v", v.Kind(), v.Keys())
	}
	a, ok := v.Get("a")
	if !ok || a.Kind() != KindArray || len(a.Elems()) != 4 || !a.Elems()[0].Truth() || a.Elems()[3].Str() != "x" {
		t.Fatalf("Get(a) = %+v %v", a, ok)
	}
	if z, _ := v.Get("z"); z.Num() != 1 {
		t.Fatalf("Num = %v", z.Num())
	}
	if _, ok := v.Get("missing"); ok {
		t.Fatal("Get(missing) found")
	}
	if Null().Kind() != KindNull || String("").Kind() != KindString || Number(1).Kind() != KindNumber || Bool(true).Kind() != KindBool {
		t.Fatal("kinds")
	}
}

func TestParseRoundTrip(t *testing.T) {
	// Everything JSON.stringify produces comes back byte for byte.
	in := []string{
		`{"model":"m","messages":[{"role":"user","content":"a\nb <x> & \"q\" \\ é ` + " " + `"}],"n":[0,-1,0.1,1e+21,1e-7,128000],"t":true,"f":false,"z":null}`,
		`"\u0000\u001f\b\t\n\f\r"`,
		`"\ud800"`,
		`"\udc00x"`,
		`[]`,
		`{}`,
		`[[[{}]]]`,
	}
	for _, s := range in {
		v, err := Parse([]byte(s))
		if err != nil {
			t.Fatalf("Parse(%s): %v", s, err)
		}
		if got := string(Marshal(v)); got != s {
			t.Errorf("round trip of %s = %s", s, got)
		}
	}
}

func TestParseNormalizes(t *testing.T) {
	// Accepted by JSON.parse, but JSON.stringify writes them differently.
	cases := map[string]string{
		" { \"a\" : [ 1 , 2 ] } ":      `{"a":[1,2]}`,
		`"\/\u0041\u00e9\uD83D\uDE00"`: "\"/Aé\U0001F600\"",
		`"\u001F"`:                     `"\u001f"`,
		`"\ud800\u0041"`:               `"\ud800A"`,
		`1.0`:                          `1`,
		`1E3`:                          `1000`,
		`-0`:                           `0`,
		`1e400`:                        `null`,
		`1e-400`:                       `0`,
		`2.5e+2`:                       `250`,
	}
	for in, want := range cases {
		v, err := Parse([]byte(in))
		if err != nil {
			t.Fatalf("Parse(%s): %v", in, err)
		}
		if got := string(Marshal(v)); got != want {
			t.Errorf("Parse(%s) → %s, want %s", in, got, want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	bad := []string{
		``, ` `, `{`, `[`, `{"a"`, `{"a":`, `{"a":1`, `{"a":1,`, `{1:2}`, `{"a" 1}`, `[1`, `[1 2]`, `[1,]`,
		`"abc`, "\"a\x01\"", `"\x"`, `"\`, `"\u12"`, `"\u12g4"`, `"\ud800\u12"`, "\"\xff\"",
		`tru`, `nul`, `fals`, `-`, `01x`, `1.`, `1.e5`, `1e`, `1e+`, `+1`, `.5`, `x`,
		`{} {}`, `1 2`, "{\"a\x01\":1}",
	}
	for _, s := range bad {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
	if _, err := Parse([]byte(`{"a":1,"b":2,"a":3}`)); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("duplicate key: %v", err)
	}
	deep := strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2)
	if _, err := Parse([]byte(deep)); err == nil {
		t.Error("deep nesting accepted")
	}
	// Uppercase hex digits are valid input.
	if v, err := Parse([]byte(`"\u00C9\u00e9"`)); err != nil || v.Str() != "Éé" {
		t.Errorf("hex digits: %q %v", v.Str(), err)
	}
}
