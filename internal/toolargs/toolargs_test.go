package toolargs

import (
	"encoding/json"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/generate"
)

const weather = `{
 "type":"object",
 "properties":{
   "location":{"type":"string","description":"City name"},
   "unit":{"type":"string","enum":["celsius","fahrenheit"]},
   "days":{"type":"integer","minimum":1,"maximum":7},
   "details":{"type":"object","properties":{"wind":{"type":"boolean"},"precision":{"type":"number","minimum":0.5,"maximum":2}},"required":["wind"]},
   "tags":{"type":"array","items":{"type":"string"},"minItems":2,"maxItems":3},
   "when":{"type":"string","format":"date"},
   "ref":{"$ref":"#/$defs/coord"}
 },
 "required":["location","unit","days","ref"],
 "$defs":{"coord":{"type":"object","properties":{"lat":{"type":"number"},"lon":{"type":"number"}},"required":["lat","lon"]}}
}`

func TestGenerateRespectsSchema(t *testing.T) {
	s, err := Parse([]byte(weather))
	if err != nil {
		t.Fatal(err)
	}
	for seed := int64(1); seed < 50; seed++ {
		r := generate.NewRNG(seed, "t")
		b, err := Generate(s, r, DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("invalid json: %s", b)
		}
		for _, k := range []string{"location", "unit", "days", "ref"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("missing required %s in %s", k, b)
			}
		}
		if u := m["unit"].(string); u != "celsius" && u != "fahrenheit" {
			t.Fatalf("enum violated: %s", u)
		}
		d := m["days"].(float64)
		if d < 1 || d > 7 {
			t.Fatalf("range violated: %v", d)
		}
		if ref, ok := m["ref"].(map[string]any); !ok || ref["lat"] == nil || ref["lon"] == nil {
			t.Fatalf("$ref not resolved: %s", b)
		}
		if tags, ok := m["tags"].([]any); ok && (len(tags) < 2 || len(tags) > 3) {
			t.Fatalf("array length violated: %d", len(tags))
		}
		if det, ok := m["details"].(map[string]any); ok {
			if _, ok := det["wind"].(bool); !ok {
				t.Fatalf("nested required missing: %s", b)
			}
		}
	}
}

func TestDeterministic(t *testing.T) {
	s, _ := Parse([]byte(weather))
	a, _ := Generate(s, generate.NewRNG(7, "x"), DefaultOptions())
	b, _ := Generate(s, generate.NewRNG(7, "x"), DefaultOptions())
	if string(a) != string(b) {
		t.Fatalf("not deterministic: %s vs %s", a, b)
	}
}

func TestBreak(t *testing.T) {
	s, _ := Parse([]byte(weather))
	r := generate.NewRNG(3, "b")
	valid, _ := Generate(s, r, DefaultOptions())
	for _, k := range AllMisbehaviours {
		out := Break(s, valid, k, r)
		var m map[string]any
		err := json.Unmarshal(out, &m)
		switch k {
		case MisInvalidJSONTruncated, MisInvalidJSONTrailingComma, MisInvalidJSONSingleQuotes, MisNotObject:
			if err == nil {
				t.Fatalf("%s should not be a valid object: %s", k, out)
			}
		case MisMissingRequired:
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, req := range []string{"location", "unit", "days", "ref"} {
				if _, ok := m[req]; ok {
					n++
				}
			}
			if n == 4 {
				t.Fatalf("missing_required did not remove a field: %s", out)
			}
		default:
			if err != nil {
				t.Fatalf("%s: %v", k, err)
			}
		}
	}
}

// TestBreakSpecAliases: the spec 017 names break the JSON exactly like the
// invalid_json_* kinds they alias.
func TestBreakSpecAliases(t *testing.T) {
	s, _ := Parse([]byte(weather))
	valid, _ := Generate(s, generate.NewRNG(3, "b"), DefaultOptions())
	for alias, canon := range map[string]string{MisTruncated: MisInvalidJSONTruncated, MisTrailingComma: MisInvalidJSONTrailingComma, MisSingleQuotes: MisInvalidJSONSingleQuotes} {
		a := Break(s, valid, alias, generate.NewRNG(1, "x"))
		c := Break(s, valid, canon, generate.NewRNG(1, "x"))
		if json.Valid(a) || string(a) != string(c) {
			t.Fatalf("%s: %s, want %s", alias, a, c)
		}
	}
}

func TestEmptySchema(t *testing.T) {
	s, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Generate(s, generate.NewRNG(1, "e"), DefaultOptions())
	if string(b) != "{}" {
		t.Fatalf("expected {} got %s", b)
	}
}
