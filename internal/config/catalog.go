package config

import (
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
)

// Dialects are the wire dialects of models[].protocols (ADR 0048).
const (
	DialectOpenAI    = "openai"
	DialectAnthropic = "anthropic"
	DialectGemini    = "gemini"
)

// dialects lists the valid models[].protocols values besides "*".
var dialects = []string{DialectOpenAI, DialectAnthropic, DialectGemini}

// Speaks reports whether the model is served in dialect: protocols lists it
// or "*" (an empty list is "*").
func (m *ModelConfig) Speaks(dialect string) bool {
	if len(m.Protocols) == 0 {
		return true
	}
	return slices.Contains(m.Protocols, "*") || slices.Contains(m.Protocols, dialect)
}

// ModelSupports reports whether the model is in the catalog of the driver
// named driverName (listing and acceptance): models[].drivers lists it or,
// without drivers, the driver speaks one of the model's dialects. A driver
// outside the known set (test drivers) has every model.
func (m *ModelConfig) ModelSupports(driverName string) bool {
	if len(m.Drivers) > 0 {
		return slices.Contains(m.Drivers, driverName)
	}
	ds, known := dialectsOf(driverName)
	if !known {
		return true
	}
	for _, d := range ds {
		if m.Speaks(d) {
			return true
		}
	}
	return false
}

// ServedOn reports whether the driver named driverName answers the model on
// a route of dialect: the model is in the driver's catalog and, except on
// drivers that serve every route (OpenRouter, OpenCode), speaks the dialect. On the
// other drivers the wrong dialect is the driver's nonexistent-model error.
func (m *ModelConfig) ServedOn(driverName, dialect string) bool {
	if !m.ModelSupports(driverName) {
		return false
	}
	return servesAllRoutes(driverName) || m.Speaks(dialect)
}

// AddToCatalog puts the model in the catalog of the driver named
// driverName, adding the driver's first dialect to protocols when the model
// speaks none of them (OpenRouter and OpenCode need none). A model already in the
// catalog is left alone.
func (m *ModelConfig) AddToCatalog(driverName string) {
	if m.ModelSupports(driverName) {
		return
	}
	if len(m.Drivers) == 0 { // keep the implicit catalog as it is
		var cur []string
		for _, d := range declaredDrivers() {
			if m.ModelSupports(d) {
				cur = append(cur, d)
			}
		}
		m.Drivers = cur
	}
	m.Drivers = append(m.Drivers, driverName)
	if ds, ok := dialectsOf(driverName); ok && !servesAllRoutes(driverName) && !slices.ContainsFunc(ds, m.Speaks) {
		m.Protocols = append(m.Protocols, ds[0])
	}
}

// validateCatalog checks models[i].protocols and models[i].drivers and
// translates the legacy driver names in protocols (openrouter, opencode):
// the whole list becomes drivers and its dialects (none → openai) become
// protocols, with a warning. Translating twice is a no-op.
func (c *Config) validateCatalog(i int) error {
	m := &c.Models[i]
	legacy := false
	for _, p := range m.Protocols {
		switch {
		case p == "*" || slices.Contains(dialects, p):
		case legacyProtocol(p):
			legacy = true
		default:
			return fmt.Errorf("config: models[%d].protocols: unknown value %q (want openai, anthropic, gemini or *)", i, p)
		}
	}
	if legacy {
		old := slices.Clone(m.Protocols)
		var protos, drvs []string
		star := slices.Contains(old, "*")
		for _, p := range old {
			if slices.Contains(dialects, p) {
				protos = append(protos, p)
			}
			if p != "*" && !slices.Contains(drvs, p) {
				drvs = append(drvs, p)
			}
		}
		switch {
		case star:
			protos, drvs = []string{"*"}, nil // "*" already meant every driver
		case len(protos) == 0:
			protos = []string{DialectOpenAI}
		}
		if len(drvs) > 0 {
			for _, d := range m.Drivers {
				if !slices.Contains(drvs, d) {
					drvs = append(drvs, d)
				}
			}
			m.Drivers = drvs
		}
		m.Protocols = protos
		w := fmt.Sprintf("config: models[%d].protocols: driver names [%s] are deprecated; read as protocols [%s] and drivers [%s] (ADR 0048)",
			i, strings.Join(old, ", "), strings.Join(m.Protocols, ", "), strings.Join(m.Drivers, ", "))
		c.warnings = append(c.warnings, w)
		slog.Warn(w)
	}
	for _, d := range m.Drivers {
		ds, known := dialectsOf(d)
		if !known {
			return fmt.Errorf("config: models[%d].drivers: unknown driver %q", i, d)
		}
		if servesAllRoutes(d) {
			continue
		}
		if !slices.ContainsFunc(ds, m.Speaks) {
			return fmt.Errorf("config: models[%d].drivers: driver %q speaks none of protocols [%s]", i, d, strings.Join(m.Protocols, ", "))
		}
	}
	return nil
}

// CatalogEntry is one models[].catalogs entry (ADR 0072): the model is in
// the catalog, priced by Pricing when set (it replaces the model's whole
// block) and, in a catalog with an allowance, debiting the quota by
// AllowanceFactor × the list cost. ContextWindow, MaxOutput, Capabilities
// and Extra make the model effective in the catalog (ForCatalog; revision
// of task 434).
type CatalogEntry struct {
	Pricing         *Pricing       `yaml:"pricing,omitempty"`
	AllowanceFactor *float64       `yaml:"allowance_factor,omitempty"`
	ContextWindow   *int           `yaml:"context_window,omitempty"`
	MaxOutput       *int           `yaml:"max_output,omitempty"`
	Capabilities    *Capabilities  `yaml:"capabilities,omitempty"`
	Extra           map[string]any `yaml:"extra,omitempty"`
}

// shapesModel reports whether the entry declares anything besides
// allowance_factor, i.e. whether the effective model differs from the
// model.
func (e *CatalogEntry) shapesModel() bool {
	return e.Pricing != nil || e.ContextWindow != nil || e.MaxOutput != nil || e.Capabilities != nil || e.Extra != nil
}

// ForCatalog is the model effective in the catalog name (ADR 0072, item
// 16, revision of task 434): the model itself when name is "", the model
// is not in name's entries or the entry declares nothing besides
// allowance_factor (nothing is copied); otherwise a shallow copy where the
// entry's pricing replaces the whole block, context_window and max_output
// replace the model's, capabilities replace field by field and extra is
// the deep merge of models[].extra with the entry's on top (mergeExtra).
// Catalogs stays, so InCatalog, PricingFor and AllowanceFactor answer as
// on the model, and ForCatalog of the result is an equal model.
func (m *ModelConfig) ForCatalog(name string) *ModelConfig {
	if m == nil || name == "" {
		return m
	}
	e, ok := m.Catalogs[name]
	if !ok || !e.shapesModel() {
		return m
	}
	c := *m
	if e.Pricing != nil {
		c.Pricing = *e.Pricing
	}
	if e.ContextWindow != nil {
		c.ContextWindow = *e.ContextWindow
	}
	if e.MaxOutput != nil {
		c.MaxOutput = *e.MaxOutput
	}
	if k := e.Capabilities; k != nil {
		for _, f := range [...]struct {
			dst **bool
			src *bool
		}{
			{&c.Capabilities.Tools, k.Tools},
			{&c.Capabilities.Reasoning, k.Reasoning},
			{&c.Capabilities.Vision, k.Vision},
			{&c.Capabilities.JSONSchema, k.JSONSchema},
		} {
			if f.src != nil {
				*f.dst = f.src
			}
		}
	}
	if e.Extra != nil {
		c.Extra = mergeExtra(m.Extra, e.Extra)
	}
	return &c
}

// mergeExtra is over merged on top of base, neither changed: objects merge
// key by key, recursively; lists, scalars and null replace. A null stays
// in the result, so the driver also removes the derived field it names.
func mergeExtra(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	maps.Copy(out, base)
	for k, v := range over {
		vm, vIsMap := v.(map[string]any)
		bm, bIsMap := out[k].(map[string]any)
		if vIsMap && bIsMap {
			out[k] = mergeExtra(bm, vm)
			continue
		}
		out[k] = v
	}
	return out
}

// InCatalog reports whether the model is in the catalog name: catalogs
// lists it or is empty (every catalog). "" (no catalog) has every model.
func (m *ModelConfig) InCatalog(name string) bool {
	if name == "" || len(m.Catalogs) == 0 {
		return true
	}
	_, ok := m.Catalogs[name]
	return ok
}

// PricingFor is the model's pricing in the catalog name: the catalog's
// pricing when it declares one, else the model's.
func (m *ModelConfig) PricingFor(catalog string) Pricing {
	if e, ok := m.Catalogs[catalog]; ok && e.Pricing != nil {
		return *e.Pricing
	}
	return m.Pricing
}

// AllowanceFactor is the allowance_factor the model declares in the catalog
// name; ok is false when it declares none (the driver's default applies).
func (m *ModelConfig) AllowanceFactor(catalog string) (float64, bool) {
	e, ok := m.Catalogs[catalog]
	if !ok || e.AllowanceFactor == nil {
		return 0, false
	}
	return *e.AllowanceFactor, true
}

// validateCatalogs checks models[i].catalogs against the catalogs the
// drivers serving the model declare.
func (m *ModelConfig) validateCatalogs(i int) error {
	if len(m.Catalogs) == 0 {
		return nil
	}
	known := map[string]CatalogDecl{}
	for _, d := range declaredDrivers() {
		decl, _ := declOf(d)
		if len(decl.Catalogs) == 0 || !m.ModelSupports(d) {
			continue
		}
		for _, c := range decl.Catalogs {
			known[c.Name] = c
		}
	}
	if len(known) == 0 {
		return fmt.Errorf("config: models[%d].catalogs: no driver serving the model has catalogs", i)
	}
	names := slices.Sorted(maps.Keys(known))
	for _, name := range slices.Sorted(maps.Keys(m.Catalogs)) {
		decl, ok := known[name]
		if !ok {
			return fmt.Errorf("config: models[%d].catalogs: unknown catalog %q (want %s)", i, name, strings.Join(names, ", "))
		}
		e := m.Catalogs[name]
		if f := e.AllowanceFactor; f != nil {
			if !decl.Allowance {
				return fmt.Errorf("config: models[%d].catalogs.%s.allowance_factor: catalog %q has no allowance", i, name, name)
			}
			if !(*f >= 0) || math.IsInf(*f, 0) {
				return fmt.Errorf("config: models[%d].catalogs.%s.allowance_factor must be >= 0 and finite, got %v", i, name, *f)
			}
		}
		if e.Pricing != nil {
			if err := e.Pricing.validate(fmt.Sprintf("models[%d].catalogs.%s.pricing", i, name)); err != nil {
				return err
			}
		}
		for _, f := range [...]struct {
			key string
			v   *int
		}{{"context_window", e.ContextWindow}, {"max_output", e.MaxOutput}} {
			if f.v != nil && *f.v <= 0 {
				return fmt.Errorf("config: models[%d].catalogs.%s.%s must be > 0, got %d", i, name, f.key, *f.v)
			}
		}
		if err := m.checkExtraKeys(fmt.Sprintf("models[%d].catalogs.%s.extra", i, name), e.Extra); err != nil {
			return err
		}
	}
	return nil
}
