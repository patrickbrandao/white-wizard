package tools

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func names(ts []Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

func TestToolsByProfile(t *testing.T) {
	cases := map[Profile][]string{
		ProfileCoder:    {"edit_file", "list_dir", "read_file", "run_command", "search_code", "write_file"},
		ProfilePesquisa: {"fetch_url", "read_file", "web_search"},
		ProfileChat:     {},
		ProfileAll:      {"edit_file", "fetch_url", "list_dir", "read_file", "run_command", "search_code", "web_search", "write_file"},
		"":              {"edit_file", "fetch_url", "list_dir", "read_file", "run_command", "search_code", "web_search", "write_file"},
	}
	for p, want := range cases {
		ts, err := Tools(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if got := names(ts); !slices.Equal(got, want) {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
	if _, err := Tools("nope"); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("want ErrInvalidProfile, got %v", err)
	}
}

func TestSchemasStrictObjects(t *testing.T) {
	for _, tool := range catalog {
		var s struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
			Additional *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tool.Schema, &s); err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		if s.Type != "object" || s.Additional == nil || *s.Additional {
			t.Errorf("%s: want type object with additionalProperties false", tool.Name)
		}
		for _, r := range s.Required {
			if _, ok := s.Properties[r]; !ok {
				t.Errorf("%s: required %q not in properties", tool.Name, r)
			}
		}
		if tool.Description == "" {
			t.Errorf("%s: empty description", tool.Name)
		}
	}
}

func TestCatalogFormats(t *testing.T) {
	raw, err := Catalog(ProfileCoder, FormatOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	var oa []struct {
		Type     string `json:"type"`
		Function struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &oa); err != nil || len(oa) != 6 || oa[0].Type != "function" || oa[0].Function.Name != "edit_file" || len(oa[0].Function.Parameters) == 0 {
		t.Fatalf("openai: %v %s", err, raw)
	}

	raw, err = Catalog(ProfilePesquisa, FormatAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	var an []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &an); err != nil || len(an) != 3 {
		t.Fatalf("anthropic: %v %s", err, raw)
	}
	for _, k := range []string{"name", "description", "input_schema"} {
		if _, ok := an[0][k]; !ok {
			t.Errorf("anthropic: missing %s", k)
		}
	}

	raw, err = Catalog("", "")
	if err != nil {
		t.Fatal(err)
	}
	var rw []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rw); err != nil || len(rw) != 8 {
		t.Fatalf("raw: %v %s", err, raw)
	}
	for _, k := range []string{"name", "description", "schema", "profiles"} {
		if _, ok := rw[0][k]; !ok {
			t.Errorf("raw: missing %s", k)
		}
	}

	raw, err = Catalog(ProfileChat, FormatOpenAI)
	if err != nil || string(raw) != "[]" {
		t.Fatalf("chat: %v %s", err, raw)
	}

	if _, err := Catalog("x", FormatRaw); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("want ErrInvalidProfile, got %v", err)
	}
	if _, err := Catalog(ProfileAll, "yaml"); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("want ErrInvalidFormat, got %v", err)
	}
}

func TestParseOutcome(t *testing.T) {
	for _, s := range []string{"", "success", "error", "timeout", "invalid_payload"} {
		if o, err := ParseOutcome(s); err != nil || string(o) != s {
			t.Errorf("%q: %v %v", s, o, err)
		}
	}
	for _, s := range []string{"natural_error", "boom"} {
		if _, err := ParseOutcome(s); !errors.Is(err, ErrInvalidOutcome) {
			t.Errorf("%q: want ErrInvalidOutcome, got %v", s, err)
		}
	}
	if f, err := ParseFormat(""); err != nil || f != FormatRaw {
		t.Errorf("format default: %v %v", f, err)
	}
	if p, err := ParseProfile(""); err != nil || p != ProfileAll {
		t.Errorf("profile default: %v %v", p, err)
	}
}
