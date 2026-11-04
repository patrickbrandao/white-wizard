package drift

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
)

// Ignore is an accepted divergence. Provider, Probe and Path empty or "*"
// match anything; Path ending in "*" matches by prefix; Kind empty matches
// every kind. Reason is mandatory.
type Ignore struct {
	Provider string `yaml:"provider" json:"provider"`
	Probe    string `yaml:"probe" json:"probe"`
	Kind     string `yaml:"kind" json:"kind"`
	Path     string `yaml:"path" json:"path"`
	Reason   string `yaml:"reason" json:"reason"`
}

// defaultIgnores are the divergences accepted by design. It is empty: the
// account headers (openai-organization, openai-project,
// anthropic-organization-id, anthropic-workspace-id) are compared, because
// wwz sends them with generated values (ADR 0049).
var defaultIgnores []Ignore

// DefaultIgnores returns a copy of the built-in ignore list.
func DefaultIgnores() []Ignore { return slices.Clone(defaultIgnores) }

func wildcard(pattern, v string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(v, p)
	}
	return pattern == v
}

// Matches reports whether ig accepts d found by probe on provider.
func (ig Ignore) Matches(provider, probe string, d Divergence) bool {
	return wildcard(ig.Provider, provider) && wildcard(ig.Probe, probe) &&
		(ig.Kind == "" || ig.Kind == d.Kind) && wildcard(ig.Path, d.Path)
}

func (ig Ignore) validate() error {
	switch {
	case strings.TrimSpace(ig.Reason) == "":
		return errors.New("reason é obrigatório")
	case ig.Kind != "" && !slices.Contains(Kinds, ig.Kind):
		return fmt.Errorf("kind %q desconhecido (use %s)", ig.Kind, strings.Join(Kinds, ", "))
	case !wildcard(ig.Provider, "") && !slices.Contains(ProviderNames, ig.Provider):
		return fmt.Errorf("provider %q desconhecido", ig.Provider)
	case !wildcard(ig.Probe, "") && !slices.Contains(ProbeNames, ig.Probe):
		return fmt.Errorf("probe %q desconhecida", ig.Probe)
	}
	return nil
}

// ParseIgnores decodes an ignore list: a YAML sequence of
// {provider, probe, kind, path, reason}.
func ParseIgnores(data []byte) ([]Ignore, error) {
	var list []Ignore
	if err := yaml.UnmarshalWithOptions(data, &list, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("drift: ignore: %w", err)
	}
	for i, ig := range list {
		if err := ig.validate(); err != nil {
			return nil, fmt.Errorf("drift: ignore[%d]: %w", i, err)
		}
	}
	return list, nil
}

// LoadIgnores reads and parses an ignore file.
func LoadIgnores(path string) ([]Ignore, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("drift: ignore: %w", err)
	}
	return ParseIgnores(data)
}

// markIgnored flags the divergences accepted by list and reports whether
// any divergence still counts.
func markIgnored(list []Ignore, provider, probe string, ds []Divergence) bool {
	counts := false
	for i, d := range ds {
		d.Ignored = slices.ContainsFunc(list, func(ig Ignore) bool { return ig.Matches(provider, probe, d) })
		counts = counts || !d.Ignored
		ds[i] = d
	}
	return counts
}
