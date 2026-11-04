package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
)

// reasoningFields are the reasoning_field values gateways accept to imitate
// the upstream model they serve (spec 007, ADR 0046).
var reasoningFields = []string{"reasoning_content", "reasoning", "hidden", "inline_tag"}

// ReasoningFields returns the reasoning_field values a generation profile
// accepts; a gateway driver declares them as the values of its
// reasoning_field option.
func ReasoningFields() []string { return slices.Clone(reasoningFields) }

// DriverOption is one option of drivers[].options a driver accepts: the
// value it takes when the mount does not set it and the values allowed
// (spec 005).
type DriverOption struct {
	Name    string
	Default string
	Values  []string
	// Check validates an open value (ADR 0072: free_requests_per_day,
	// console_key) against the whole configuration; with Check, Values may
	// be empty (any value Check accepts). The error says what is wrong.
	Check func(value string, c *Config) error `json:"-"`
}

// OptionPositiveInt is a DriverOption.Check for an integer >= 1.
func OptionPositiveInt(v string, _ *Config) error {
	if n, err := strconv.Atoi(v); err != nil || n < 1 {
		return errors.New("want an integer >= 1")
	}
	return nil
}

// OptionKeyLabel is a DriverOption.Check for the label of a keys[] entry.
func OptionKeyLabel(v string, c *Config) error {
	if c != nil && slices.ContainsFunc(c.Keys, func(k KeyConfig) bool { return k.Label == v }) {
		return nil
	}
	return errors.New("not the label of a keys[] entry")
}

// DriverDecl is what a driver declares to the configuration. The drivers
// publish it through driver.Register (driver.Declarer), so the
// configuration knows no driver by name (R15, spec 005).
type DriverDecl struct {
	// Dialects are the dialects the driver serves models in; the first is
	// the one AddToCatalog adds. A model is in the driver's catalog by
	// default when they share a dialect. A driver that declares none has
	// every model (test drivers).
	Dialects []string
	// AllRoutes: like the real provider, the driver serves every model of
	// its catalog on every route, whatever its protocols (OpenRouter
	// normalizes, ADR 0048; the OpenCode gateway translates, ADR 0062; on
	// it protocols only names the native dialect, which shapes the answer).
	AllRoutes bool
	// LegacyProtocol: the driver name was once accepted in
	// models[].protocols and is translated to models[].drivers with a
	// warning (ADR 0048).
	LegacyProtocol bool
	// ToolMisbehavior: errors.tool_misbehavior.drivers accepts the driver.
	ToolMisbehavior bool
	// DefaultMount is the position (1, 2, ...) of the driver among the
	// drivers mounted when the configuration lists none; 0 leaves it out.
	DefaultMount int
	// Options are the drivers[].options the driver accepts.
	Options []DriverOption
	// ModelExtra are the keys of the driver's /models item a models[].extra
	// may set (spec 003, 006 16.7).
	ModelExtra []string
	// Catalogs are the catalogs a models[].catalogs of a model the driver
	// serves may name (ADR 0072); none = the driver has no catalogs.
	Catalogs []CatalogDecl `json:"Catalogs,omitempty"`
	// Plans are the values tiers.<name>.plan may take (ADR 0072); the
	// union over the registered drivers is accepted.
	Plans []string `json:"Plans,omitempty"`
	// ReservedKeys are keys[].key values the driver reserves while mounted
	// (ADR 0072: the opencode public key).
	ReservedKeys []string `json:"ReservedKeys,omitempty"`
}

// CatalogDecl is one catalog a driver declares.
type CatalogDecl struct {
	Name string
	// Allowance: the catalog debits a quota by an allowance, so its
	// entries may set allowance_factor.
	Allowance bool
}

// driverDecl resolves a registered driver's declaration once; ok is false
// for a driver that declares nothing.
type driverDecl struct {
	once sync.Once
	load func() (DriverDecl, bool)
	decl DriverDecl
	ok   bool
}

func (d *driverDecl) get() (DriverDecl, bool) {
	d.once.Do(func() { d.decl, d.ok = d.load() })
	return d.decl, d.ok
}

// decls is copied on write: the catalog checks run on every request.
var (
	declMu sync.Mutex
	decls  atomic.Pointer[map[string]*driverDecl]
)

// RegisterDriver records the declaration of the driver name; load is
// called the first time the configuration needs it and reports false when
// the driver declares nothing. driver.Register calls it for every driver.
func RegisterDriver(name string, load func() (DriverDecl, bool)) {
	declMu.Lock()
	defer declMu.Unlock()
	next := map[string]*driverDecl{}
	if cur := decls.Load(); cur != nil {
		next = maps.Clone(*cur)
	}
	next[name] = &driverDecl{load: load}
	decls.Store(&next)
}

// declOf returns the declaration of the driver name and whether the driver
// is registered and declares one.
func declOf(name string) (DriverDecl, bool) {
	cur := decls.Load()
	if cur == nil {
		return DriverDecl{}, false
	}
	d, ok := (*cur)[name]
	if !ok {
		return DriverDecl{}, false
	}
	return d.get()
}

// declaredDrivers are the names of the drivers that declare something,
// sorted.
func declaredDrivers() []string {
	cur := decls.Load()
	if cur == nil {
		return nil
	}
	var out []string
	for _, n := range slices.Sorted(maps.Keys(*cur)) {
		if _, ok := declOf(n); ok {
			out = append(out, n)
		}
	}
	return out
}

// dialectsOf are the dialects the driver name serves; known is false when it
// declares none.
func dialectsOf(name string) (ds []string, known bool) {
	decl, _ := declOf(name)
	return decl.Dialects, len(decl.Dialects) > 0
}

// servesAllRoutes reports whether the driver name serves every route.
func servesAllRoutes(name string) bool {
	decl, _ := declOf(name)
	return decl.AllRoutes
}

// legacyProtocol reports whether p is a driver name once accepted in
// models[].protocols.
func legacyProtocol(p string) bool {
	decl, _ := declOf(p)
	return decl.LegacyProtocol
}

// toolMisbehaviorDrivers are the driver names accepted by
// errors.tool_misbehavior.drivers, sorted.
func toolMisbehaviorDrivers() []string {
	var out []string
	for _, n := range declaredDrivers() {
		if d, _ := declOf(n); d.ToolMisbehavior {
			out = append(out, n)
		}
	}
	return out
}

// defaultMounts are the drivers mounted when the configuration lists none,
// in DefaultMount order.
func defaultMounts() []DriverMount {
	var names []string
	for _, n := range declaredDrivers() {
		if d, _ := declOf(n); d.DefaultMount > 0 {
			names = append(names, n)
		}
	}
	slices.SortStableFunc(names, func(a, b string) int {
		da, _ := declOf(a)
		db, _ := declOf(b)
		return da.DefaultMount - db.DefaultMount
	})
	out := make([]DriverMount, len(names))
	for i, n := range names {
		out[i] = DriverMount{Name: n}
	}
	return out
}

// registered reports whether a driver named name registered, declaring
// something or not.
func registered(name string) bool {
	cur := decls.Load()
	if cur == nil {
		return false
	}
	_, ok := (*cur)[name]
	return ok
}

// driverOptionValues maps each option the driver name accepts to its
// allowed values; known is false for a driver that is not registered. A
// registered driver that declares nothing accepts no option (spec 005).
func driverOptionValues(name string) (opts map[string][]string, known bool) {
	decls, known := driverOptionDecls(name)
	if !known {
		return nil, false
	}
	opts = make(map[string][]string, len(decls))
	for n, o := range decls {
		opts[n] = o.Values
	}
	return opts, true
}

// driverOptionDecls maps each option the driver name accepts to its
// declaration; known is false for a driver that is not registered.
func driverOptionDecls(name string) (opts map[string]DriverOption, known bool) {
	if !registered(name) {
		return nil, false
	}
	decl, _ := declOf(name)
	opts = make(map[string]DriverOption, len(decl.Options))
	for _, o := range decl.Options {
		opts[o.Name] = o
	}
	return opts, true
}

// validateDriverOptions rejects an option the driver does not declare and a
// value the driver cannot honour. A name no driver registered is left to
// the driver registry.
func validateDriverOptions(i int, d DriverMount) error {
	allowed, known := driverOptionDecls(d.Name)
	if !known {
		return nil
	}
	keys := make([]string, 0, len(d.Options))
	for k := range d.Options {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		o, ok := allowed[k]
		if !ok {
			return fmt.Errorf("config: drivers[%d] (%s): unknown option %q", i, d.Name, k)
		}
		if o.Check != nil && len(o.Values) == 0 {
			continue // open value: checkDriverOptions
		}
		if !slices.Contains(o.Values, d.Options[k]) {
			return fmt.Errorf("config: drivers[%d] (%s): option %s=%q is invalid (allowed: %v)", i, d.Name, k, d.Options[k], o.Values)
		}
	}
	return nil
}

// checkDriverOptions runs the Check of every option the mount sets, after
// validateDriverOptions accepted the names and closed values.
func (c *Config) checkDriverOptions(i int, d DriverMount) error {
	decls, _ := driverOptionDecls(d.Name)
	for _, k := range slices.Sorted(maps.Keys(d.Options)) {
		o := decls[k]
		if o.Check == nil {
			continue
		}
		if err := o.Check(d.Options[k], c); err != nil {
			return fmt.Errorf("config: drivers[%d] (%s): option %s=%q is invalid: %w", i, d.Name, k, d.Options[k], err)
		}
	}
	return nil
}

// rejectRenamedDriverFields fails on the old drivers[].opts name (ADR 0048),
// without an alias.
func rejectRenamedDriverFields(raw map[string]any) error {
	list, _ := raw["drivers"].([]any)
	for i, it := range list {
		if m, ok := it.(map[string]any); ok {
			if _, has := m["opts"]; has {
				return fmt.Errorf("config: drivers[%d]: field \"opts\" renamed to \"options\"", i)
			}
		}
	}
	return nil
}
