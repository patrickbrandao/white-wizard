package server

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// candidate is one driver route mounted at a full path.
type candidate struct {
	drv    driver.Driver
	kind   driver.Kind
	prefix string // mount prefix, "" at the root
	mount  int    // index of the mount in drivers[]
}

// wildRoute is a route ending in "/*" (e.g. /v1/models/{id}).
type wildRoute struct {
	method string
	base   string // full path up to and including the trailing "/"
	cands  []candidate
}

// router is the immutable route table built from drivers[] of one config.
type router struct {
	exact map[string][]candidate // "METHOD /full/path"
	wild  []wildRoute            // longest base first
	paths map[string]struct{}    // every full path (exact and wildcard base), for 405
	hints map[string]string      // api key -> driver name (keys[].driver)
	// byPath lists the exact routes of each full path, in declaration
	// order, and mounts every mounted driver: the route misses (task 235).
	byPath map[string][]pathRoute
	mounts []candidate
}

// pathRoute is one exact route of a path.
type pathRoute struct {
	method string
	cand   candidate
}

// match is the outcome of routing one request.
type match struct {
	drv    driver.Driver
	kind   driver.Kind
	path   string // path relative to the mount prefix (becomes x-wwz-path)
	param  string // trailing segment of a wildcard route
	prefix string // normalised mount prefix of the matched driver, "" at the root
}

// normPrefix turns "/openai/" into "/openai" and "/" into "".
func normPrefix(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimRight(p, "/")
	return p
}

// buildRouter instantiates every mounted driver and indexes its routes.
func buildRouter(cfg *config.Config) (*router, error) {
	r := &router{
		exact:  map[string][]candidate{},
		paths:  map[string]struct{}{},
		hints:  map[string]string{},
		byPath: map[string][]pathRoute{},
	}
	wild := map[string]*wildRoute{}
	for mi, m := range cfg.Drivers {
		if m.Prefix != "" && !strings.HasPrefix(m.Prefix, "/") {
			return nil, fmt.Errorf("server: driver %q prefix %q must start with /", m.Name, m.Prefix)
		}
		drv, err := driver.New(m.Name, m.Options)
		if err != nil {
			return nil, fmt.Errorf("server: %w", err)
		}
		drv = withValidation(drv, cfg)
		prefix := normPrefix(m.Prefix)
		r.mounts = append(r.mounts, candidate{drv: drv, prefix: prefix, mount: mi})
		for _, rt := range drv.Routes() {
			method := strings.ToUpper(rt.Method)
			c := candidate{drv: drv, kind: rt.Kind, prefix: prefix, mount: mi}
			if base, ok := strings.CutSuffix(rt.Path, "*"); ok {
				full := prefix + base
				k := method + " " + full
				w := wild[k]
				if w == nil {
					w = &wildRoute{method: method, base: full}
					wild[k] = w
				}
				w.cands = append(w.cands, c)
				r.paths[strings.TrimSuffix(full, "/")] = struct{}{}
				continue
			}
			full := prefix + rt.Path
			k := method + " " + full
			r.exact[k] = append(r.exact[k], c)
			r.paths[full] = struct{}{}
			r.byPath[full] = append(r.byPath[full], pathRoute{method: method, cand: c})
		}
	}
	for _, w := range wild {
		r.wild = append(r.wild, *w)
	}
	sort.Slice(r.wild, func(i, j int) bool {
		if len(r.wild[i].base) != len(r.wild[j].base) {
			return len(r.wild[i].base) > len(r.wild[j].base)
		}
		return r.wild[i].method+r.wild[i].base < r.wild[j].method+r.wild[j].base
	})
	for _, k := range cfg.Keys {
		if k.Key != "" && k.Driver != "" {
			r.hints[k.Key] = k.Driver
		}
	}
	return r, nil
}

// lookup finds the route for method and path. The second result reports
// whether the path exists for another method (405 instead of 404).
func (r *router) lookup(method, path string, h driver.Headers) (*match, bool) {
	if cands, ok := r.exact[method+" "+path]; ok {
		c := r.pick(cands, h)
		return &match{drv: c.drv, kind: c.kind, path: strings.TrimPrefix(path, c.prefix), prefix: c.prefix}, false
	}
	for i := range r.wild {
		w := &r.wild[i]
		if w.method != method || !strings.HasPrefix(path, w.base) || len(path) == len(w.base) {
			continue
		}
		c := r.pick(w.cands, h)
		return &match{drv: c.drv, kind: c.kind, path: strings.TrimPrefix(path, c.prefix), param: path[len(w.base):], prefix: c.prefix}, false
	}
	if _, ok := r.paths[path]; ok {
		return nil, true
	}
	for i := range r.wild {
		if strings.HasPrefix(path, r.wild[i].base) && len(path) > len(r.wild[i].base) {
			return nil, true
		}
	}
	return nil, false
}

// miss resolves a request lookup found no route for: the driver that
// answers it and what was missed. A path served with other methods goes
// to the driver that serves it (Allow lists its methods there, Kind is
// its route's kind); an unknown path to the driver mounted at the longest
// prefix that covers it. Shared candidates are told apart as pick does.
// ok is false when no mount covers the path.
func (r *router) miss(method, path string, h driver.Headers) (candidate, driver.RouteMiss, bool) {
	routes := r.byPath[path]
	if len(routes) == 0 {
		for i := range r.wild {
			w := &r.wild[i]
			if strings.HasPrefix(path, w.base) && len(path) > len(w.base) {
				for _, c := range w.cands {
					routes = append(routes, pathRoute{method: w.method, cand: c})
				}
			}
		}
	}
	if len(routes) > 0 {
		cands := make([]candidate, len(routes))
		for i, pr := range routes {
			cands[i] = pr.cand
		}
		c := r.pick(cands, h)
		miss := driver.RouteMiss{Method: method, Path: strings.TrimPrefix(path, c.prefix)}
		for _, pr := range routes {
			if pr.cand.mount != c.mount {
				continue
			}
			// A KindResource route sharing the path (GET on OpenAI's chat
			// listing, task 315) does not set the miss's family.
			if miss.Kind == "" || (miss.Kind == driver.KindResource && pr.cand.kind != driver.KindResource) {
				miss.Kind = pr.cand.kind
			}
			if !slices.Contains(miss.Allow, pr.method) {
				miss.Allow = append(miss.Allow, pr.method)
			}
		}
		return c, miss, true
	}
	var cands []candidate
	best := -1
	for _, c := range r.mounts {
		if c.prefix != "" && path != c.prefix && !strings.HasPrefix(path, c.prefix+"/") {
			continue
		}
		switch {
		case len(c.prefix) > best:
			best, cands = len(c.prefix), []candidate{c}
		case len(c.prefix) == best:
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return candidate{}, driver.RouteMiss{Method: method, Path: path}, false
	}
	c := r.pick(cands, h)
	return c, driver.RouteMiss{Method: method, Path: strings.TrimPrefix(path, c.prefix)}, true
}

// pick resolves a shared path: the single driver whose Sniff accepts the
// headers wins; otherwise keys[].driver of the presented credential breaks
// the tie; otherwise the first mount in config order.
func (r *router) pick(cands []candidate, h driver.Headers) candidate {
	if len(cands) == 1 {
		return cands[0]
	}
	var sniffed []candidate
	for _, c := range cands {
		if c.drv.Sniff(h) {
			sniffed = append(sniffed, c)
		}
	}
	if len(sniffed) == 1 {
		return sniffed[0]
	}
	pool := sniffed
	if len(pool) == 0 {
		pool = cands
	}
	if len(r.hints) > 0 {
		for _, c := range pool {
			key := c.drv.Authenticate(h).Key
			if key == "" {
				continue
			}
			name, ok := r.hints[key]
			if !ok {
				continue
			}
			for _, c2 := range pool {
				if c2.drv.Name() == name {
					return c2
				}
			}
		}
	}
	return pool[0]
}
