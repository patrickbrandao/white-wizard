package toolargs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/generate"
)

func mustParse(t *testing.T, doc string) *Schema {
	t.Helper()
	s, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func genAny(t *testing.T, doc string, seed int64, opt Options) any {
	t.Helper()
	b, err := Generate(mustParse(t, doc), generate.NewRNG(seed, "x"), opt)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("invalid JSON %s", b)
	}
	return v
}

func TestParseErrorsAndCache(t *testing.T) {
	if _, err := Parse([]byte(`{not json`)); err == nil || !strings.Contains(err.Error(), "tool schema") {
		t.Fatalf("expected parse error, got %v", err)
	}
	a := mustParse(t, `{"type":"object"}`)
	b := mustParse(t, `{"type":"object"}`)
	if a != b {
		t.Fatal("identical documents must be served from the cache")
	}
	if e := mustParse(t, "  \n "); e.Type[0] != "object" {
		t.Fatalf("blank doc: %+v", e)
	}
}

func TestConvertTypesAndInference(t *testing.T) {
	s := mustParse(t, `{"type":["null","string",7],"additionalProperties":false}`)
	if len(s.Type) != 2 || s.Type[0] != "null" || s.Type[1] != "string" {
		t.Fatalf("type list: %v", s.Type)
	}
	if s.AdditionalProperties == nil || *s.AdditionalProperties {
		t.Fatal("additionalProperties:false must be kept")
	}
	if s := mustParse(t, `{"additionalProperties":{"type":"string"}}`); s.AdditionalProperties != nil {
		t.Fatal("schema-valued additionalProperties is ignored")
	}
	if s := mustParse(t, `{"properties":{"a":{}}}`); s.Type[0] != "object" {
		t.Fatal("properties imply object")
	}
	if s := mustParse(t, `{"items":{"type":"string"}}`); s.Type[0] != "array" {
		t.Fatal("items imply array")
	}
	if s := mustParse(t, `{"enum":["a"]}`); len(s.Type) != 0 {
		t.Fatal("enum alone leaves type empty")
	}
	if s := mustParse(t, `{"const":1}`); len(s.Type) != 0 {
		t.Fatal("const alone leaves type empty")
	}
	if s := mustParse(t, `{"description":"x"}`); len(s.Type) != 0 {
		t.Fatal("nothing to infer")
	}
}

func TestGenerateCompositionAndRefs(t *testing.T) {
	const doc = `{
	  "type":"object",
	  "properties":{
	    "c":{"const":{"k":1}},
	    "e":{"enum":[1,2,3]},
	    "one":{"oneOf":[{"type":"string","enum":["o"]}]},
	    "any":{"anyOf":[{"type":"boolean"},{"type":"null"}]},
	    "all":{"allOf":[{"type":"object","properties":{"a":{"const":1}},"required":["a"]},{"type":"object","properties":{"b":{"const":2}},"required":["b"]},{"type":"string"}]},
	    "def":{"$ref":"#/definitions/D"},
	    "dfs":{"$ref":"#/$defs/E"},
	    "self":{"$ref":"#"},
	    "missing":{"$ref":"#/$defs/nope"},
	    "deep":{"$ref":"#/$defs/E"}
	  },
	  "required":["c","e","one","any","all","def","dfs","missing","deep"],
	  "definitions":{"D":{"type":"string","enum":["d"]}},
	  "$defs":{"E":{"type":"object","properties":{"n":{"$ref":"#/definitions/D"}},"required":["n"]}}
	}`
	for seed := int64(1); seed < 20; seed++ {
		m := genAny(t, doc, seed, DefaultOptions()).(map[string]any)
		if c := m["c"].(map[string]any); c["k"] != float64(1) {
			t.Fatalf("const: %v", m["c"])
		}
		if e := m["e"].(float64); e < 1 || e > 3 {
			t.Fatalf("enum: %v", e)
		}
		if m["one"] != "o" {
			t.Fatalf("oneOf: %v", m["one"])
		}
		if all := m["all"].(map[string]any); all["a"] != float64(1) || all["b"] != float64(2) {
			t.Fatalf("allOf merge: %v", m["all"])
		}
		if m["def"] != "d" {
			t.Fatalf("definitions ref: %v", m["def"])
		}
		if m["dfs"].(map[string]any)["n"] != "d" {
			t.Fatalf("nested ref: %v", m["dfs"])
		}
		if m["missing"] != "value" && m["missing"] != nil {
			// An unresolved $ref stays an untyped schema.
			if _, ok := m["missing"].(string); !ok {
				t.Fatalf("unresolved ref: %v", m["missing"])
			}
		}
	}
}

func TestGenerateRootRef(t *testing.T) {
	const doc = `{"type":"object","properties":{"self":{"$ref":"#"},"v":{"const":1}},"required":["self","v"]}`
	opt := DefaultOptions()
	opt.MaxDepth = 2
	m := genAny(t, doc, 1, opt).(map[string]any)
	inner, ok := m["self"].(map[string]any)
	if !ok || inner["v"] != float64(1) {
		t.Fatalf("$ref \"#\" must resolve to the root schema: %v", m)
	}
}

func TestGenerateDepthLimit(t *testing.T) {
	const doc = `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"object","properties":{"c":{"type":"string"}},"required":["c"]}},"required":["b"]}},"required":["a"]}`
	opt := DefaultOptions()
	opt.MaxDepth = 1
	m := genAny(t, doc, 1, opt).(map[string]any)
	a := m["a"].(map[string]any)
	if a["b"] != nil {
		t.Fatalf("beyond MaxDepth must be null: %v", a)
	}
	// MaxDepth 0 falls back to 8.
	opt.MaxDepth = 0
	m = genAny(t, doc, 1, opt).(map[string]any)
	if m["a"].(map[string]any)["b"].(map[string]any)["c"] == nil {
		t.Fatal("default depth must reach c")
	}
}

func TestGenerateNumbersAndIntegers(t *testing.T) {
	opt := DefaultOptions()
	for seed := int64(1); seed < 40; seed++ {
		i := genAny(t, `{"type":"integer","exclusiveMinimum":10,"exclusiveMaximum":20}`, seed, opt).(float64)
		if i < 11 || i > 19 {
			t.Fatalf("exclusive int: %v", i)
		}
		i = genAny(t, `{"type":"integer","minimum":5,"maximum":50,"multipleOf":10}`, seed, opt).(float64)
		if int(i)%10 != 0 || i < 5 || i > 50 {
			t.Fatalf("multipleOf: %v", i)
		}
		i = genAny(t, `{"type":"integer","minimum":9,"maximum":3}`, seed, opt).(float64)
		if i != 9 {
			t.Fatalf("hi<lo collapses to lo: %v", i)
		}
		i = genAny(t, `{"type":"integer","multipleOf":0.5,"minimum":0,"maximum":4}`, seed, opt).(float64)
		if i < 0 || i > 4 {
			t.Fatalf("fractional multipleOf (m=0 skipped): %v", i)
		}
		n := genAny(t, `{"type":"number","exclusiveMinimum":1,"exclusiveMaximum":2}`, seed, opt).(float64)
		if n <= 1 || n >= 2 {
			t.Fatalf("exclusive number: %v", n)
		}
		n = genAny(t, `{"type":"number","minimum":0,"maximum":10,"multipleOf":2.5}`, seed, opt).(float64)
		if n < 0 || n > 10 || n/2.5 != float64(int(n/2.5)) {
			t.Fatalf("number multipleOf: %v", n)
		}
		n = genAny(t, `{"type":"number","minimum":5,"maximum":1}`, seed, opt).(float64)
		if n != 5 {
			t.Fatalf("number hi<lo: %v", n)
		}
		n = genAny(t, `{"type":"number"}`, seed, opt).(float64)
		if n < 0 || n > 100 {
			t.Fatalf("default number range: %v", n)
		}
	}
	if _, ok := genAny(t, `{"type":"boolean"}`, 1, opt).(bool); !ok {
		t.Fatal("boolean")
	}
	if genAny(t, `{"type":"null"}`, 1, opt) != nil {
		t.Fatal("null")
	}
	if genAny(t, `{"type":["null","integer"],"minimum":4,"maximum":4}`, 1, opt) != float64(4) {
		t.Fatal("non-null type preferred")
	}
	if genAny(t, `{"type":["null"]}`, 1, opt) != nil {
		t.Fatal("only null allowed")
	}
	if genAny(t, `{"type":"weird"}`, 1, opt) != "value" {
		t.Fatal("unknown type yields placeholder")
	}
	if genAny(t, `{"description":"nothing"}`, 1, opt) != "value" {
		t.Fatal("untyped schema without properties yields placeholder")
	}
}

func TestGenerateArrays(t *testing.T) {
	opt := DefaultOptions()
	opt.MinArrayLen, opt.MaxArrayLen = 2, 3
	arr := genAny(t, `{"type":"array"}`, 1, opt).([]any)
	if len(arr) < 2 || len(arr) > 3 || arr[0] != "item-1" || arr[1] != "item-2" {
		t.Fatalf("untyped items: %v", arr)
	}
	arr = genAny(t, `{"type":"array","items":{"type":"integer"},"minItems":4,"maxItems":1}`, 1, opt).([]any)
	if len(arr) != 4 {
		t.Fatalf("maxItems<minItems collapses to minItems: %v", arr)
	}
}

func TestGenerateStrings(t *testing.T) {
	opt := DefaultOptions()
	re := map[string]func(string) bool{
		"date":      func(s string) bool { return len(s) == 10 && s[4] == '-' },
		"date-time": func(s string) bool { return strings.HasSuffix(s, "Z") && s[10] == 'T' },
		"time":      func(s string) bool { return len(s) == 8 && s[2] == ':' },
		"uuid":      func(s string) bool { return len(s) == 36 && s[14] == '4' },
		"email":     func(s string) bool { return strings.HasSuffix(s, "@example.com") },
		"uri":       func(s string) bool { return strings.HasPrefix(s, "https://example.com/") },
		"url":       func(s string) bool { return strings.HasPrefix(s, "https://example.com/") },
		"ipv4":      func(s string) bool { return strings.HasPrefix(s, "10.") && strings.Count(s, ".") == 3 },
		"hostname":  func(s string) bool { return strings.HasSuffix(s, ".example.com") },
	}
	for f, ok := range re {
		for seed := int64(1); seed < 10; seed++ {
			s := genAny(t, `{"type":"string","format":"`+f+`"}`, seed, opt).(string)
			if !ok(s) {
				t.Fatalf("format %s: %q", f, s)
			}
		}
	}
	if s := genAny(t, `{"type":"string","pattern":"^/etc/.*"}`, 1, opt).(string); !strings.HasPrefix(s, "/tmp/") {
		t.Fatalf("pattern path heuristic: %q", s)
	}
	if s := genAny(t, `{"type":"string","description":"File PATH to read"}`, 1, opt).(string); !strings.HasSuffix(s, ".txt") {
		t.Fatalf("description path heuristic: %q", s)
	}
	for seed := int64(1); seed < 10; seed++ {
		if s := genAny(t, `{"type":"string","minLength":60}`, seed, opt).(string); len(s) < 60 {
			t.Fatalf("minLength: %q", s)
		}
		if s := genAny(t, `{"type":"string","minLength":1,"maxLength":3}`, seed, opt).(string); len(s) < 1 || len(s) > 3 {
			t.Fatalf("maxLength: %q", s)
		}
	}
}

func TestOptionalProbability(t *testing.T) {
	const doc = `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}`
	opt := DefaultOptions()
	opt.OptionalProbability = 0
	if m := genAny(t, doc, 1, opt).(map[string]any); len(m) != 0 {
		t.Fatalf("0%%: %v", m)
	}
	opt.OptionalProbability = 100
	if m := genAny(t, doc, 1, opt).(map[string]any); len(m) != 2 {
		t.Fatalf("100%%: %v", m)
	}
}

func TestGenerateMap(t *testing.T) {
	s := mustParse(t, `{"type":"object","properties":{"a":{"const":1}},"required":["a"]}`)
	opt := DefaultOptions()
	opt.MaxDepth = 0
	if m := GenerateMap(s, generate.NewRNG(1, "m"), opt); m["a"] != float64(1) && m["a"] != json.Number("1") && m["a"] != 1.0 {
		t.Fatalf("object map: %v", m)
	}
	str := mustParse(t, `{"type":"string","enum":["only"]}`)
	if m := GenerateMap(str, generate.NewRNG(1, "m"), DefaultOptions()); m["value"] != "only" || len(m) != 1 {
		t.Fatalf("non-object is wrapped: %v", m)
	}
}

func TestBreakVariants(t *testing.T) {
	r := generate.NewRNG(1, "b")
	s := mustParse(t, `{"type":"object","properties":{"mode":{"type":"string","enum":["a","b"]},"n":{"type":"integer"}},"required":["n"]}`)

	// Truncated: short input collapses to "{", longer input is halved.
	if got := Break(s, []byte(`{}`), MisTruncated, r); string(got) != "{" {
		t.Fatalf("short truncate: %q", got)
	}
	if got := Break(s, []byte(`{"n":123456}`), MisTruncated, r); string(got) != `{"n":1` {
		t.Fatalf("truncate: %q", got)
	}
	// Trailing comma: object and non-object endings.
	if got := Break(s, []byte(`{"n":1}`), MisTrailingComma, r); string(got) != `{"n":1,}` {
		t.Fatalf("trailing comma on object: %q", got)
	}
	if got := Break(s, []byte(`[1]`), MisTrailingComma, r); string(got) != `[1],` {
		t.Fatalf("trailing comma on other: %q", got)
	}
	if got := Break(s, []byte(`{"n":"x"}`), MisSingleQuotes, r); string(got) != `{'n':'x'}` {
		t.Fatalf("single quotes: %q", got)
	}
	if got := Break(s, []byte(`{}`), MisNotObject, r); string(got) != `"not an object"` {
		t.Fatalf("not object: %q", got)
	}

	// Structural kinds on input that is not a JSON object.
	for _, bad := range []string{`not json`, `null`} {
		if got := Break(s, []byte(bad), MisWrongType, r); string(got) != `{"unexpected": true}` {
			t.Fatalf("%q: %q", bad, got)
		}
	}

	// missing_required: schema with required drops one; without required drops any key.
	got := Break(s, []byte(`{"n":1,"mode":"a"}`), MisMissingRequired, r)
	if strings.Contains(string(got), `"n"`) {
		t.Fatalf("required key must be removed: %s", got)
	}
	free := mustParse(t, `{"type":"object"}`)
	got = Break(free, []byte(`{"k":1}`), MisMissingRequired, r)
	if string(got) != `{}` {
		t.Fatalf("no required: %s", got)
	}
	if got = Break(nil, []byte(`{"k":1}`), MisMissingRequired, r); string(got) != `{}` {
		t.Fatalf("nil schema: %s", got)
	}

	// wrong_type for every value kind (single-key objects keep it deterministic).
	for in, want := range map[string]string{
		`{"k":"s"}`:     `{"k":12345}`,
		`{"k":1.5}`:     `{"k":"twelve"}`,
		`{"k":true}`:    `{"k":"yes"}`,
		`{"k":[1]}`:     `{"k":"wrong"}`,
		`{"k":null}`:    `{"k":"wrong"}`,
		`{"k":{"a":1}}`: `{"k":"wrong"}`,
	} {
		if got := Break(s, []byte(in), MisWrongType, r); string(got) != want {
			t.Fatalf("wrong_type %s: %s want %s", in, got, want)
		}
	}

	if got := Break(s, []byte(`{"n":1}`), MisExtraProperty, r); !strings.Contains(string(got), `"unexpected_`) {
		t.Fatalf("extra property: %s", got)
	}

	// enum_mismatch: targets the enum property when there is one...
	if got := Break(s, []byte(`{"mode":"a","n":1}`), MisEnumMismatch, r); !strings.Contains(string(got), `"mode":"not-in-enum"`) || !strings.Contains(string(got), `"n":1`) {
		t.Fatalf("enum mismatch: %s", got)
	}
	// ...otherwise (or without schema) overwrites some key.
	if got := Break(free, []byte(`{"k":1}`), MisEnumMismatch, r); string(got) != `{"k":"not-in-enum"}` {
		t.Fatalf("enum mismatch fallback: %s", got)
	}
	if got := Break(nil, []byte(`{"k":1}`), MisEnumMismatch, r); string(got) != `{"k":"not-in-enum"}` {
		t.Fatalf("enum mismatch nil schema: %s", got)
	}

	// Unknown kind returns the input re-encoded.
	if got := Break(s, []byte(`{"n":1}`), "mystery", r); string(got) != `{"n":1}` {
		t.Fatalf("unknown kind: %s", got)
	}
}

func TestSetRootReachesDefsAndComposites(t *testing.T) {
	s := mustParse(t, `{
	  "type":"object",
	  "properties":{"x":{"oneOf":[{"$ref":"#/$defs/A"}]},"y":{"anyOf":[{"$ref":"#/$defs/A"}]},"z":{"allOf":[{"$ref":"#/$defs/A"}]},"w":{"type":"array","items":{"$ref":"#/$defs/A"}}},
	  "required":["x","y","z","w"],
	  "$defs":{"A":{"type":"string","enum":["a"]}}
	}`)
	m := GenerateMap(s, generate.NewRNG(1, "r"), DefaultOptions())
	if m["x"] != "a" || m["y"] != "a" {
		t.Fatalf("refs inside composites must resolve: %v", m)
	}
	if w := m["w"].([]any); w[0] != "a" {
		t.Fatalf("items ref: %v", w)
	}
	if _, ok := m["z"].(map[string]any); !ok {
		t.Fatalf("allOf yields a merged object: %v", m["z"])
	}
}
