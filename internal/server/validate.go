package server

import (
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// Request validation (spec 004, ADRs 0029 and 0042): always on, every
// driver refuses what its real provider refuses, in the order the provider
// checks, with the real status, envelope and text. The rules live in the
// drivers (driver.RequestValidator); the server only runs them inside
// ParseRequest, so the check sits where the driver's own parse errors sit
// in the engine's order and is counted like them. A driver without a
// validator does not validate (R15: a new driver does not change the core).

// validation is the driver.Models a validator sees: the protocol of its
// driver and the configuration the route table was built from (the
// providers that check the model before the body need the models).
type validation struct {
	cfg      *config.Config
	protocol string
	drv      driver.Driver // the wrapped driver (its ModelNamer resolves slugs)
}

var _ driver.Models = (*validation)(nil)

// KnownModel reports whether name, on a kind route, resolves to a model the
// engine serves: resolved as the engine resolves it (id, alias or the
// driver's slug, optionally followed by a -scenario- suffix), in the
// driver's catalog, speaking the route's dialect and not past retires_at
// (engine.Servable, ADR 0053). A miss on a model variant (OpenRouter
// ":free") counts as known: the engine answers it with its own text.
// Without a route catalog every model of the driver counts; inRoute
// narrows it to the catalog of the route.
func (v *validation) KnownModel(kind driver.Kind, name string) bool {
	known, _ := v.Model(kind, name)
	return known
}

// Model is KnownModel and, for an unknown one, whether it exists but is
// past retires_at (the Responses API names it deprecated).
func (v *validation) Model(kind driver.Kind, name string) (known, retired bool) {
	return v.model(kind, name, "")
}

// model is Model on a route whose catalog is cat (driver.CatalogRouter,
// ADR 0072; "" has every model): a model outside cat is unknown, as the
// engine's model step refuses it (engine.servable), and not retired.
func (v *validation) model(kind driver.Kind, name, cat string) (known, retired bool) {
	if v.cfg == nil {
		return true, false
	}
	m, err := engine.ResolveModel(v.cfg, v.drv, name)
	if err != nil {
		return true, false
	}
	if m != nil && !m.InCatalog(cat) {
		return false, false
	}
	now := time.Now()
	if engine.Servable(m, v.protocol, kind, now) {
		return true, false
	}
	return false, m != nil && m.Retired(now)
}

// inRoute is the driver.Models of a request whose route serves the catalog
// cat: what the engine resolved with driver.RouteCatalog and stored in the
// headers (driver.SetCatalog) before ParseRequest.
type inRoute struct {
	v   *validation
	cat string
}

var _ driver.Models = inRoute{}

func (r inRoute) KnownModel(kind driver.Kind, name string) bool {
	known, _ := r.v.model(kind, name, r.cat)
	return known
}

func (r inRoute) Model(kind driver.Kind, name string) (known, retired bool) {
	return r.v.model(kind, name, r.cat)
}

// modelsFor is the driver.Models the validator of a request with headers h
// sees: v itself without a route catalog (a driver without CatalogRouter,
// unchanged), else v narrowed to that catalog.
func (v *validation) modelsFor(h driver.Headers) driver.Models {
	if cat := driver.CatalogFrom(h); cat != "" {
		return inRoute{v: v, cat: cat}
	}
	return v
}

// slugRouter mirrors the engine's optional interface for OpenRouter's
// single-model route.
type slugRouter interface {
	ModelPathBySlug(path string) bool
}

// withValidation wraps drv with its validator; drivers without one are
// returned unchanged. The wrapper keeps the optional interfaces the engine
// looks for (IDMinter, ModelNamer, slugRouter) in the combinations the
// drivers have; TestWithValidationKeepsInterfaces fails when a driver
// brings a new one.
func withValidation(drv driver.Driver, cfg *config.Config) driver.Driver {
	check, ok := driver.As[driver.RequestValidator](drv)
	if !ok {
		return drv
	}
	v := validatingDriver{Driver: drv, check: check, v: &validation{cfg: cfg, protocol: drv.Protocol(), drv: drv}}
	m, minter := drv.(driver.IDMinter)
	if !minter {
		return v
	}
	vm := validatingMinter{validatingDriver: v, m: m}
	n, named := drv.(driver.ModelNamer)
	if !named {
		return vm
	}
	vn := validatingNamed{validatingMinter: vm, n: n}
	if s, ok := drv.(slugRouter); ok {
		return validatingSlug{validatingNamed: vn, s: s}
	}
	return vn
}

// validatingDriver validates the body before delegating to the driver.
type validatingDriver struct {
	driver.Driver
	check driver.RequestValidator
	v     *validation
}

// Unwrap gives driver.As the wrapped driver (RouteMisser, task 235).
func (d validatingDriver) Unwrap() driver.Driver { return d.Driver }

// Conventions are those of the wrapped driver: its own Conventioner (azure
// writes apim-request-id itself) or the defaults of its name.
func (d validatingDriver) Conventions() driver.Conventions { return driver.ConventionsOf(d.Driver) }

func (d validatingDriver) ParseRequest(kind driver.Kind, body []byte, h driver.Headers) (*core.Request, *core.Error) {
	if err := d.check.ValidateRequest(kind, body, h, d.v.modelsFor(h)); err != nil {
		return nil, err
	}
	return d.Driver.ParseRequest(kind, body, h)
}

// WriteError sends a raw reply verbatim (the gateways answer validation
// failures with bodies the driver's error envelope does not produce) and
// hands every other error to the driver.
func (d validatingDriver) WriteError(w core.ResponseWriter, req *core.Request, e *core.Error, rl *core.RateLimitState) {
	if r, ok := driver.RawReplyOf(e); ok {
		if r.ContentType != "" {
			w.Header("content-type", r.ContentType)
		}
		w.Status(r.Status)
		_, _ = w.Write(r.Body)
		return
	}
	d.Driver.WriteError(w, req, e, rl)
}

// validatingMinter keeps the driver's IDMinter visible through the wrapper.
type validatingMinter struct {
	validatingDriver
	m driver.IDMinter
}

func (d validatingMinter) RequestID(kind driver.Kind, unique string) string {
	return d.m.RequestID(kind, unique)
}

func (d validatingMinter) ResponseID(kind driver.Kind, rng *generate.RNG, now time.Time) string {
	return d.m.ResponseID(kind, rng, now)
}

func (d validatingMinter) ToolCallID(kind driver.Kind, rng *generate.RNG) string {
	return d.m.ToolCallID(kind, rng)
}

// validatingNamed adds the driver's ModelNamer.
type validatingNamed struct {
	validatingMinter
	n driver.ModelNamer
}

func (d validatingNamed) ModelName(m *config.ModelConfig) string { return d.n.ModelName(m) }

func (d validatingNamed) UnprefixModel(name string) (string, bool) { return d.n.UnprefixModel(name) }

// validatingSlug adds the driver's slugRouter.
type validatingSlug struct {
	validatingNamed
	s slugRouter
}

func (d validatingSlug) ModelPathBySlug(path string) bool { return d.s.ModelPathBySlug(path) }
