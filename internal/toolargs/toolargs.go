// Package toolargs synthesises JSON arguments that satisfy a tool's JSON
// Schema: required fields always present, optional ones by probability,
// enums respected, numeric ranges and array lengths honoured, nested objects
// and $ref resolved. It also builds deliberately broken arguments for the
// tool-misbehaviour catalogue.
package toolargs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// Options tune generation.
type Options struct {
	OptionalProbability      float64 // 0..100
	MinInt, MaxInt           int64
	MinNumber, MaxNumber     float64
	MinArrayLen, MaxArrayLen int
	MaxDepth                 int
}

// DefaultOptions mirror llm-d-inference-sim's defaults.
func DefaultOptions() Options {
	return Options{OptionalProbability: 50, MinInt: 0, MaxInt: 100, MinNumber: 0, MaxNumber: 100, MinArrayLen: 1, MaxArrayLen: 5, MaxDepth: 8}
}

// Schema is a parsed JSON Schema subset.
type Schema struct {
	Type                 []string
	Properties           map[string]*Schema
	Required             []string
	Items                *Schema
	Enum                 []json.RawMessage
	Const                json.RawMessage
	Minimum              *float64
	Maximum              *float64
	ExclusiveMin         *float64
	ExclusiveMax         *float64
	MultipleOf           *float64
	MinLength            *int
	MaxLength            *int
	MinItems             *int
	MaxItems             *int
	Format               string
	Pattern              string
	OneOf                []*Schema
	AnyOf                []*Schema
	AllOf                []*Schema
	Ref                  string
	Default              json.RawMessage
	Description          string
	AdditionalProperties *bool
	defs                 map[string]*Schema
	root                 *Schema
	unsupported          []string
}

type rawSchema struct {
	Type                 any                   `json:"type"`
	Properties           map[string]*rawSchema `json:"properties"`
	Required             []string              `json:"required"`
	Items                *rawSchema            `json:"items"`
	Enum                 []json.RawMessage     `json:"enum"`
	Const                json.RawMessage       `json:"const"`
	Minimum              *float64              `json:"minimum"`
	Maximum              *float64              `json:"maximum"`
	ExclusiveMin         *float64              `json:"exclusiveMinimum"`
	ExclusiveMax         *float64              `json:"exclusiveMaximum"`
	MultipleOf           *float64              `json:"multipleOf"`
	MinLength            *int                  `json:"minLength"`
	MaxLength            *int                  `json:"maxLength"`
	MinItems             *int                  `json:"minItems"`
	MaxItems             *int                  `json:"maxItems"`
	Format               string                `json:"format"`
	Pattern              string                `json:"pattern"`
	OneOf                []*rawSchema          `json:"oneOf"`
	AnyOf                []*rawSchema          `json:"anyOf"`
	AllOf                []*rawSchema          `json:"allOf"`
	Ref                  string                `json:"$ref"`
	Default              json.RawMessage       `json:"default"`
	Description          string                `json:"description"`
	Defs                 map[string]*rawSchema `json:"$defs"`
	Definitions          map[string]*rawSchema `json:"definitions"`
	AdditionalProperties json.RawMessage       `json:"additionalProperties"`
}

var cache sync.Map // hash -> *Schema

// Parse compiles a JSON Schema document; results are cached by content hash.
func Parse(raw []byte) (*Schema, error) {
	h := fnv.New64a()
	_, _ = h.Write(raw) // fnv: Write never fails
	key := h.Sum64()
	if v, ok := cache.Load(key); ok {
		return v.(*Schema), nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		s := &Schema{Type: []string{"object"}, Properties: map[string]*Schema{}}
		s.root = s
		cache.Store(key, s)
		return s, nil
	}
	var rs rawSchema
	if err := json.Unmarshal(raw, &rs); err != nil {
		return nil, fmt.Errorf("tool schema: %w", err)
	}
	s := convert(&rs)
	s.defs = map[string]*Schema{}
	for name, d := range rs.Defs {
		s.defs[name] = convert(d)
	}
	for name, d := range rs.Definitions {
		s.defs[name] = convert(d)
	}
	s.root = s
	setRoot(s, s)
	cache.Store(key, s)
	return s, nil
}

func setRoot(s, root *Schema) {
	if s == nil {
		return
	}
	s.root = root
	for _, p := range s.Properties {
		setRoot(p, root)
	}
	setRoot(s.Items, root)
	for _, o := range s.OneOf {
		setRoot(o, root)
	}
	for _, o := range s.AnyOf {
		setRoot(o, root)
	}
	for _, o := range s.AllOf {
		setRoot(o, root)
	}
	for _, d := range root.defs {
		if d != s && d.root == nil {
			setRoot(d, root)
		}
	}
}

func convert(r *rawSchema) *Schema {
	if r == nil {
		return nil
	}
	s := &Schema{
		Required: r.Required, Enum: r.Enum, Const: r.Const, Minimum: r.Minimum, Maximum: r.Maximum,
		ExclusiveMin: r.ExclusiveMin, ExclusiveMax: r.ExclusiveMax, MultipleOf: r.MultipleOf,
		MinLength: r.MinLength, MaxLength: r.MaxLength, MinItems: r.MinItems, MaxItems: r.MaxItems,
		Format: r.Format, Pattern: r.Pattern, Ref: r.Ref, Default: r.Default, Description: r.Description,
	}
	switch t := r.Type.(type) {
	case string:
		s.Type = []string{t}
	case []any:
		for _, x := range t {
			if str, ok := x.(string); ok {
				s.Type = append(s.Type, str)
			}
		}
	}
	if len(r.Properties) > 0 {
		s.Properties = make(map[string]*Schema, len(r.Properties))
		for k, v := range r.Properties {
			s.Properties[k] = convert(v)
		}
	}
	s.Items = convert(r.Items)
	for _, o := range r.OneOf {
		s.OneOf = append(s.OneOf, convert(o))
	}
	for _, o := range r.AnyOf {
		s.AnyOf = append(s.AnyOf, convert(o))
	}
	for _, o := range r.AllOf {
		s.AllOf = append(s.AllOf, convert(o))
	}
	if len(r.AdditionalProperties) > 0 {
		var b bool
		if json.Unmarshal(r.AdditionalProperties, &b) == nil {
			s.AdditionalProperties = &b
		}
	}
	if len(s.Type) == 0 {
		switch {
		case s.Properties != nil:
			s.Type = []string{"object"}
		case s.Items != nil:
			s.Type = []string{"array"}
		case len(s.Enum) > 0 || len(s.Const) > 0:
			// type inferred from enum values at generation time
		}
	}
	return s
}

// Generate produces a JSON document satisfying the schema.
func Generate(s *Schema, r *generate.RNG, opt Options) ([]byte, error) {
	if opt.MaxDepth == 0 {
		opt.MaxDepth = 8
	}
	v := gen(s, r, opt, 0)
	return json.Marshal(v)
}

// GenerateMap is Generate returning a map for templating.
func GenerateMap(s *Schema, r *generate.RNG, opt Options) map[string]any {
	if opt.MaxDepth == 0 {
		opt.MaxDepth = 8
	}
	v := gen(s, r, opt, 0)
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{"value": v}
}

func (s *Schema) resolve() *Schema {
	if s == nil || s.Ref == "" || s.root == nil {
		return s
	}
	name := strings.TrimPrefix(strings.TrimPrefix(s.Ref, "#/$defs/"), "#/definitions/")
	if name == "#" {
		return s.root
	}
	if d, ok := s.root.defs[name]; ok {
		return d
	}
	return s
}

func gen(s *Schema, r *generate.RNG, opt Options, depth int) any {
	s = s.resolve()
	if s == nil || depth > opt.MaxDepth {
		return nil
	}
	if len(s.Const) > 0 {
		var v any
		_ = json.Unmarshal(s.Const, &v)
		return v
	}
	if len(s.Enum) > 0 {
		var v any
		_ = json.Unmarshal(s.Enum[r.IntN(len(s.Enum))], &v)
		return v
	}
	if len(s.OneOf) > 0 {
		return gen(s.OneOf[r.IntN(len(s.OneOf))], r, opt, depth+1)
	}
	if len(s.AnyOf) > 0 {
		return gen(s.AnyOf[r.IntN(len(s.AnyOf))], r, opt, depth+1)
	}
	if len(s.AllOf) > 0 {
		merged := map[string]any{}
		for _, sub := range s.AllOf {
			if m, ok := gen(sub, r, opt, depth+1).(map[string]any); ok {
				for k, v := range m {
					merged[k] = v
				}
			}
		}
		return merged
	}
	typ := ""
	if len(s.Type) > 0 {
		// Prefer a non-null type when several are allowed.
		typ = s.Type[0]
		for _, t := range s.Type {
			if t != "null" {
				typ = t
				break
			}
		}
	}
	switch typ {
	case "object", "":
		out := map[string]any{}
		required := map[string]bool{}
		for _, k := range s.Required {
			required[k] = true
		}
		keys := make([]string, 0, len(s.Properties))
		for k := range s.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if required[k] || r.Chance(opt.OptionalProbability) {
				out[k] = gen(s.Properties[k], r, opt, depth+1)
			}
		}
		if len(s.Properties) == 0 && typ == "" {
			return "value"
		}
		return out
	case "array":
		lo, hi := opt.MinArrayLen, opt.MaxArrayLen
		if s.MinItems != nil {
			lo = *s.MinItems
		}
		if s.MaxItems != nil {
			hi = *s.MaxItems
		}
		if hi < lo {
			hi = lo
		}
		n := r.Between(lo, hi)
		arr := make([]any, 0, n)
		for i := 0; i < n; i++ {
			if s.Items != nil {
				arr = append(arr, gen(s.Items, r, opt, depth+1))
			} else {
				arr = append(arr, "item-"+strconv.Itoa(i+1))
			}
		}
		return arr
	case "string":
		return genString(s, r)
	case "integer":
		lo, hi := float64(opt.MinInt), float64(opt.MaxInt)
		if s.Minimum != nil {
			lo = *s.Minimum
		}
		if s.ExclusiveMin != nil {
			lo = *s.ExclusiveMin + 1
		}
		if s.Maximum != nil {
			hi = *s.Maximum
		}
		if s.ExclusiveMax != nil {
			hi = *s.ExclusiveMax - 1
		}
		if hi < lo {
			hi = lo
		}
		v := int64(lo) + r.Int64N(int64(hi-lo)+1)
		if s.MultipleOf != nil && *s.MultipleOf > 0 {
			m := int64(*s.MultipleOf)
			if m > 0 {
				v = (v / m) * m
				if float64(v) < lo {
					v += m
				}
			}
		}
		return v
	case "number":
		lo, hi := opt.MinNumber, opt.MaxNumber
		if s.Minimum != nil {
			lo = *s.Minimum
		}
		if s.ExclusiveMin != nil {
			lo = *s.ExclusiveMin + 0.01
		}
		if s.Maximum != nil {
			hi = *s.Maximum
		}
		if s.ExclusiveMax != nil {
			hi = *s.ExclusiveMax - 0.01
		}
		if hi < lo {
			hi = lo
		}
		v := lo + r.Float64()*(hi-lo)
		if s.MultipleOf != nil && *s.MultipleOf > 0 {
			v = math.Round(v / *s.MultipleOf) * *s.MultipleOf
		}
		return math.Round(v*100) / 100
	case "boolean":
		return r.IntN(2) == 1
	case "null":
		return nil
	}
	return "value"
}

var words = []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima", "mike", "november", "oscar", "papa", "quebec", "romeo", "sierra", "tango", "uniform", "victor", "whiskey", "xray", "yankee", "zulu"}

func genString(s *Schema, r *generate.RNG) string {
	switch s.Format {
	case "date":
		return fmt.Sprintf("%04d-%02d-%02d", 2020+r.IntN(7), 1+r.IntN(12), 1+r.IntN(28))
	case "date-time":
		return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02dZ", 2020+r.IntN(7), 1+r.IntN(12), 1+r.IntN(28), r.IntN(24), r.IntN(60), r.IntN(60))
	case "time":
		return fmt.Sprintf("%02d:%02d:%02d", r.IntN(24), r.IntN(60), r.IntN(60))
	case "uuid":
		return fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x", r.Uint32(), r.IntN(0xffff), r.IntN(0xfff), 0x8000|r.IntN(0x3fff), r.Int64N(1<<48))
	case "email":
		return words[r.IntN(len(words))] + "." + words[r.IntN(len(words))] + "@example.com"
	case "uri", "url":
		return "https://example.com/" + words[r.IntN(len(words))] + "/" + strconv.Itoa(r.IntN(1000))
	case "ipv4":
		return fmt.Sprintf("10.%d.%d.%d", r.IntN(256), r.IntN(256), 1+r.IntN(254))
	case "hostname":
		return words[r.IntN(len(words))] + ".example.com"
	}
	// Heuristics from the description/pattern for paths and names.
	if strings.Contains(s.Pattern, "/") || strings.Contains(strings.ToLower(s.Description), "path") {
		return "/tmp/" + words[r.IntN(len(words))] + "-" + strconv.Itoa(r.IntN(10000)) + ".txt"
	}
	n := 1 + r.IntN(3)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = words[r.IntN(len(words))]
	}
	out := strings.Join(parts, " ")
	if s.MinLength != nil && len(out) < *s.MinLength {
		for len(out) < *s.MinLength {
			out += " " + words[r.IntN(len(words))]
		}
	}
	if s.MaxLength != nil && len(out) > *s.MaxLength {
		out = out[:*s.MaxLength]
	}
	return out
}

// Misbehaviour kinds (AIMock catalogue).
const (
	MisInvalidJSONTruncated     = "invalid_json_truncated"
	MisInvalidJSONTrailingComma = "invalid_json_trailing_comma"
	MisInvalidJSONSingleQuotes  = "invalid_json_single_quotes"
	MisMissingRequired          = "missing_required"
	MisWrongType                = "wrong_type"
	MisExtraProperty            = "extra_property"
	MisEnumMismatch             = "enum_mismatch"
	MisNotObject                = "not_object"
)

// Spec 017 (I1) names of the invalid-JSON styles; aliases of the
// invalid_json_* kinds above.
const (
	MisTruncated     = "truncated"
	MisTrailingComma = "trailing_comma"
	MisSingleQuotes  = "single_quotes"
)

// AllMisbehaviours lists the argument-level kinds (tool-level kinds such as
// unknown_tool and duplicate_id are handled by the engine).
var AllMisbehaviours = []string{MisInvalidJSONTruncated, MisInvalidJSONTrailingComma, MisInvalidJSONSingleQuotes, MisMissingRequired, MisWrongType, MisExtraProperty, MisEnumMismatch, MisNotObject}

// Break takes valid arguments and returns a broken variant of the given kind.
func Break(s *Schema, valid []byte, kind string, r *generate.RNG) []byte {
	switch kind {
	case MisInvalidJSONTruncated, MisTruncated:
		if len(valid) > 2 {
			return valid[:len(valid)/2]
		}
		return []byte("{")
	case MisInvalidJSONTrailingComma, MisTrailingComma:
		if bytes.HasSuffix(valid, []byte("}")) {
			return append(append([]byte{}, valid[:len(valid)-1]...), ',', '}')
		}
		return append(valid, ',')
	case MisInvalidJSONSingleQuotes, MisSingleQuotes:
		return bytes.ReplaceAll(valid, []byte(`"`), []byte(`'`))
	case MisNotObject:
		return []byte(`"not an object"`)
	}
	var m map[string]any
	if err := json.Unmarshal(valid, &m); err != nil || m == nil {
		return []byte(`{"unexpected": true}`)
	}
	s = s.resolve()
	switch kind {
	case MisMissingRequired:
		if s != nil && len(s.Required) > 0 {
			delete(m, s.Required[r.IntN(len(s.Required))])
		} else {
			for k := range m {
				delete(m, k)
				break
			}
		}
	case MisWrongType:
		for k, v := range m {
			switch v.(type) {
			case string:
				m[k] = 12345
			case float64, int64, int:
				m[k] = "twelve"
			case bool:
				m[k] = "yes"
			default:
				m[k] = "wrong"
			}
			break
		}
	case MisExtraProperty:
		m["unexpected_"+words[r.IntN(len(words))]] = r.IntN(1000)
	case MisEnumMismatch:
		done := false
		if s != nil {
			for k, p := range s.Properties {
				if len(p.resolve().Enum) > 0 {
					m[k] = "not-in-enum"
					done = true
					break
				}
			}
		}
		if !done {
			for k := range m {
				m[k] = "not-in-enum"
				break
			}
		}
	}
	out, _ := json.Marshal(m)
	return out
}
