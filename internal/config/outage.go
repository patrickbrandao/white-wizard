package config

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-yaml"
)

// GlobalErrors is the root errors block: settings that apply to every
// request whatever its model, key or driver.
type GlobalErrors struct {
	Outages []Outage `yaml:"outages"`
}

// Outage is a scheduled unavailability window (spec 009, errors.outages):
// from every firing of Schedule and for Duration, requests of the target
// fail with Type (no draw of errors.rate; only Rate applies) and carry a
// retry-after up to the end of the window.
type Outage struct {
	Schedule string        `yaml:"schedule"` // six-field cron with seconds
	Duration time.Duration `yaml:"duration"`
	Type     string        `yaml:"type"`     // default overloaded
	Rate     *float64      `yaml:"rate"`     // 0..100; nil = 100
	Timezone string        `yaml:"timezone"` // IANA name; default UTC
	// Drivers restricts the window to requests served by these drivers;
	// empty means every driver.
	Drivers []string `yaml:"drivers"`

	// rt is compiled when the outage is decoded from YAML (or by Compile)
	// and shared by the copies of the profile; an invalid schedule leaves
	// it nil and Validate reports why.
	rt *outageRuntime
}

// DefaultOutageType is the error type of an outage without type.
const DefaultOutageType = "overloaded"

type outageRuntime struct {
	cron *Cron
	memo atomic.Pointer[outageMemo]
}

// outageMemo caches the latest firing for one wall-clock second, so the
// per-request cost is an atomic load while the second lasts.
type outageMemo struct {
	sec   int64
	dur   time.Duration
	fire  time.Time
	found bool
}

// UnmarshalYAML decodes an outage strictly and compiles its schedule.
func (o *Outage) UnmarshalYAML(b []byte) error {
	type plain Outage
	var p plain
	if err := yaml.UnmarshalWithOptions(b, &p, yaml.Strict()); err != nil {
		return err
	}
	*o = Outage(p)
	_ = o.Compile()
	return nil
}

// Compile parses the schedule and time zone. Outages decoded from YAML are
// already compiled; call it on outages built in code before sharing them.
func (o *Outage) Compile() error {
	rt, err := o.compile()
	if err != nil {
		return err
	}
	o.rt = rt
	return nil
}

func (o *Outage) compile() (*outageRuntime, error) {
	loc, err := loadLocation(o.Timezone)
	if err != nil {
		return nil, err
	}
	c, err := ParseCron(o.Schedule, loc)
	if err != nil {
		return nil, err
	}
	return &outageRuntime{cron: c}, nil
}

// locations caches time.LoadLocation, which reads the zone database on
// every call.
var locations sync.Map // name -> *time.Location

func loadLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	if v, ok := locations.Load(name); ok {
		return v.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q", name)
	}
	locations.Store(name, loc)
	return loc, nil
}

// Kind is the error type the outage injects.
func (o *Outage) Kind() string {
	if o.Type == "" {
		return DefaultOutageType
	}
	return o.Type
}

// EffectiveRate is the per-request probability (0..100) inside a window.
func (o *Outage) EffectiveRate() float64 {
	if o.Rate == nil {
		return 100
	}
	return *o.Rate
}

// AppliesTo reports whether the outage targets requests of driver.
func (o *Outage) AppliesTo(driver string) bool {
	return len(o.Drivers) == 0 || slices.Contains(o.Drivers, driver)
}

// Window reports whether now falls inside a window of the outage, i.e.
// some firing t0 <= now has now < t0+Duration, and when that window ends.
func (o *Outage) Window(now time.Time) (time.Time, bool) {
	if o.Duration <= 0 {
		return time.Time{}, false
	}
	rt := o.rt
	if rt == nil {
		var err error
		if rt, err = o.compile(); err != nil {
			return time.Time{}, false
		}
	}
	sec := now.Unix()
	m := rt.memo.Load()
	if m == nil || m.sec != sec || m.dur != o.Duration {
		base := time.Unix(sec, 0)
		// Firings are whole seconds: the latest one <= now is the latest
		// one <= base, and any window still open at now started after
		// base-Duration-1s.
		f, ok := rt.cron.Prev(base, base.Add(-o.Duration-time.Second))
		m = &outageMemo{sec: sec, dur: o.Duration, fire: f, found: ok}
		rt.memo.Store(m)
	}
	if !m.found {
		return time.Time{}, false
	}
	end := m.fire.Add(o.Duration)
	if !now.Before(end) {
		return time.Time{}, false
	}
	return end, true
}

// errorTypeCheck validates outage types; internal/chaos registers it so
// config does not import the list of injectable types.
var errorTypeCheck atomic.Pointer[func(string) bool]

// RegisterErrorTypes sets the function Validate uses to accept the type of
// an outage (nil = any non-empty name).
func RegisterErrorTypes(fn func(string) bool) {
	if fn == nil {
		errorTypeCheck.Store(nil)
		return
	}
	errorTypeCheck.Store(&fn)
}

// validateOutages checks a list of outages; path names it in errors
// (e.g. "errors.outages" or "profiles.errors.flaky.outages").
func (c *Config) validateOutages(path string, outs []Outage) error {
	for i := range outs {
		if err := c.validateOutage(&outs[i]); err != nil {
			return fmt.Errorf("config: %s[%d]: %w", path, i, err)
		}
	}
	return nil
}

func (c *Config) validateOutage(o *Outage) error {
	if o.Schedule == "" {
		return errors.New("schedule is required")
	}
	loc, err := loadLocation(o.Timezone)
	if err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	if _, err := ParseCron(o.Schedule, loc); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	if o.Duration <= 0 {
		return fmt.Errorf("duration must be positive, got %s", o.Duration)
	}
	if r := o.EffectiveRate(); r < 0 || r > 100 || math.IsNaN(r) {
		return fmt.Errorf("rate must be between 0 and 100, got %v", r)
	}
	if fn := errorTypeCheck.Load(); fn != nil && !(*fn)(o.Kind()) {
		return fmt.Errorf("unknown error type %q", o.Kind())
	}
	for _, d := range o.Drivers {
		if !slices.ContainsFunc(c.Drivers, func(m DriverMount) bool { return m.Name == d }) {
			return fmt.Errorf("drivers: unknown driver %q", d)
		}
	}
	return nil
}
