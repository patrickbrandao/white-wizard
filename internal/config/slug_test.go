package config

import (
	"strings"
	"testing"
)

func testSlug(id string) string {
	if strings.Contains(id, "/") {
		return id
	}
	return "white-wizard/" + id
}

// TestFindModelBySlug pins OpenRouter's lookup as checked against the real
// API (task 163).
func TestFindModelBySlug(t *testing.T) {
	c := &Config{Models: []ModelConfig{
		{ID: "wwz-fast", Aliases: []string{"fast-alias"}},
		{ID: "openai/gpt-4o-mini"},
		{ID: "only-openai", Protocols: []string{"openai"}, Drivers: []string{"openai"}},
		{ID: "other/gpt-4o-mini"}, // same name after the author: the first wins
		{ID: "b/wwz-fast"},        // full slug of its own
	}}
	for name, want := range map[string][2]string{
		"wwz-fast":                {"wwz-fast", "wwz-fast"},
		"WWZ-FAST":                {"wwz-fast", "wwz-fast"},
		"white-wizard/wwz-fast":   {"wwz-fast", "wwz-fast"},
		"White-Wizard/Fast-Alias": {"wwz-fast", "fast-alias"},
		"fast-alias":              {"wwz-fast", "fast-alias"},
		"gpt-4o-mini":             {"openai/gpt-4o-mini", "openai/gpt-4o-mini"},
		"OpenAI/GPT-4o-mini":      {"openai/gpt-4o-mini", "openai/gpt-4o-mini"},
		"other/gpt-4o-mini":       {"other/gpt-4o-mini", "other/gpt-4o-mini"},
		"b/wwz-fast":              {"b/wwz-fast", "b/wwz-fast"},
		"x/gpt-4o-mini":           {},
		"4o-mini":                 {},
		"only-openai":             {},
		"white-wizard/nope":       {},
		"":                        {},
	} {
		m, matched := c.FindModelBySlug(name, "openrouter", testSlug)
		id := ""
		if m != nil {
			id = m.ID
		}
		if id != want[0] || matched != want[1] {
			t.Errorf("FindModelBySlug(%q) = %q, %q; want %q, %q", name, id, matched, want[0], want[1])
		}
	}
}

// TestFindModelVariant pins OpenRouter's variant suffixes as checked
// against the real API (task 186, testdata/real/openrouter/models/).
func TestFindModelVariant(t *testing.T) {
	c := &Config{Models: []ModelConfig{
		{ID: "openai/gpt-4o-mini"},
		{ID: "nvidia/lightning"},
		{ID: "nvidia/lightning:free"},
		{ID: "recursive/flint:free"},
	}}
	type want struct {
		id, matched string
		miss        *VariantMiss
	}
	for name, w := range map[string]want{
		"openai/gpt-4o-mini:nitro":       {"openai/gpt-4o-mini", "openai/gpt-4o-mini", nil},
		"gpt-4o-mini:floor":              {"openai/gpt-4o-mini", "openai/gpt-4o-mini", nil},
		"openai/gpt-4o-mini:NITRO":       {"openai/gpt-4o-mini", "openai/gpt-4o-mini", nil},
		"openai/gpt-4o-mini:wwz":         {"openai/gpt-4o-mini", "openai/gpt-4o-mini", nil},
		"openai/gpt-4o-mini:":            {"openai/gpt-4o-mini", "openai/gpt-4o-mini", nil},
		"openai/gpt-4o-mini:nitro:floor": {"openai/gpt-4o-mini", "openai/gpt-4o-mini", nil},
		"openai/gpt-4o-mini:free":        {"", "", &VariantMiss{Name: "openai/gpt-4o-mini", Free: true}},
		"OpenAI/GPT-4o-mini:free":        {"", "", &VariantMiss{Name: "OpenAI/GPT-4o-mini", Free: true}},
		"gpt-4o-mini:free":               {"", "", &VariantMiss{Name: "gpt-4o-mini", Free: true}},
		"openai/gpt-4o-mini:nitro:free":  {"", "", &VariantMiss{Name: "openai/gpt-4o-mini", Free: true}},
		"openai/gpt-4o-mini:FREE":        {"", "", &VariantMiss{Name: "openai/gpt-4o-mini:FREE"}},
		"nvidia/lightning:free":          {"nvidia/lightning:free", "nvidia/lightning:free", nil},
		"lightning:FREE":                 {"nvidia/lightning:free", "nvidia/lightning:free", nil},
		"nvidia/lightning:free:nitro":    {"nvidia/lightning:free", "nvidia/lightning:free", nil},
		"nvidia/lightning:nitro:free":    {"nvidia/lightning:free", "nvidia/lightning:free", nil},
		"nvidia/lightning:nitro":         {"nvidia/lightning", "nvidia/lightning", nil},
		"recursive/flint":                {"", "", &VariantMiss{Name: "recursive/flint"}},
		"flint":                          {"", "", &VariantMiss{Name: "flint"}},
		"recursive/flint:nitro":          {"", "", &VariantMiss{Name: "recursive/flint:nitro"}},
		"openai/gpt-nonexistent:free":    {},
		"x/gpt-4o-mini:free":             {},
		"openai/gpt-nonexistent:nitro":   {},
		"":                               {},
	} {
		m, matched, miss := c.FindModelVariant(name, "openrouter", testSlug)
		id := ""
		if m != nil {
			id = m.ID
		}
		if id != w.id || matched != w.matched || (miss == nil) != (w.miss == nil) || (miss != nil && *miss != *w.miss) {
			t.Errorf("FindModelVariant(%q) = %q, %q, %+v; want %q, %q, %+v", name, id, matched, miss, w.id, w.matched, w.miss)
		}
	}
}
