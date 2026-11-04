package openrouter_test

import (
	"encoding/json"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

// TestModelPathBySlug: only the singular route demands the full slug (ADR 0040).
func TestModelPathBySlug(t *testing.T) {
	d := &openrouter.Driver{}
	for path, want := range map[string]bool{
		"/api/v1/model/white-wizard/wwz-fast":  true,
		"/api/v1/models/white-wizard/wwz-fast": false,
		"/api/v1/models/wwz-fast":              false,
	} {
		if got := d.ModelPathBySlug(path); got != want {
			t.Errorf("ModelPathBySlug(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestSlug pins ADR 0038: ids without an author get white-wizard/, ids that
// already read author/model are kept.
func TestSlug(t *testing.T) {
	for id, want := range map[string]string{
		"wwz-fast":            "white-wizard/wwz-fast",
		"anthropic/claude-x":  "anthropic/claude-x",
		"white-wizard/legacy": "white-wizard/legacy",
	} {
		if got := openrouter.Slug(id); got != want {
			t.Errorf("Slug(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestUnprefixModel(t *testing.T) {
	d, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	n := d.(driver.ModelNamer)
	if got, ok := n.UnprefixModel("white-wizard/wwz-fast"); !ok || got != "wwz-fast" {
		t.Errorf("UnprefixModel(white-wizard/wwz-fast) = %q, %v", got, ok)
	}
	for _, name := range []string{"wwz-fast", "anthropic/claude-x"} {
		if got, ok := n.UnprefixModel(name); ok {
			t.Errorf("UnprefixModel(%q) = %q, true; want false", name, got)
		}
	}
	if got := n.ModelName(&config.ModelConfig{ID: "anthropic/claude-x"}); got != "anthropic/claude-x" {
		t.Errorf("ModelName(anthropic/claude-x) = %q", got)
	}
}

// TestListedSlug: /models lists the slug as id and canonical_slug.
func TestListedSlug(t *testing.T) {
	d, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := core.NewMemWriter()
	d.ListModels(w, nil, []driver.ModelInfo{{Cfg: &config.ModelConfig{ID: "wwz-fast"}}, {Cfg: &config.ModelConfig{ID: "anthropic/claude-x"}}})
	var body struct {
		Data []struct {
			ID            string `json:"id"`
			CanonicalSlug string `json:"canonical_slug"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body(), &body); err != nil {
		t.Fatal(err)
	}
	want := []string{"white-wizard/wwz-fast", "anthropic/claude-x"}
	if len(body.Data) != len(want) {
		t.Fatalf("data = %+v", body.Data)
	}
	for i, m := range body.Data {
		if m.ID != want[i] || m.CanonicalSlug != want[i] {
			t.Errorf("data.%d = %+v, want %q", i, m, want[i])
		}
	}
}
